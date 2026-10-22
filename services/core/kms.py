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


def call(identity, method, path, body=None, timeout=20):
    '''One call to this node's KMS as the admin or SAE identity; returns (status, json)'''
    ctx = _context(identity)
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(settings.KMS_URL + path, data=data, method=method, headers={'Content-Type': 'application/json'})
    try:
        with urllib.request.urlopen(req, context=ctx, timeout=timeout) as r:
            return r.status, json.loads(r.read() or b'{}')
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


def pp_send(peer, session, kind, body):
    status, data = call('admin', 'POST', '/kme/v1/pp/send', {'peer': peer, 'session': session, 'kind': kind, 'body': body}, timeout=300)
    if status != 200:
        raise RuntimeError(f'sending {kind} to {peer}: {data.get("message") or status}')


def pp_recv(peer, session, timeout=PP_WAIT_S):
    '''Next message of a session from peer, waiting up to timeout seconds'''
    deadline = time.time() + timeout
    q = urllib.parse.urlencode({'peer': peer, 'session': session, 'wait': 30})
    while time.time() < deadline:
        status, data = call('admin', 'GET', f'/kme/v1/pp/recv?{q}', timeout=60)
        if status == 200:
            return data['kind'], data['body']
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
