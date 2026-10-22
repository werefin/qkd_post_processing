'''
Continuous two-party QKD settings of this node, kept on disk so a restart resumes them; set from the dashboard,
or by the network controller when it manages the node
'''

import json
import os
import threading

from ..core import settings

# block_auto: each block planned from the link's last ones, up to block_s; off, every block is block_s long
DEFAULTS = {'active': False, 'site': None, 'segment_s': 5.0, 'block_s': 900.0, 'block_auto': True, 'announce_to': [],
            'expected_qber': 0.05, 'keep_blocks': 2, 'next_block': {}, 'plans': {}, 'replay': None, 'started': None,
            'managed': False}

lock = threading.Lock()  # held around every load, change and save, by the routes, the scheduler and the controller


def load():
    try:
        return {**DEFAULTS, **json.loads(settings.TP_STATE.read_text())}
    except (OSError, ValueError):
        return dict(DEFAULTS)


def save(state):
    tmp = settings.TP_STATE.with_suffix('.tmp')
    tmp.write_text(json.dumps(state, indent=1))
    os.replace(tmp, settings.TP_STATE)


def recorder_params(state):
    '''What the acquisition service needs to record for this node, and how long segments must be kept'''
    return {'site': state['site'], 'segment_s': state['segment_s'], 'replay': state.get('replay'),
            'keep_s': state['keep_blocks'] * state['block_s']}


def check(segment_s, block_s):
    if not 1 <= segment_s <= 600 or not 30 <= block_s <= 86400 or block_s % segment_s:
        raise ValueError('segments of 1-600 s, blocks of 30 s to a day and a whole number of segments')
