'''
Lab continuous QKD: tt_record_dual.py records a link in segments while continuous_qkd.py distils a key block from
them every block_s seconds of data, both on this node
'''

import collections
import signal
import sys
import time

from flask import Blueprint, jsonify, request

from ..core import kms, procs, settings
from ..core import recordings as rec
from . import acquire, config, tagger

bp = Blueprint('acquisition_continuous', __name__)

STATUS_PREFIX = 'CONT '  # continuous_qkd.py: one JSON status per line
state = {'proc': None, 'run': None, 'status': None, 'log': collections.deque(maxlen=400), 'params': None, 'started_at': None}


def running():
    return procs.running(state['proc'])


def recording():
    return acquire.state['kind'] == 'continuous' and acquire.running()


def _on_line(line):
    st = procs.prefixed_json(line, STATUS_PREFIX)
    if st is not None:
        state['status'] = st
    # the processor's own lines and problems; the peak search's progress lines would drown them
    elif '[continuous]' in line or line.startswith(('[WARN]', '[ERROR]', 'Traceback')):
        state['log'].append(line)


def _parse(body):
    segment_s = float(body.get('segment_s', 10))
    block_s = float(body.get('block_s', 300))
    speed = float((body.get('replay') or {}).get('speed', 1))
    method = body.get('method', 'cascade')
    if method != 'cascade' or not 1 <= segment_s <= 600 or not 10 <= block_s <= 86400 or speed <= 0:
        raise ValueError('optimized Cascade, segments of 1-600 s, blocks of 10 s to a day')
    neighbor = body.get('neighbor') or ''
    if neighbor and neighbor not in kms.neighbors():
        raise ValueError(f'{neighbor} is not a QKD link of this node')
    return segment_s, block_s, speed, method, neighbor


@bp.route('/api/continuous/start', methods=['POST'])
def api_start():
    if not config.loaded():
        return jsonify({'error': 'load a config first'}), 400
    body = request.get_json(silent=True) or {}
    try:
        segment_s, block_s, speed, method, neighbor = _parse(body)
    except (TypeError, ValueError) as e:
        return jsonify({'error': str(e)}), 400
    with tagger.lock:
        if (mode := tagger.busy()):
            return jsonify(tagger.refusal(mode)), 409
        run = settings.CONTINUOUS_DIR / time.strftime('%Y%m%d_%H%M%S')
        run.mkdir(parents=True, exist_ok=True)
        recorder = [sys.executable, str(settings.ACQUISITION_DIR / 'tt_record_dual.py'), str(settings.UPLOADED_CONFIG_PATH),
                    '--stop-mode', 'continuous', '--segment-s', str(segment_s), '--output-dir', str(run)]
        replay = body.get('replay')
        if replay:
            try:
                path_a, path_b = rec.resolve_pair(replay.get('a'), replay.get('b'))
            except ValueError as e:
                return jsonify({'error': str(e)}), 400
            recorder += ['--replay', str(path_a), str(path_b), '--replay-speed', str(speed)]
        elif body.get('link'):
            recorder += ['--link', str(body['link'])]
        resp = acquire.start(recorder, 'continuous')
        if not acquire.running():
            return resp
        processor = [sys.executable, str(settings.ACQUISITION_DIR / 'continuous_qkd.py'), str(run), '--importtt', settings.IMPORTTT_BIN,
                     '--config', str(settings.UPLOADED_CONFIG_PATH), '--block-s', str(block_s), '--method', method]
        if neighbor:
            processor += ['--keystore', str(settings.IMPORT_DIR / neighbor), '--keystore-max', str(settings.KEYSTORE_MAX)]
        state['log'].clear()
        state['status'] = None
        try:
            proc = procs.start(processor, _on_line)
        except OSError as e:
            acquire.state['proc'].send_signal(signal.SIGINT)
            return jsonify({'error': f'could not start the processor: {e}'}), 500
        state.update(proc=proc, run=run.name, started_at=time.time(),
                     params={'segment_s': segment_s, 'block_s': block_s, 'method': method, 'neighbor': neighbor,
                             'source': 'replay' if replay else (body.get('link') or 'default link'), 'speed': speed if replay else 1})
    return jsonify({'started': True, 'run': run.name})


@bp.route('/api/continuous/stop', methods=['POST'])
def api_stop():
    '''Stops recording; the processor finishes the segments already written and distils the last block'''
    with tagger.lock:
        if recording():
            acquire.state['proc'].send_signal(signal.SIGINT)
            return jsonify({'stopping': True})
        if running() and request.args.get('abort') == '1':
            state['proc'].send_signal(signal.SIGINT)
            return jsonify({'aborting': True})
    return jsonify({'error': 'no continuous recording is running'}), 409


@bp.route('/api/continuous/status')
def api_status():
    return jsonify({'recording': recording(), 'processing': running(), 'run': state['run'], 'params': state['params'],
                    'started_at': state['started_at'], 'status': state['status'], 'log': list(state['log'])[-150:]})


tagger.register('continuous QKD', running)
