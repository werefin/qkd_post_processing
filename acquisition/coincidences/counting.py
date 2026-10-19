'''
Counting over integration windows on one common clock; every click is counted exactly once however the stream
is chunked
'''

import numpy as np
from numba import njit

from .plan import HIST_BIN_PS, HIST_HALF_PS

PS_PER_S = 1_000_000_000_000


@njit(cache=True, nogil=True)
def _group_counts(ref, flat, starts, lo, hi, origin, period, n_win):
    '''Reference clicks with at least one click of every other member in [t + lo, t + hi), per window'''
    counts = np.zeros(n_win, np.int64)
    m = starts.size - 1
    ptr = starts[:-1].copy()
    for t in ref:
        hit = True
        for k in range(m):
            p, end = ptr[k], starts[k + 1]
            while p < end and flat[p] < t + lo:
                p += 1
            ptr[k] = p
            if p == end or flat[p] >= t + hi:
                hit = False
                break
        if hit:
            w = (t - origin) // period
            if 0 <= w < n_win:
                counts[w] += 1
    return counts


@njit(cache=True, nogil=True)
def _delay_histogram(ref, other, half_range, bin_ps, hist):
    '''Adds other minus reference, within the half range, to hist'''
    left = 0
    n = other.size
    for t in ref:
        lower = t - half_range
        while left < n and other[left] < lower:
            left += 1
        j = left
        while j < n and other[j] < lower + 2 * half_range:
            hist[(other[j] - lower) // bin_ps] += 1
            j += 1


class WindowCounter:
    '''Feed every channel's new clicks on the common clock, then advance to a time up to which every channel is
    complete; finished windows come back as (index, counts in Plan.counts order, duration in ps)'''

    def __init__(self, plan, origin_ps: int):
        self.plan, self.origin = plan, int(origin_ps)
        self.period = int(round(float(plan.opts['integration_s']) * PS_PER_S))
        self.done = self.origin
        self.buf = {l: np.empty(0, np.int64) for l in plan.labels}
        self.windows: dict = {}
        self.chunk = (np.zeros(len(plan.counts), np.int64), 0)  # counts and span of the last advance, for live rates
        self.pairs = [g for g in plan.groups if len(g['channels']) == 2]
        bins = 2 * HIST_HALF_PS // HIST_BIN_PS
        self.hist = {g['name']: np.zeros(bins, np.int64) for g in self.pairs}  # since the last window ended
        self.recent = {g['name']: np.zeros(bins, np.int64) for g in self.pairs}  # the window before

    def feed(self, label: str, ts: np.ndarray):
        if ts.size:
            self.buf[label] = np.concatenate([self.buf[label], ts.astype(np.int64)])

    def current(self) -> int:
        return (self.done - self.origin) // self.period

    def advance(self, horizon: int) -> list:
        '''Counts every click in [done, horizon) and returns the windows that ended'''
        horizon = int(horizon)
        if horizon <= self.done:
            self.chunk = (np.zeros(len(self.plan.counts), np.int64), 0)
            return []
        lo_t, hi_t = self.done, horizon
        k0 = (lo_t - self.origin) // self.period
        n = int((hi_t - 1 - self.origin) // self.period - k0 + 1)
        base = self.origin + k0 * self.period
        acc = np.zeros((n, len(self.plan.counts)), np.int64)
        part = {l: x[np.searchsorted(x, lo_t):np.searchsorted(x, hi_t)] for l, x in self.buf.items()}
        for i, l in enumerate(self.plan.labels):
            if part[l].size:
                acc[:, i] = np.bincount((part[l] - base) // self.period, minlength=n)
        col = len(self.plan.labels)
        for g in self.plan.groups:
            others = [self.buf[c] for c in g['channels'][1:]]
            flat = np.concatenate(others)
            starts = np.zeros(len(others) + 1, np.int64)
            starts[1:] = np.cumsum([m.size for m in others])
            for lo, hi in (self.plan.roi, self.plan.noise):
                acc[:, col] = _group_counts(part[g['channels'][0]], flat, starts, lo, hi, base, self.period, n)
                col += 1
        for g in self.pairs:
            _delay_histogram(part[g['channels'][0]], self.buf[g['channels'][1]], HIST_HALF_PS, HIST_BIN_PS, self.hist[g['name']])
        for i in range(n):
            self.windows[int(k0 + i)] = self.windows.get(int(k0 + i), 0) + acc[i]
        self.chunk = (acc.sum(axis=0), hi_t - lo_t)
        self.done = hi_t
        keep = hi_t - self.plan.reach
        for l, x in self.buf.items():
            self.buf[l] = x[np.searchsorted(x, keep):]
        done = [k for k in sorted(self.windows) if self.origin + (k + 1) * self.period <= self.done]
        if done:
            self.recent, self.hist = self.hist, {k: np.zeros_like(v) for k, v in self.hist.items()}
        return [(k, self.windows.pop(k), self.period) for k in done]

    def close(self) -> list:
        '''The window in progress, shorter than the rest, when the run stops'''
        return [(k, self.windows.pop(k), self.done - (self.origin + k * self.period)) for k in sorted(self.windows)]

    def histograms(self) -> dict:
        '''Delay histograms of the two-fold groups over this window and the one before, for the dashboard'''
        x = ((np.arange(2 * HIST_HALF_PS // HIST_BIN_PS) + 0.5) * HIST_BIN_PS - HIST_HALF_PS) / 1e3
        return {'x_ns': [round(float(v), 4) for v in x],
                'groups': {k: (self.hist[k] + self.recent[k]).tolist() for k in self.hist}}
