'''
Every finished block, in memory for the status and on disk in blocks.jsonl for the history, rotated by size
(BLOCKS_LOG_MAX_MB, BLOCKS_LOG_KEEP), and each neighbor's clock model that the next block's alignment starts from
'''

import collections
import json
import threading
import time

from ...core import settings

_lock = threading.Lock()
_recent = collections.deque(maxlen=200)
_sync = {}  # neighbor: how well the clocks were held, one point per segment, the last few hours


def _rotated(i):
    return settings.TP_BLOCKS.with_name(f'{settings.TP_BLOCKS.name}.{i}')


def _rotate():
    '''blocks.jsonl --> blocks.jsonl.1 and each older file one up, the one past BLOCKS_LOG_KEEP dropped; caller holds _lock'''
    _rotated(settings.BLOCKS_LOG_KEEP).unlink(missing_ok=True)
    for i in range(settings.BLOCKS_LOG_KEEP - 1, 0, -1):
        if _rotated(i).exists():
            _rotated(i).replace(_rotated(i + 1))
    settings.TP_BLOCKS.replace(_rotated(1))


def _tail(n):
    '''The last n lines of the log, oldest first, reaching back into the rotated files as far as it takes'''
    chunks, have = [], 0
    for path in [settings.TP_BLOCKS] + [_rotated(i) for i in range(1, settings.BLOCKS_LOG_KEEP + 1)]:
        try:
            with path.open() as f:
                lines = list(collections.deque(f, maxlen=n - have))
        except OSError:
            continue
        chunks.append(lines)
        have += len(lines)
        if have >= n:
            break
    return [line for chunk in reversed(chunks) for line in chunk]


def record(entry):
    entry['finished'] = time.time()
    with _lock:
        _recent.append(entry)
        try:
            with settings.TP_BLOCKS.open('a') as f:
                f.write(json.dumps(entry) + '\n')
                full = settings.BLOCKS_LOG_MAX_MB > 0 and f.tell() >= settings.BLOCKS_LOG_MAX_MB * 2 ** 20
            if full:
                _rotate()
        except OSError:
            pass


def recent(n=60):
    with _lock:
        return list(_recent)[-n:]


def history(peer, limit=3000):
    '''This link's blocks from the log on disk, oldest first, so the history outlives a restart'''
    out = []
    for line in _tail(4 * limit):
        try:
            b = json.loads(line)
        except ValueError:
            continue
        if b.get('peer') == peer:
            out.append(b)
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
    lines = _tail(_recent.maxlen)
    with _lock:
        for line in lines:
            try:
                _recent.append(json.loads(line))
            except ValueError:
                pass


def add_sync(peer, point):
    with _lock:
        _sync.setdefault(peer, collections.deque(maxlen=2000)).append(point)


def sync(peer, n=720):
    with _lock:
        return list(_sync.get(peer, []))[-n:]
