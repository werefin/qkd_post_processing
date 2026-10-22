'''
Block work of continuous two-party QKD: one worker per neighbor and role, so a node announcing to one neighbor
and answering another keeps up with both; the agent queues Alice's blocks as soon as their first segment closes,
and Bob's as neighbors announce them
'''

import queue
import threading
import time

from flask import Blueprint, jsonify, request

from ...core import metrics, settings
from . import alice, bob, store

bp = Blueprint('processing_blocks', __name__)

_lock = threading.Lock()
_queues = {}   # (role, peer): queue of argument tuples
_running = {}  # (role, peer): what that worker is doing


def _worker(role, peer, q):
    settings.TP_WORK.mkdir(parents=True, exist_ok=True)
    while True:
        args = q.get()
        _running[(role, peer)] = {'role': role, 'peer': peer, 'since': time.time()}
        try:
            (alice.run if role == 'alice' else bob.run)(peer, *args)
        finally:
            _running.pop((role, peer), None)


def _put(role, peer, args):
    with _lock:
        if (role, peer) not in _queues:
            _queues[(role, peer)] = queue.Queue()
            threading.Thread(target=_worker, args=(role, peer, _queues[(role, peer)]), daemon=True).start()
        _queues[(role, peer)].put(args)


def queue_alice(peer, block, block_s, segment_s, expected_qber):
    _put('alice', peer, (float(block), float(block_s), float(segment_s), float(expected_qber)))


def queue_bob(peer, session):
    _put('bob', peer, (session,))


def queued():
    return sum(q.qsize() for q in list(_queues.values()))


@bp.route('/v1/blocks', methods=['GET', 'POST'])
def api_blocks():
    '''GET: one link's history (peer, limit); POST: queues Alice's side of a block (peer, block, block_s,
    segment_s, expected_qber), which streams its segments as they close'''
    if request.method == 'GET':
        peer = request.args.get('peer', '')
        return jsonify({'peer': peer, 'blocks': store.history(peer, int(request.args.get('limit') or 3000)) if peer else [],
                        'sync': store.sync(peer) if peer else []})
    body = request.get_json(silent=True) or {}
    try:
        queue_alice(str(body['peer']), body['block'], body['block_s'], body.get('segment_s', 5), body.get('expected_qber', 0.05))
    except (KeyError, TypeError, ValueError):
        return jsonify({'error': 'peer, block and block_s are required'}), 400
    return jsonify({'queued': queued()})


@bp.route('/v1/blocks/status')
def api_status():
    models = store.models()
    running = list(_running.values())
    return jsonify({'queued': queued(), 'running': running[0] if running else None, 'running_all': running,
                    'blocks': store.recent(int(request.args.get('n') or 60)),
                    'skew_ppb': {p: m['slope'] / 1e3 for p, m in models.items()}})


def start_threads():
    store.load()


def _last_per_peer(field):
    out = {}
    for b in store.recent(200):
        if b.get('ok') and b.get(field) is not None:
            out[b['peer']] = b[field]
    return out


metrics.gauge('blocks_queued', queued)
metrics.gauge('block_qber', lambda: _last_per_peer('qber'))
metrics.gauge('block_key_bits', lambda: _last_per_peer('key_bits'))
metrics.gauge('block_failed_recent', lambda: sum(1 for b in store.recent(20) if not b.get('ok')))
