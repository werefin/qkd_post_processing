'''
Real-time clock tracking between two sites: a Kalman filter on the delay between the clocks (Bob's minus Alice's,
on Alice's clock), its rate (the skew) and the rate's own drift, fed with the coincidence peak's centre measured
every slice of data; it predicts where the next segment's peak sits and with what uncertainty, refuses centres
that do not fit, and says how well the clocks are held
'''

import math

import numpy as np
from two_party_stream import near

PS_PER_S = 1_000_000_000_000
SLICE_PS = 250_000_000_000   # one centre per quarter second of data, as importtt's drift filter
PAIR_RADIUS_PS = 3_000       # residuals searched around the model, as importtt's drift filter
CENTRE_SCORE = 3.0           # a slice below this peak-over-background score carries no usable centre
GATE = 25.0                  # a centre more than five standard deviations off the prediction is refused
LOST_AFTER = 8               # refused or empty slices in a row before the lock counts as lost
# drift of the skew: a random walk of this spectral density, in ps^2/s^5; temperature swings move the skew by
# a few ppb over minutes, which this follows without chasing the noise of single slices (test_clock_tracker.py)
SKEW_DRIFT_Q = 0.1


class ClockTracker:
    '''State x = [delay (ps), skew (ps/s), skew drift (ps/s^2)] at time t (ps on Alice's clock)'''

    def __init__(self, t, delay, skew, delay_sd=200.0, skew_sd=2_000.0, drift_sd=50.0, q=SKEW_DRIFT_Q):
        self.t = int(t)
        self.x = np.array([float(delay), float(skew), 0.0])
        self.P = np.diag([delay_sd ** 2, skew_sd ** 2, drift_sd ** 2])
        self.q = q
        self.misses = 0
        self.lost = False

    @classmethod
    def from_model(cls, model: dict, **kw):
        '''From a clock model of two_party_blocks (delay_ref at t_ref, slope), as a full search gives it'''
        return cls(model['t_ref'], model['delay_ref'], model['slope'], **kw)

    def _step(self, t):
        dt = (int(t) - self.t) / PS_PER_S
        F = np.array([[1.0, dt, dt * dt / 2], [0.0, 1.0, dt], [0.0, 0.0, 1.0]])
        a = abs(dt)
        Q = self.q * np.array([[a ** 5 / 20, a ** 4 / 8, a ** 3 / 6], [a ** 4 / 8, a ** 3 / 3, a ** 2 / 2], [a ** 3 / 6, a ** 2 / 2, a]])
        return F @ self.x, F @ self.P @ F.T + Q

    def predict(self, t):
        '''(delay, its standard deviation) at time t'''
        x, P = self._step(t)
        return float(x[0]), math.sqrt(max(P[0, 0], 0.0))

    def model_at(self, t) -> dict:
        '''The prediction as a clock model of two_party_blocks, linear around time t'''
        x, _ = self._step(t)
        return {'delay_ref': int(round(x[0])), 't_ref': int(t), 'slope': float(x[1])}

    def update(self, t, delay, sd) -> bool:
        '''One measured centre; False when it does not fit the prediction and is refused'''
        x, P = self._step(t)
        s = P[0, 0] + sd * sd
        innovation = float(delay) - x[0]
        if innovation * innovation / s > GATE:
            self.misses += 1
            self.lost = self.misses >= LOST_AFTER
            return False
        k = P[:, 0] / s
        self.x = x + k * innovation
        self.P = P - np.outer(k, P[0, :])
        self.t = int(t)
        self.misses = 0
        return True

    def miss(self):
        self.misses += 1
        self.lost = self.misses >= LOST_AFTER

    def feed(self, centres) -> dict:
        '''A segment's centres, (t, delay, sd) or None for a slice without a usable peak; returns how well the
        clocks were held over them'''
        used, residuals = 0, []
        for c in centres:
            if c is None:
                self.miss()
            elif self.update(*c):
                used += 1
                residuals.append(c[1] - self.predict(c[0])[0])
        return {
            'slices': len(centres), 'locked_slices': used,
            'residual_rms_ps': round(float(np.sqrt(np.mean(np.square(residuals)))), 1) if residuals else None,
            'delay_sd_ps': round(math.sqrt(max(self.P[0, 0], 0.0)), 1),
            'skew_ppb': round(self.x[1] / 1_000.0, 3), 'skew_sd_ppb': round(math.sqrt(max(self.P[1, 1], 0.0)) / 1_000.0, 3),
            'skew_drift_ppb_per_min': round(self.x[2] * 60 / 1_000.0, 3), 'lost': self.lost,
        }

    def to_dict(self) -> dict:
        '''Kept between segments and blocks; its model keys let the status read the skew as before'''
        return {**self.model_at(self.t), 'kf': {'t': self.t, 'x': self.x.tolist(), 'P': self.P.tolist(), 'q': self.q}}

    @classmethod
    def from_dict(cls, d: dict):
        kf = d['kf']
        tr = cls(kf['t'], 0.0, 0.0, q=kf.get('q', SKEW_DRIFT_Q))
        tr.x, tr.P = np.array(kf['x'], float), np.array(kf['P'], float)
        return tr


