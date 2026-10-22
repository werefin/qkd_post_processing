'''
Stored recordings on the shared data volume: each is a timestamp file, a channel file and its metadata, named by
the time it started
'''

import collections
import os

from . import settings

SUFFIXES = ('_timestamp_sequence.npy', '_channel_sequence.npy', '_meta.json')


def names():
    return sorted(p.name for p in settings.RECORDINGS_DIR.glob('*_timestamp_sequence.npy'))


def base(ts_name):
    return ts_name.removesuffix('_timestamp_sequence.npy')


def group_paths(rec_base):
    return [p for suffix in SUFFIXES if (p := settings.RECORDINGS_DIR / f'{rec_base}{suffix}').is_file()]


def total_bytes():
    return sum(p.stat().st_size for p in settings.RECORDINGS_DIR.glob('*') if p.is_file())


def delete(rec_base):
    freed = 0
    for p in group_paths(rec_base):
        try:
            freed += p.stat().st_size
            p.unlink()
        except OSError:
            pass
    return freed


def enforce_cap():
    '''Opt-in (RECORDINGS_MAX_GB > 0): deletes the oldest recordings until back under the cap'''
    if settings.RECORDINGS_MAX_GB <= 0:
        return
    cap = settings.RECORDINGS_MAX_GB * 1024 ** 3
    for name in names():
        if total_bytes() <= cap:
            break
        delete(base(name))


def pairs():
    '''Recordings started together share their YYYYMMDD_HHMMSS prefix; those make a ready pair'''
    by_start = collections.defaultdict(list)
    for n in names():
        by_start[n[:15]].append(n)
    return [{'a': g[0], 'b': g[1], 'started': s} for s, g in sorted(by_start.items()) if len(g) == 2]


def resolve_pair(a=None, b=None):
    '''Validates a chosen pair of timestamp files; with none given, the two newest recordings'''
    have = names()
    if not a and not b:
        if len(have) < 2:
            raise ValueError(f'need 2 recordings, have {len(have)}')
        a, b = have[-2], have[-1]
    if a not in have or b not in have or a == b:
        raise ValueError('choose two different stored recordings')
    return settings.RECORDINGS_DIR / a, settings.RECORDINGS_DIR / b


def load_timestamps(path):
    import numpy as np
    arr = np.load(path, mmap_mode='r')
    if arr.ndim != 1 or not np.issubdtype(arr.dtype, np.integer) or arr.shape[0] < 2:
        raise ValueError(f'{path.name}: not a valid timestamp array')
    return arr


def link_into(dst, ts_name):
    '''Symlinks one recording's files into dst, the layout cmd/importtt reads'''
    for f in group_paths(base(ts_name)):
        os.symlink(f, dst / f.name)


def singles_per_s(ts_path):
    '''Singles per second of channels 1 to 4 over a recording, and its length in seconds'''
    import numpy as np
    ts = np.load(ts_path, mmap_mode='r')
    ch = np.load(str(ts_path).replace('_timestamp_sequence.npy', '_channel_sequence.npy'), mmap_mode='r')
    secs = max((int(ts[-1]) - int(ts[0])) / 1e12, 1e-9) if len(ts) > 1 else 0.0
    counts = np.bincount(np.asarray(ch, dtype=np.int64).clip(0, 8), minlength=9)[1:5]
    return [round(float(c) / secs, 1) if secs else 0.0 for c in counts], round(secs, 3)
