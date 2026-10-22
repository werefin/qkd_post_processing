'''
Node's TimeTagger serves one mode at a time: a recording or replay, lab continuous QKD, a coincidence log or
the two-party recorder; each mode registers how to tell that it runs, and starts only under the lock
'''

import threading

lock = threading.Lock()
_modes = {}  # name: function, true while that mode holds the TimeTagger


def register(name, is_running):
    _modes[name] = is_running


def busy(*ignore):
    '''The mode holding the TimeTagger, or None; caller holds lock'''
    return next((name for name, fn in _modes.items() if name not in ignore and fn()), None)


def refusal(mode):
    return {'error': f'the TimeTagger is busy: {mode} is running'}