def slice_centres(a, b_on_a, b_raw, slice_ps=SLICE_PS, radius_ps=PAIR_RADIUS_PS):
    '''Peak's centre, Bob's raw time minus Alice's, in every slice of a segment: each announced click paired
    with Bob's nearest one on Alice's clock, accidentals taken out with the far residuals. Returns, per slice,
    (time, delay, its sd) or None, and the peak's width; the time is the mean of the slice's pairs, since the
    clocks drift by nanoseconds within a slice'''
    if a.size == 0 or b_on_a.size == 0:
        return [], None
    # only announced clicks with a pair candidate in reach can count, and finding them first spares the rest
    first, last = int(a[0]), int(a[-1])
    a = a[near(a, b_on_a, radius_ps)]
    if a.size == 0:
        return [], None
    j = np.clip(np.searchsorted(b_on_a, a), 1, b_on_a.size - 1) if b_on_a.size > 1 else np.zeros(a.size, int)
    if b_on_a.size > 1:
        j = np.where(np.abs(b_on_a[j - 1] - a) < np.abs(b_on_a[j] - a), j - 1, j)
    r = b_on_a[j] - a
    close = np.abs(r) <= radius_ps
    t, r, d = a[close], r[close].astype(np.float64), (b_raw[j] - a)[close].astype(np.float64)
    mapping = d - r  # Bob's raw time minus his time on Alice's clock: linear in time, the model's own delay
    if t.size < 10:
        return [], None
    core = np.abs(r) < radius_ps / 2
    sigma = max(1.4826 * float(np.median(np.abs(r[core] - np.median(r[core])))) if core.any() else 0.0, 20.0)
    half = max(3 * sigma, 150.0)
    side_lo = radius_ps / 2
    out = []
    # whole slices only: the sliver a segment ends on holds too few pairs and would always count as a miss
    for k in range(max(int((last - first) // slice_ps), 1)):
        lo, hi = first + k * slice_ps, first + (k + 1) * slice_ps
        sel = (t >= lo) & (t < hi)
        rs, ms, ts = r[sel], mapping[sel], t[sel]
        central = np.abs(rs) < half
        bg = np.count_nonzero(np.abs(rs) > side_lo) * (2 * half) / (2 * (radius_ps - side_lo))
        excess = central.sum() - bg
        if excess <= 0 or excess / math.sqrt(max(bg, 1.0)) < CENTRE_SCORE:
            out.append(None)
            continue
        # the model's delay at the pairs' mean time, plus the background-subtracted mean residual around it
        t_c = int(lo + np.mean(ts[central] - lo))
        delay = float(np.mean(ms[central])) + float(rs[central].sum()) / excess
        out.append((t_c, delay, sigma / math.sqrt(excess)))
    return out, sigma
