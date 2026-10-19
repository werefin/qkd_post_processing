'''
Clock alignment of the second TimeTagger onto the first: none for one TimeTagger, else the live tracker
locking onto the coincidence peak while the data arrives
'''

import numpy as np

PS_PER_S = 1_000_000_000_000


class OneClock:
    description = 'one TimeTagger, one clock'
    ready = ok = True

    def update(self, data):
        pass

    def to_a(self, ts):
        return ts

    def status(self):
        return None


class LiveModel:
    '''live_peak.LiveTracker fed with every click; counting starts at the lock, the last model bridging a loss'''
    description = 'B on A\'s clock by the live tracker\'s lag and skew model (live_peak.py), refitted as data arrives'

    def __init__(self, tracker_kwargs: dict):
        from live_peak import LiveTracker
        self.tracker = LiveTracker(**tracker_kwargs)
        self.model, self.st = None, {'state': 'waiting'}
        self.ready = self.ok = False

    def update(self, data):
        a, b = data[0][1], data[1][1]
        self.tracker.feed([a] if a.size else [], [b] if b.size else [])
        try:
            self.tracker.step()
        except Exception:  # a failed update retries with the next data
            pass
        self.st = self.tracker.status()
        self.ok = self.st['state'] == 'locked' and self.st.get('model') is not None
        if self.ok:
            self.model = self.st['model']
        self.ready = self.model is not None

    def to_a(self, ts):
        # subtract the B minus A delay the model predicts at that time
        d, t_ref, slope = self.model['delay_b_minus_a_ps'], self.model['t_ref_ps'], self.model['slope_ps_per_s']
        return ts - d - np.round(slope * ((ts - t_ref - d).astype(np.float64) / PS_PER_S)).astype(np.int64)

    def status(self):
        return {k: self.st.get(k) for k in ('state', 'skew_ppb', 'hwhm_ps')}
