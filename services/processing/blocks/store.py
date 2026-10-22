'''
Every finished block, in memory for the status and on disk in blocks.jsonl for the history, and each neighbor's
clock model that the next block's alignment starts from
'''

import collections
import json
import threading
import time

from ...core import settings

_lock = threading.Lock()
_recent = collections.deque(maxlen=200)


def record(entry):
    entry['finished'] = time.time()
    with _lock:
        _recent.append(entry)
    try:
        with settings.TP_BLOCKS.open('a') as f:
            f.write(json.dumps(entry) + '\n')
    except OSError:
        pass


def recent(n=60):
    with _lock:
        return list(_recent)[-n:]


def history(peer, limit=3000):
    '''This link's blocks from the log on disk, oldest first, so the history outlives a restart'''
    out = []
    try:
        with settings.TP_BLOCKS.open() as f:
            for line in collections.deque(f, maxlen=4 * limit):
                try:
                    b = json.loads(line)
                except ValueError:
                    continue
                if b.get('peer') == peer:
                    out.append(b)
    except OSError:
        pass
    return out[-limit:]


def last_window(peer):
    '''Window this side's filter chose last time with peer, which the announcing side cannot see'''
    with _lock:
        return next((b.get('window_ps') for b in reversed(_recent) if b.get('peer') == peer and b.get('window_ps')), None)


def models():
    try:
        return json.loads(settings.TP_MODELS.read_text())
    except (OSError, ValueError):
        return {}


def save_models(m):
    settings.TP_MODELS.write_text(json.dumps(m))


def load():
    '''Recent blocks of the log, so the status after a restart is not empty'''
    try:
        with settings.TP_BLOCKS.open() as f:
            lines = collections.deque(f, maxlen=_recent.maxlen)
    except OSError:
        return
    with _lock:
        for line in lines:
            try:
                _recent.append(json.loads(line))
            except ValueError:
                pass
