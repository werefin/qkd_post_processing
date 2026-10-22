'''
Length of each link's next block: importtt predicts every length's finite key from the link's earlier blocks and
takes the shortest within 5% of the best expected rate; all of it public and fixed before the block's own data
exists, so every block stays as secure as a fixed one
'''

import json
import math
import subprocess
import time
import urllib.parse

from ..core import peers, settings

FIRST_S = 300      # a link with no finished block yet: long enough to certify key on most links
RECENT_OK = 10     # finished blocks the sifted rate, QBER and Cascade's efficiency are averaged over
RECENT_ALL = 50    # blocks, failed ones included, the failure rate is counted over
PRIOR_S = 3600     # as if an hour without failure had been seen, so one early failure does not cut every block


def _h2(p):
    return 0.0 if p <= 0 or p >= 1 else -p * math.log2(p) - (1 - p) * math.log2(1 - p)


def _whole(seconds, segment_s, block_s):
    return min(block_s, max(segment_s, math.ceil(seconds / segment_s) * segment_s))


def record(blocks):
    '''The link's public record from this node's half of its last blocks; None until one finished with key data'''
    mine = [b for b in blocks if b.get('role') == 'alice'][-RECENT_ALL:]
    ok = [b for b in mine if b.get('ok') and b.get('sifted') and b.get('seconds') and b.get('qber') is not None][-RECENT_OK:]
    if not ok:
        return None
    sifted = sum(b['sifted'] for b in ok)
    ideal = sum(b['sifted'] * _h2(b['qber']) for b in ok)
    leak = sum(b.get('ec_leak') or 0 for b in ok)
    run = sum(b.get('block_s') or b.get('seconds') or 0 for b in mine)
    failed = sum(1 for b in mine if not b.get('ok'))
    return {'sifted_per_s': sifted / sum(b['seconds'] for b in ok), 'qber': sum(b['qber'] * b['sifted'] for b in ok) / sifted,
            'ec_efficiency': leak / ideal if ideal and leak else 0.0, 'fail_per_s': failed / (run + PRIOR_S)}


def length(st, peer):
    '''(seconds, plan) of the next block toward peer: block_s when fixed, else planned up to block_s; the plan
    is None when there was nothing to plan from yet'''
    segment_s, block_s = float(st['segment_s']), float(st['block_s'])
    if not st.get('block_auto'):
        return block_s, None
    try:
        blocks = peers.call_json('processing', 'GET', f'/v1/blocks?peer={urllib.parse.quote(peer)}&limit={2 * RECENT_ALL}',
                                 timeout=10)['blocks']
    except (peers.PeerError, KeyError):
        blocks = []
    rec = record(blocks)
    if rec is None:
        return _whole(FIRST_S, segment_s, block_s), None
    args = {**rec, 'step_s': segment_s, 'min_s': max(30.0, segment_s), 'max_s': block_s}
    try:
        out = subprocess.run([settings.IMPORTTT_BIN, '-plan-block', json.dumps(args)], capture_output=True, text=True,
                             timeout=60, check=True)
        plan = json.loads(out.stdout.strip().splitlines()[-1])
        seconds = _whole(float(plan['block_s']), segment_s, block_s)
    except (OSError, subprocess.SubprocessError, ValueError, KeyError, IndexError):
        return _whole(FIRST_S, segment_s, block_s), None
    return seconds, {**rec, **plan, 'at': time.time()}
