'''
Client of this node's key management system, as the node's admin or as its dashboard SAE, and the KMS mailbox
that carries two-party post-processing messages between neighbors
'''

import base64
import hashlib
import json
import ssl
import time
import urllib.error
import urllib.parse
import urllib.request

from . import settings

PP_WAIT_S = 900  # how long one side waits for the other's next step
# a mailbox message with raw bytes: a 4-byte big-endian header length, the JSON header, the bytes as they are
FRAME_TYPE = 'application/x-qkd-pp'


class KMSError(Exception):
    pass


_ssl_contexts = {}


def _context(identity):
    if identity not in _ssl_contexts:
        cert, key = settings.KMS_IDENTITIES[identity]
        if not (settings.KMS_URL and settings.KMS_CA and cert and key):
            raise KMSError('this node is not connected to a KMS (KMS_URL and its certificates are unset)')
        ctx = ssl.create_default_context(cafile=settings.KMS_CA)
        ctx.minimum_version = ssl.TLSVersion.TLSv1_3
        ctx.load_cert_chain(cert, key)
        _ssl_contexts[identity] = ctx
    return _ssl_contexts[identity]


def frame(header, data):
    h = json.dumps(header).encode()
    return len(h).to_bytes(4, 'big') + h + data


def unframe(raw):
    '''(header, bytes) back from frame'''
    n = int.from_bytes(raw[:4], 'big')
    if len(raw) < 4 or n > len(raw) - 4:
        raise KMSError('mailbox frame of the wrong size')
    return json.loads(raw[4:4 + n]), raw[4 + n:]


def call(identity, method, path, body=None, timeout=20, data=None):
    '''One call to this node's KMS as the admin or SAE identity; returns (status, json). With data, body and data
    go as one frame; a frame answered comes back as its header with its bytes under the key blob'''
    ctx = _context(identity)
    payload, ctype = (None, 'application/json') if body is None else (json.dumps(body).encode(), 'application/json')
    if data is not None:
        payload, ctype = frame(body, data), FRAME_TYPE
    req = urllib.request.Request(settings.KMS_URL + path, data=payload, method=method, headers={'Content-Type': ctype})
    try:
        with urllib.request.urlopen(req, context=ctx, timeout=timeout) as r:
            raw = r.read()
            if r.headers.get('Content-Type') == FRAME_TYPE:
                header, blob = unframe(raw)
                return r.status, {**header, 'blob': blob}
            return r.status, json.loads(raw or b'{}')
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read() or b'{}')
        except ValueError:
            return e.code, {'message': str(e.reason)}
    except (urllib.error.URLError, OSError) as e:
        raise KMSError(f'KMS unreachable: {getattr(e, "reason", e)}')


def call_json(identity, method, path, body=None):
    status, data = call(identity, method, path, body)
    if status != 200:
        raise KMSError(data.get('message') or f'KMS answered {status}')
    return data


def quote(s):
    return urllib.parse.quote(str(s), safe='')


def fingerprint(key_b64):
    '''First 64 bits of SHA-256: enough to compare two ends by eye, nothing usable about the key'''
    return hashlib.sha256(base64.b64decode(key_b64)).hexdigest()[:16]


def neighbors():
    try:
        return [link['neighbor'] for link in call_json('admin', 'GET', '/kme/v1/node').get('links') or []]
    except KMSError:
        return []


def pp_send(peer, session, kind, body, data=None):
    '''One message to peer; data, raw bytes, crosses both KMS hops as it is and arrives in the body under blob'''
    status, data = call('admin', 'POST', '/kme/v1/pp/send', {'peer': peer, 'session': session, 'kind': kind, 'body': body},
                        timeout=300, data=data)
    if status != 200:
        raise RuntimeError(f'sending {kind} to {peer}: {data.get("message") or status}')


def pp_recv(peer, session, timeout=PP_WAIT_S):
    '''Next message of a session from peer, waiting up to timeout seconds'''
    deadline = time.time() + timeout
    q = urllib.parse.urlencode({'peer': peer, 'session': session, 'wait': 30})
    while time.time() < deadline:
        status, data = call('admin', 'GET', f'/kme/v1/pp/recv?{q}', timeout=60)
        if status == 200:
            body = data['body']
            if 'blob' in data:
                body = {**(body or {}), 'blob': data['blob']}
            return data['kind'], body
        if status != 204:
            raise RuntimeError(f'receiving from {peer}: {data.get("message") or status}')
    raise RuntimeError(f'{peer} did not answer within {timeout} s')


def pp_drain(peer, session, limit=50):
    '''Messages of a session already waiting, without blocking'''
    q = urllib.parse.urlencode({'peer': peer, 'session': session, 'wait': 0})
    out = []
    for _ in range(limit):
        try:
            status, data = call('admin', 'GET', f'/kme/v1/pp/recv?{q}', timeout=10)
        except KMSError:
            break
        if status != 200:
            break
        out.append(data.get('body') or {})
    return out
