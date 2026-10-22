'''
Alerts: what an operator must know without watching the dashboard, posted to ALERT_WEBHOOK when they change; the
watchdog also restarts the recorder when it stopped while continuous QKD is on
'''

import json
import threading
import time
import urllib.request

from flask import Blueprint, jsonify

from ..core import kms, metrics, peers, settings
from . import state

bp = Blueprint('agent_alerts', __name__)

_now = {'alerts': [], 'sent': None}


def current():
    return _now['alerts']


def _block_alerts(st, blocks, now):
    out = []
    for peer in sorted(set(st['announce_to']) | {b['peer'] for b in blocks}):
        mine = [b for b in blocks if b['peer'] == peer]
        ok = [b for b in mine if b.get('ok') and b.get('key_bits')]
        if mine and not mine[-1].get('ok'):
            out.append({'level': 'warning', 'what': f'block:{peer}', 'text': f'last block with {peer} failed: {mine[-1].get("error", "")}'})
        if mine and mine[-1].get('qber') is not None and mine[-1]['qber'] > settings.ALERT_MAX_QBER:
            out.append({'level': 'warning', 'what': f'qber:{peer}', 'text': f'QBER with {peer} is {100 * mine[-1]["qber"]:.1f}%'})
        since = ok[-1]['finished'] if ok else st.get('started') or now
        if peer in st['announce_to'] and now - since > 3 * st['block_s'] + 600:
            out.append({'level': 'critical', 'what': f'nokey:{peer}', 'text': f'no key with {peer} for {(now - since) / 60:.0f} min'})
    return out


def _pool_alerts():
    try:
        links = kms.call_json('admin', 'GET', '/kme/v1/node').get('links') or []
    except Exception:
        return [{'level': 'critical', 'what': 'kms', 'text': 'the key management system does not answer'}]
    return [{'level': 'warning', 'what': f'pool:{link["neighbor"]}',
             'text': f'{link.get("mine", 0)} keys left toward {link["neighbor"]} (under {settings.ALERT_MIN_KEYS})'}
            for link in links if link.get('mine', 0) < settings.ALERT_MIN_KEYS]


def evaluate(recording):
    '''Alerts for the current state; recording is whether the recorder runs, None if unknown'''
    out, st, now = [], state.load(), time.time()
    if st['active']:
        if recording is False:
            out.append({'level': 'critical', 'what': 'recorder', 'text': "this node's TimeTagger recorder has stopped; restarting it"})
        try:
            out += _block_alerts(st, peers.call_json('processing', 'GET', '/v1/blocks/status?n=200')['blocks'], now)
        except peers.PeerError as e:
            out.append({'level': 'critical', 'what': 'processing', 'text': str(e)})
    if settings.KMS_URL:
        out += _pool_alerts()
    return out


def _recording():
    try:
        return peers.call_json('acquisition', 'GET', '/v1/recorder?brief=1', timeout=10)['recording']
    except peers.PeerError:
        return None


def _post(alerts):
    key = sorted(a['what'] for a in alerts)
    if not settings.ALERT_WEBHOOK or key == _now['sent']:
        return
    _now['sent'] = key
    try:
        req = urllib.request.Request(settings.ALERT_WEBHOOK, data=json.dumps({'node': settings.NODE_ID, 'alerts': alerts}).encode(),
                                     headers={'Content-Type': 'application/json'})
        urllib.request.urlopen(req, timeout=10).close()
    except Exception:
        pass  # tried again when the alerts change


def _watchdog():
    while True:
        time.sleep(15)
        recording = _recording()
        try:
            alerts = evaluate(recording)
        except Exception as e:  # the watchdog itself must keep running
            alerts = [{'level': 'warning', 'what': 'watchdog', 'text': f'alert check failed: {e}'[:200]}]
        if recording is None:
            alerts.append({'level': 'critical', 'what': 'acquisition', 'text': 'the acquisition service does not answer'})
        st = state.load()
        if st['active'] and recording is False:
            try:
                peers.call('acquisition', 'POST', '/v1/recorder/start', state.recorder_params(st), timeout=120)
            except peers.PeerError:
                pass
        _now['alerts'] = alerts
        _post(alerts)


@bp.route('/api/alerts')
def api_alerts():
    return jsonify({'alerts': current()})


def start_threads():
    threading.Thread(target=_watchdog, daemon=True).start()


metrics.gauge('alerts', lambda: {lvl: sum(1 for a in current() if a['level'] == lvl) for lvl in ('critical', 'warning')})
