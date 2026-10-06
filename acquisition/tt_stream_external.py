'''
@Version: 1.1
@Date:    18.13.2023
@Autor:   René Heilmann
@Company: Quantum Optics Jena GmbH
@Email:   r.heilmann@qo-jena.com
@License: copyrighted
Small test module for the TimeTagger interface
It will try to connect to TT server held up by "tt_main" and takes the first TimeTagger available to query a permanent read-out of the given four channels
1. setup the "tt_stream_connection_parameters" and "tt_settings" dict in the example
2. run this file
3. Stop with "CTRL C"
'''

# custom multiprocessing.connection auth (see get_custom_multiprocess_communication_module)
import os
import sys
import hmac
import hashlib
import socket
from multiprocessing import connection as mp_connection
from multiprocessing.context import AuthenticationError
# basic utilities
import time
import copy
import logging


class TimeTaggerStream():

    VERSION = '1.1'

    # Adapt TimeTagger Setting here
    TT_SETTINGS_DEFAULT = {
        'tt_serial': 'any',           # specific TT serial, or 'any'
        'channels': [1, 2, 3, 4],     # physical channel numbers as printed on the TT
        'delays_ps': [],              # per-channel delay in ps; empty = leave as-is
        'deadtimes_ps': [],           # per-channel deadtime in ps; empty = leave as-is
        'triggers_volt': [],          # per-channel trigger level in V; empty = leave as-is
        'loop_period_s': 0.5,
    }

    TT_CONNECTION_PARAMS_DEFAULT = {
        'tt_ip': '10.10.10.10',
        'tt_port': 40_401,                # port used for the Customer Interface
        'auth_key_hex': '4444'            # key used for the Customer Interface
    }

    TT_CONNECTION_DEFAULT = None

    def __init__(self, *args) -> None:
        '''Loads the default settings'''
        self.tt_settings = copy.deepcopy(self.TT_SETTINGS_DEFAULT)
        self.tt_connection_params = copy.deepcopy(self.TT_CONNECTION_PARAMS_DEFAULT)
        self.tt_connection = self.TT_CONNECTION_DEFAULT    # Connection obj from the multiprocessing module

        self.stream_connected = False
        self.tt_serial = ''
        self.tt_stream_id = -1

        self.socket_timeout_s = 5.0
        self.hmac_hashlib = hashlib.sha256

        logging.addLevelName(logging.WARNING, 'WARN')
        logging.basicConfig(format='[%(levelname)s] %(message)s', level=logging.DEBUG)

    def get_custom_multiprocess_communication_module(self) -> object:
        '''
        Ensures the same authentication challenge across Python versions on the listener and client side, allows higher socket timeouts, and
        makes the HMAC hash function configurable (multiprocessing.connection hardcodes md5), by redefining "deliver_challenge" and "answer_challenge";
        the hashlib in "self.hmac_hashlib" is used for both; from Python 3.12 on this is already implemented in the original module, but this
        override still works fine there too; ref: https://github.com/python/cpython/blob/3.10/Lib/multiprocessing/connection.py
        '''
        MESSAGE_LENGTH = 20
        CHALLENGE = b'#CHALLENGE#'
        WELCOME = b'#WELCOME#'
        FAILURE = b'#FAILURE#'

        def deliver_challenge_customized(connection, authkey):
            if not isinstance(authkey, bytes):
                raise ValueError(
                    "Authkey must be bytes, not {0!s}".format(type(authkey)))
            message = os.urandom(MESSAGE_LENGTH)
            connection.send_bytes(CHALLENGE + message)
            digest = hmac.new(authkey, message, self.hmac_hashlib).digest()
            response = connection.recv_bytes(256)        # reject large message
            if hmac.compare_digest(response, digest):     # constant-time compare, avoids a timing side channel
                connection.send_bytes(WELCOME)
            else:
                connection.send_bytes(FAILURE)
                raise AuthenticationError('digest received was wrong')

        def answer_challenge_customized(connection, authkey):
            if not isinstance(authkey, bytes):
                raise ValueError(
                    "Authkey must be bytes, not {0!s}".format(type(authkey)))
            message = connection.recv_bytes(256)         # reject large message
            assert message[:len(CHALLENGE)] == CHALLENGE, 'message = %r' % message
            message = message[len(CHALLENGE):]
            digest = hmac.new(authkey, message, self.hmac_hashlib).digest()
            connection.send_bytes(digest)
            response = connection.recv_bytes(256)        # reject large message
            if response != WELCOME:
                raise AuthenticationError('digest sent was rejected')

        def SocketClient_customized(address):
            family = mp_connection.address_type(address)
            with socket.socket(getattr(socket, family)) as s:
                s.settimeout(self.socket_timeout_s)
                s.setblocking(True)
                s.connect(address)
                return mp_connection.Connection(s.detach())

        # overwrite the authentication challenges and socket client
        mp_connection.deliver_challenge = deliver_challenge_customized
        mp_connection.answer_challenge = answer_challenge_customized
        mp_connection.SocketClient = SocketClient_customized

        return mp_connection

    def update_tt_settings(self, new_settings: dict):
        '''Updates the TimeTagger settings; not all keys need to be present'''
        for elem_key in new_settings:
            if elem_key not in self.tt_settings:
                logging.warning(f'update_tt_settings: ignoring unknown setting "{elem_key}"')
                continue

            elem_val = new_settings[elem_key]
            elem_type_class = type(self.tt_settings[elem_key])
            if type(elem_val) != elem_type_class:
                logging.warning(f'update_tt_settings: ignoring "{elem_key}", expected a {elem_type_class} value')
                continue

            self.tt_settings[elem_key] = elem_val

        self.tt_settings['updated'] = True

    def update_tt_connection_params(self, new_connection: dict):
        '''Updates the connection parameters (IP, port, auth key) for the TimeTagger interface'''
        for elem_key in new_connection:
            if elem_key not in self.tt_connection_params:
                logging.warning(f'update_tt_connection_params: ignoring unknown setting "{elem_key}"')
                continue

            elem_val = new_connection[elem_key]
            elem_type_class = type(self.tt_connection_params[elem_key])
            if type(elem_val) != elem_type_class:
                logging.warning(f'update_tt_connection_params: ignoring "{elem_key}", expected a {elem_type_class} value')
                continue

            self.tt_connection_params[elem_key] = elem_val

        self.tt_connection_params['updated'] = True

    def connect(self) -> bool:
        '''Connects to the TimeTagger interface; call "update_tt_connection_params" first'''
        if not self.tt_connection_params.get('updated', False):
            logging.error('connect: call update_tt_connection_params() first, there is no connection target set yet')
            return False

        if self.is_connected():
            logging.warning('TimeTagger interface already connected')
            return True

        tt_address = (self.tt_connection_params['tt_ip'], self.tt_connection_params['tt_port'])
        auth_key_hex = bytes.fromhex(self.tt_connection_params['auth_key_hex'])

        logging.info(f'connecting to TimeTagger interface at {tt_address[0]}:{tt_address[1]}...')

        attempts_left = 3
        while 1:
            # retry: the server port might have just restarted
            try:
                mpc_module = self.get_custom_multiprocess_communication_module()
                self.tt_connection = mpc_module.Client(tt_address, authkey=auth_key_hex)
                logging.info('connection to TimeTagger interface established')
                return True

            except (ConnectionRefusedError, ConnectionResetError, OSError) as exc:
                attempts_left -= 1
                if attempts_left < 1:
                    logging.warning(f'connection refused after 3 attempts ({exc}); check the IP and port are correct and the TimeTagger interface is running')
                    self.disconnect()
                    return False
                time.sleep(0.5)
                logging.info(f'connection refused, retrying ({attempts_left} attempt(s) left)...')

            except AuthenticationError:
                logging.error('connection refused: authentication key does not match the TimeTagger interface')
                self.disconnect()
                return False

            except Exception as exc:
                logging.error(f'connection aborted by an unexpected error: {exc}')
                return False

    def disconnect(self):
        '''Disconnects from the TimeTagger interface'''
        try:
            self.tt_connection.close()
        except Exception:
            pass

        self.tt_connection = self.TT_CONNECTION_DEFAULT
        self.stream_connected = False
        self.tt_serial = ''
        self.tt_stream_id = -1

        logging.info('TimeTagger interface disconnected')

    def is_connected(self):
        '''Whether a connection is currently established'''
        return self.tt_connection is not self.TT_CONNECTION_DEFAULT

    def send_data(self, data_send):
        '''TX implementation; sends data via TCP/IP'''
        if not self.is_connected():
            logging.warning('send_data: cannot send, not connected to the TimeTagger interface')
            return False

        try:
            self.tt_connection.send(data_send)
            return True
        except (ConnectionRefusedError, ConnectionResetError, EOFError, OSError) as exc:
            logging.error(f'send_data: connection lost ({exc})')
            self.disconnect()
            return False

    def recv_data(self):
        '''RX implementation; receives data via TCP/IP'''
        data_recv = None
        if not self.is_connected():
            logging.warning('recv_data: cannot receive, not connected to the TimeTagger interface')
            return data_recv

        try:
            data_recv = self.tt_connection.recv()
            return data_recv
        except (ConnectionRefusedError, ConnectionResetError, EOFError, OSError) as exc:
            logging.error(f'recv_data: connection lost ({exc})')
            self.disconnect()
            return data_recv

    def start_stream(self):
        '''Starts a data stream; returns the resulting connection status'''
        if self.stream_connected:
            logging.warning('start_stream: a data stream is already established, nothing to do')
            return True

        data_send = {
            'act': 'connect',
            'data': self.tt_settings
        }
        self.send_data(data_send)
        data_recv = self.recv_data()

        try:
            self.stream_connected = data_recv['connected']

            if self.stream_connected:
                self.tt_serial = data_recv['tt_serial']
                self.tt_stream_id = data_recv['stream_id']

                logging.info('=== TimeTagger stream established ===')
                logging.info('TT-SERIAL    {}'.format(self.tt_serial))
                logging.info('STREAM-ID    {}'.format(self.tt_stream_id))
                logging.info('CHANNELS     {}'.format(data_recv['channels']))
                logging.info('DELAYS       {}'.format(data_recv['delays_ps']))
                logging.info('DEADTIMES    {}'.format(data_recv['deadtimes_ps']))
                logging.info('TRIGGERS     {}'.format(data_recv['triggers_volt']))
                logging.info('----------------------------------')
                logging.info('Interface Version    {}'.format(data_recv['version_stream_interface']))
                logging.info('TT Manufacturer      {}'.format(data_recv['vendor']))
                logging.info('TT Driver Version    {}'.format(data_recv['version_vendor_software']))
                logging.info('TT Firmware Info     {}'.format(data_recv['device']))
                logging.info('==================================')
            else:
                logging.error('TimeTagger stream failed, no TimeTagger available or all streams are already in use')
        except Exception:
            logging.error('TimeTagger stream failed, no data received')

        return self.stream_connected

    def read_stream(self):
        '''
        Reads the data stream. Returns None on failure, otherwise a dict:
            valid                 bool        whether the result was gathered properly
            error_code             int        potential error code during read-out
            channel_ids_used      list[int]   physical channels in use for this stream
            channel_sequence      list[int]   sequence of channel IDs clicked
            timestamp_sequence    list[int]   timestamps in picoseconds, same length as channel_sequence
            frame_start_ps         int        measurement start time (TimeTagger clock)
            frame_end_ps           int        measurement stop time (TimeTagger clock)
            countrates_cps        list[float] count rate per channel in use, in counts/s
            unixtime_readout      float       host unixtime at read-out
            exec_time_readout_ms  float       read-out execution time
            stream_uptime_s       float       stream uptime in seconds
            delays_ps             list[int]   delay per channel in use, in ps
            deadtimes_ps          list[int]   deadtime per channel in use, in ps
            triggers_volt         list[float] trigger level per channel in use, in V
        '''
        stream_data = None

        if not self.stream_connected:
            return stream_data

        data_send = {
            'act': 'read_stream',
            'data': {
                'tt_serial': self.tt_serial,
                'stream_id': self.tt_stream_id,
            }
        }
        self.send_data(data_send)
        data_recv = self.recv_data()

        try:
            stream_valid = data_recv['valid']
        except Exception:
            return stream_data

        if stream_valid:
            stream_data = data_recv
        else:
            self.disconnect()

        return stream_data


