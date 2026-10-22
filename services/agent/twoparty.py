'''
Continuous two-party QKD, the production mode: the acquisition service records this node's TimeTagger in UTC-aligned
segments, and the agent queues each finished block toward every neighbor this node announces to; resumes after a
restart, and both pools receive the same keys
'''

import threading
import time

from flask import Blueprint, jsonify, request

from ..core import kms, metrics, peers
from . import alerts, blockplan, state

bp = Blueprint('agent_twoparty', __name__)

GRACE_S = 5  # after a block ends, for both sites to write the segment that closes it
_RECORDER_KEYS = ('site', 'segment_s', 'replay')


def follow(new, old=None):
    '''Makes the recorder follow new settings: restarted when what it records changed, stopped when inactive;
    returns (status, answer) of the acquisition service'''
    if old and old['active'] and any(old.get(k) != new.get(k) for k in _RECORDER_KEYS):
        peers.call('acquisition', 'POST', '/v1/recorder/stop', {}, timeout=90)
    if not new['active']:
        return peers.call('acquisition', 'POST', '/v1/recorder/stop', {}, timeout=90)
    return peers.call('acquisition', 'POST', '/v1/recorder/start', state.recorder_params(new), timeout=120)


def _managed():
    return jsonify({'error': 'the network controller manages this node: change its settings there'}), 409


@bp.route('/api/twoparty/status')
def api_status():
    st = state.load()
    rec = peers.call_json('acquisition', 'GET', '/v1/recorder')
    blk = peers.call_json('processing', 'GET', '/v1/blocks/status')
    return jsonify({'state': {k: v for k, v in st.items() if k != 'next_block'}, 'next_block': st['next_block'],
                    'recording': rec['recording'], 'segments': rec['segments'], 'last_segment': rec['last_segment'],
                    'queued': blk['queued'], 'running': blk['running'], 'blocks': blk['blocks'],
                    'skew_ppb': blk['skew_ppb'], 'alerts': alerts.current(), 'log': rec.get('log', [])})


@bp.route('/api/twoparty/start', methods=['POST'])
def api_start():
    with state.lock:
        return _start(state.load(), request.get_json(silent=True) or {})


def _start(old, body):
    if old['managed']:
        return _managed()
    try:
        segment_s, block_s = float(body.get('segment_s', old['segment_s'])), float(body.get('block_s', old['block_s']))
        state.check(segment_s, block_s)
        announce = [p for p in body.get('announce_to', old['announce_to']) if p]
        known = set(kms.neighbors())
        if any(p not in known for p in announce):
            raise ValueError(f'announce only to neighbors of this node: {", ".join(sorted(known)) or "none"}')
        expected_qber = float(body.get('expected_qber', old['expected_qber']))
    except (TypeError, ValueError) as e:
        return jsonify({'error': str(e)}), 400
    # a fresh start begins at the newest segment; resuming after a restart keeps the schedule where it was
    new = {**old, 'active': True, 'site': body.get('site') or old['site'], 'segment_s': segment_s, 'block_s': block_s,
           'announce_to': announce, 'expected_qber': expected_qber, 'started': time.time(), 'next_block': {},
           'block_auto': bool(body.get('block_auto', old['block_auto'])), 'plans': {},
           'replay': body.get('replay', old.get('replay')) or None}  # a start without settings keeps the saved ones
    status, answer = follow(new, old)
    if status >= 400:
        return jsonify(answer), status
    state.save(new)
    return jsonify({'started': True})


@bp.route('/api/twoparty/stop', methods=['POST'])
def api_stop():
    '''Stops recording and scheduling; blocks already queued still finish'''
    with state.lock:
        st = state.load()
        if st['managed']:
            return _managed()
        st['active'] = False
        state.save(st)
    follow(st)
    return jsonify({'stopped': True})


def _queue_due(st):
    '''Queues every block whose first segment both sites have written toward each neighbor this node announces
    to, so its segments stream while it runs; each block's length is set as it is queued, from the blocks before
    it, and a block is only marked done once the processing service took it'''
    segment_s, now = float(st['segment_s']), time.time()
    newest = (now - GRACE_S - segment_s) // segment_s * segment_s  # the newest segment both sites have written
    oldest = now - st['keep_blocks'] * float(st['block_s'])
    changed = False
    for peer in st['announce_to']:
        nxt = st['next_block'].get(peer)
        if nxt is None or nxt < oldest:
            nxt = newest  # a new link, or blocks too old to still be on disk: start from the newest segment
        while nxt <= newest:
            seconds, plan = blockplan.length(st, peer)
            try:
                peers.call_json('processing', 'POST', '/v1/blocks', {'peer': peer, 'block': nxt, 'block_s': seconds,
                                                                    'segment_s': segment_s, 'expected_qber': st['expected_qber']})
            except peers.PeerError:
                break
            nxt += seconds
            st.setdefault('plans', {})[peer] = {**(plan or {}), 'block_s': seconds, 'start': nxt - seconds}
            changed = True
        if st['next_block'].get(peer) != nxt:
            st['next_block'][peer], changed = nxt, True
    return changed


def _scheduler():
    while True:
        time.sleep(2)
        with state.lock:
            st = state.load()
            if st['active'] and _queue_due(st):
                state.save(st)


def start_threads():
    threading.Thread(target=_scheduler, daemon=True).start()


metrics.gauge('twoparty_active', lambda: int(state.load()['active']))
metrics.gauge('twoparty_managed', lambda: int(state.load()['managed']))
