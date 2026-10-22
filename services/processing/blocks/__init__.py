'''
Block work of continuous two-party QKD: blocks run one at a time, apart from the manual jobs, so neither holds up
the other; the agent queues Alice's blocks on its schedule and Bob's as neighbors announce them
'''

import queue
import threading
import time

from flask import Blueprint, jsonify, request

from ...core import metrics, settings
from . import alice, bob, store

bp = Blueprint('processing_blocks', __name__)

_queue = queue.Queue()
_running = {'now': None}


def queue_alice(peer, block, block_s, expected_qber):
    _queue.put(('alice', peer, (float(block), float(block_s), float(expected_qber))))


def queue_bob(peer, session):
    _queue.put(('bob', peer, (session,)))


def _worker():
    settings.TP_WORK.mkdir(parents=True, exist_ok=True)
    while True:
        role, peer, args = _queue.get()
        _running['now'] = {'role': role, 'peer': peer, 'since': time.time()}
        try:
            (alice.run if role == 'alice' else bob.run)(peer, *args)
        finally:
            _running['now'] = None


@bp.route('/v1/blocks', methods=['GET', 'POST'])
def api_blocks():
    '''GET: one link's history (peer, limit); POST: queues Alice's side of a block (peer, block, block_s, expected_qber)'''
    if request.method == 'GET':
        peer = request.args.get('peer', '')
        return jsonify({'peer': peer, 'blocks': store.history(peer, int(request.args.get('limit') or 3000)) if peer else []})
    body = request.get_json(silent=True) or {}
    try:
        queue_alice(str(body['peer']), body['block'], body['block_s'], body.get('expected_qber', 0.05))
    except (KeyError, TypeError, ValueError):
        return jsonify({'error': 'peer, block and block_s are required'}), 400
    return jsonify({'queued': _queue.qsize()})


@bp.route('/v1/blocks/status')
def api_status():
    models = store.models()
    return jsonify({'queued': _queue.qsize(), 'running': _running['now'], 'blocks': store.recent(int(request.args.get('n') or 60)),
                    'skew_ppb': {p: m['slope'] / 1e3 for p, m in models.items()}})


def start_threads():
    store.load()
    threading.Thread(target=_worker, daemon=True).start()


def _last_per_peer(field):
    out = {}
    for b in store.recent(200):
        if b.get('ok') and b.get(field) is not None:
            out[b['peer']] = b[field]
    return out


metrics.gauge('blocks_queued', lambda: _queue.qsize())
metrics.gauge('block_qber', lambda: _last_per_peer('qber'))
metrics.gauge('block_key_bits', lambda: _last_per_peer('key_bits'))
metrics.gauge('block_failed_recent', lambda: sum(1 for b in store.recent(20) if not b.get('ok')))