# Example
if __name__ == '__main__':

    # Things you might want to configure
    tt_stream_connection_parameters = {
        'tt_ip': '192.168.101.3',  # adapt to your network settings
        'tt_port': 49_404,
        'auth_key_hex': '4444'
    }

    tt_settings = {
        'tt_serial': 'any',           # specific TT serial, or 'any'
        'channels': [1, 2, 3, 4],     # physical channel numbers as printed on the TT
        'delays_ps': [],              # per-channel delay; empty = leave as-is
        'deadtimes_ps': [],           # per-channel deadtime; empty = leave as-is
        'triggers_volt': [],          # per-channel trigger level; empty = leave as-is
        'loop_period_s': 0.5          # frame size of a single measurement, in seconds [0.1 - 10.0]
    }

    # prepare and start the TimeTagger stream
    tts = TimeTaggerStream()

    tts.update_tt_connection_params(tt_stream_connection_parameters)
    tts.update_tt_settings(tt_settings)
    tts.connect()
    tts.start_stream()

    # read the TimeTagger stream
    try:
        while tts.is_connected():
            stream_data = tts.read_stream()
            if stream_data is not None:
                print('')
                print('UPTIME  {:>10.1f} s'.format(stream_data['stream_uptime_s']))
                print('EVENTS  {:>10.1f} k'.format(1e-3 * len(stream_data['channel_sequence'])))
                print('TT-READ {:>10.1f} ms'.format(stream_data['exec_time_readout_ms']))
                print('CHANNEL SEQUENCE')
                print(stream_data['channel_sequence'])
                print('TIMESTAMP SEQUENCE (ps)')
                print(stream_data['timestamp_sequence'])

            time.sleep(0.1)

    except (KeyboardInterrupt, SystemExit):
        print('\nKeyboardInterrupt, SystemExit')

    # discard the TimeTagger stream
    tts.disconnect()

    filename = os.path.basename(__file__)
    print(f'\n{filename} stopped\n')
