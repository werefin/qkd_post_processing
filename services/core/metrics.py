'''
Numbers each service reports on /metrics: requests counted by the web base, and gauges each module registers for
its own state, under flat names
'''

import collections
import threading

_lock = threading.Lock()
_requests = collections.Counter()  # status class
_gauges = []                       # (name, function returning a number or {label value: number})


def count_request(status):
    with _lock:
        _requests[f'{status // 100}xx'] += 1


def gauge(name, fn):
    '''fn returns a number, or a mapping whose keys become name_key'''
    _gauges.append((name, fn))


def collect():
    with _lock:
        out = {f'http_requests_{k}': n for k, n in _requests.items()}
    for name, fn in _gauges:
        try:
            value = fn()
        except Exception:  # a broken gauge must not hide the others
            continue
        if isinstance(value, dict):
            out.update({f'{name}_{k}': v for k, v in value.items()})
        else:
            out[name] = value
    return {k: (None if v is None else round(float(v), 6)) for k, v in sorted(out.items())}
