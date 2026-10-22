'''
Link monitor: for one neighbor, both systems, the channels, the quantum channel's stage, the pools and the block
series, and that series as CSV or JSON
'''

import csv
import io
import json

from flask import Blueprint, current_app, jsonify, request
from werkzeug.utils import secure_filename

from ..core import kms, peers, settings
from . import alerts, live_status, state

bp = Blueprint('agent_monitor', __name__)

STAGES = ['Waiting for quantum signal', 'Waiting for classical channel', 'Waiting for sync', 'QBER too high', 'Generating secure keys']
CSV_COLUMNS = ['block', 'role', 'ok', 'segments', 'seconds', 'sifted', 'qber', 'phase_error_bound', 'ec_leak', 'charged',
               'surviving', 'key_bits', 'keys_saved', 'compression', 'window_ps', 'rounds', 'aligned_by', 'skew_ppb', 'error']


def _history(peer):
    return peers.call_json('processing', 'GET', f'/v1/blocks?peer={kms.quote(peer)}')['blocks'] if peer else []


def _sync(peer):
    '''How well the clocks were held, per segment, as the answering node's tracker measured it'''
    return peers.call_json('processing', 'GET', f'/v1/blocks?peer={kms.quote(peer)}&limit=1').get('sync', []) if peer else []


def stage(live, recording, link, last):
    '''Quantum channel's stage, as the TimeTagger vendor's monitor names them'''
    if not (live and sum(live[-1]['singles']) > 0 and recording):
        return 0
    if not link.get('reachable'):
        return 1
    if not last or not last.get('ok') or last.get('sifted') in (None, 0):
        return 2
    return 3 if (last.get('qber') or 0) > settings.ALERT_MAX_QBER else 4


@bp.route('/api/monitor')
def api_monitor():
    st = state.load()
    links = []
    try:
        links = kms.call_json('admin', 'GET', '/kme/v1/node?probe=1').get('links') or []
    except Exception:
        pass
    rec = peers.call_json('acquisition', 'GET', '/v1/recorder')
    neighbors = [l['neighbor'] for l in links] or sorted(set(st['announce_to']))
    peer = request.args.get('peer') or (st['announce_to'][0] if st['announce_to'] else (neighbors[0] if neighbors else None))
    link = next((l for l in links if l['neighbor'] == peer), {})
    blocks = _history(peer)
    last = blocks[-1] if blocks else None
    return jsonify({
        'peer': peer, 'peers': neighbors, 'node': settings.NODE_ID,
        'local': {'site': st['site'], 'recording': rec['recording'], 'since': rec['since'], 'active': st['active'],
                  'segment_s': st['segment_s'], 'block_s': st['block_s']},
        'remote': {'node': peer, 'reachable': link.get('reachable'), 'rtt_ms': link.get('rtt_ms'), 'error': link.get('error')},
        'pool': {'mine': link.get('mine'), 'theirs': link.get('theirs')},
        'stage': stage(rec['live'], rec['recording'], link, last), 'stages': STAGES,
        'qber_threshold': settings.ALERT_MAX_QBER, 'announces': peer in st['announce_to'],
        'live': rec['live'], 'fast': rec['fast'], 'remote_fast': live_status.remote(peer) if peer else [],
        'blocks': blocks, 'ok_blocks': sum(1 for b in blocks if b.get('ok')), 'alerts': alerts.current(),
        'sync': _sync(peer)})


def _download(text, mimetype, name):
    resp = current_app.response_class(text, mimetype=mimetype)
    resp.headers['Content-Disposition'] = f'attachment; filename="{name}"'
    return resp


@bp.route('/api/monitor/blocks.<fmt>')
def api_blocks(fmt):
    '''This link's blocks, for spreadsheets and offline analysis'''
    peer = request.args.get('peer', '')
    blocks = _history(peer)
    name = f'qkd_blocks_{secure_filename(peer) or "none"}'
    if fmt == 'json':
        return _download(json.dumps(blocks, indent=1), 'application/json', f'{name}.json')
    if fmt != 'csv':
        return jsonify({'error': 'csv or json'}), 404
    buf = io.StringIO()
    w = csv.writer(buf)
    w.writerow(CSV_COLUMNS + ['basis_bits_hv', 'basis_bits_da', 'basis_errors_hv', 'basis_errors_da', 'basis_checked_hv', 'basis_checked_da']
               + [f'singles_local_{c}' for c in range(1, 5)] + [f'singles_remote_{c}' for c in range(1, 5)])
    for b in blocks:
        pair = lambda k: (b.get(k) or [None, None])[:2]  # noqa: E731
        quad = lambda k: (b.get(k) or [None] * 4)[:4]  # noqa: E731
        w.writerow([b.get(c) for c in CSV_COLUMNS] + pair('basis_bits') + pair('basis_errors') + pair('basis_checked')
                   + quad('singles_local') + quad('singles_remote'))
    return _download(buf.getvalue(), 'text/csv', f'{name}.csv')
