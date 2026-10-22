'''
Link to the network controller: every few seconds this node's status goes up in a heartbeat, signed by its admin
certificate, and the settings the controller wants for it come back and are applied; unset, the node runs alone
'''

import json
import ssl
import threading
import time
import urllib.error
import urllib.request

from flask import Blueprint, jsonify

from ..core import kms, metrics, peers, settings
from . import alerts, state, twoparty

bp = Blueprint('agent_controller', __name__)

MANAGED_KEYS = ('active', 'site', 'announce_to', 'segment_s', 'block_s', 'block_auto', 'expected_qber', 'keep_blocks')
link = {'connected': False, 'last_ok': None, 'error': None, 'revision': None, 'apply_error': None}
_ctx = {}


def _context():
    if 'ctx' not in _ctx:
        cert, key = settings.KMS_IDENTITIES['admin']
        ctx = ssl.create_default_context(cafile=settings.CONTROLLER_CA or None)
        ctx.minimum_version = ssl.TLSVersion.TLSv1_3
        ctx.load_cert_chain(cert, key)
        _ctx['ctx'] = ctx
    return _ctx['ctx']


def _status():
    '''What the controller shows of this node: its services, recorder, settings, last block per link, pools'''
    st = state.load()
    out = {'time': time.time(), 'active': st['active'], 'site': st['site'], 'announce_to': st['announce_to'],
           'managed': st['managed'], 'alerts': alerts.current(), 'apply_error': link['apply_error'], 'services': {}}
    try:
        rec = peers.call_json('acquisition', 'GET', '/v1/recorder?brief=1', timeout=5)
        out.update(recording=rec['recording'], singles=rec.get('singles_now'))
        out['services']['acquisition'] = True
    except peers.PeerError:
        out['services']['acquisition'] = False
    try:
        last = {}
        for b in peers.call_json('processing', 'GET', '/v1/blocks/status?n=200', timeout=5)['blocks']:
            last[b['peer']] = {k: b.get(k) for k in ('ok', 'qber', 'key_bits', 'finished', 'role', 'error')}
        out['blocks'] = last
        out['services']['processing'] = True
    except peers.PeerError:
        out['services']['processing'] = False
    try:
        out['pools'] = {l['neighbor']: {'mine': l.get('mine'), 'theirs': l.get('theirs')}
                        for l in kms.call_json('admin', 'GET', '/kme/v1/node').get('links') or []}
        out['services']['kms'] = True
    except kms.KMSError:
        out['services']['kms'] = False
    return out


def _heartbeat(status):
    req = urllib.request.Request(f'{settings.CONTROLLER_URL}/v1/agents/{kms.quote(settings.NODE_ID)}/heartbeat',
                                 data=json.dumps(status).encode(), method='POST', headers={'Content-Type': 'application/json'})
    with urllib.request.urlopen(req, context=_context(), timeout=15) as r:
        return json.loads(r.read() or b'{}')


def apply(desired):
    '''Takes the controller's settings for this node: the recorder follows them, the schedule restarts when the node
    is switched on; a node the controller does not manage goes back to its own settings'''
    with state.lock:
        old = state.load()
        if not desired.get('managed'):
            if old['managed']:
                state.save({**old, 'managed': False})
            return
        new = {**old, 'managed': True, **{k: desired[k] for k in MANAGED_KEYS if k in desired}}
        if old['managed'] and all(old.get(k) == new.get(k) for k in MANAGED_KEYS):
            return
        if new['active'] and not old['active']:
            new.update(started=time.time(), next_block={}, plans={})
        status, answer = twoparty.follow(new, old)
        link['apply_error'] = answer.get('error') if status >= 400 else None
        state.save(new)


def _sync():
    while True:
        try:
            answer = _heartbeat(_status())
            link.update(connected=True, last_ok=time.time(), error=None, revision=answer.get('revision'))
            apply(answer.get('config') or {})
        except (urllib.error.URLError, OSError, ValueError) as e:
            link.update(connected=False, error=str(getattr(e, 'reason', e))[:200])
        time.sleep(settings.CONTROLLER_INTERVAL_S)


@bp.route('/api/controller')
def api_controller():
    return jsonify({'url_set': bool(settings.CONTROLLER_URL), **link})


def start_threads():
    if settings.CONTROLLER_URL and settings.NODE_ID:
        threading.Thread(target=_sync, daemon=True).start()


metrics.gauge('controller_connected', lambda: int(link['connected']) if settings.CONTROLLER_URL else None)
