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
def _gallop(arr, x, start):
    '''First index from start on with arr[i] > x, searched outward from start, so a small move costs little'''
    n = arr.size
    if start >= n or arr[start] > x:
        return start
    lo, step = start, 1
    while lo + step < n and arr[lo + step] <= x:
        lo += step
        step *= 2
    hi = min(lo + step, n)
    lo += 1
    while lo < hi:
        mid = (lo + hi) // 2
        if arr[mid] <= x:
            lo = mid + 1
        else:
            hi = mid
    return lo


@njit(cache=True, nogil=True)
def _pair_pass(ref, flat, starts, lo, hi, nlo, nhi, half, bin_ps, origin, period, n_win, counts, hists):
    '''All two-fold partners of one reference channel: signal and noise ROI counts per window, as _group_counts
    gives them, and each partner's delay histogram. Driven by the partner clicks, so a dense reference channel
    is only searched, never walked: reference clicks t with a partner u in [t + lo, t + hi) are those in
    (u - hi, u - lo], each counted once as both bounds only grow with u'''
    los, his = np.array([lo, nlo]), np.array([hi, nhi])
    for k in range(starts.size - 1):
        for j in range(2):
            i0 = i1 = last = 0
            for p in range(starts[k], starts[k + 1]):
                u = flat[p]
                i0 = _gallop(ref, u - his[j], i0)
                i1 = _gallop(ref, u - los[j], max(i1, i0))
                for q in range(max(i0, last), i1):
                    w = (ref[q] - origin) // period
                    if 0 <= w < n_win:
                        counts[2 * k + j, w] += 1
                last = max(last, i1)
        # delay u - t in [-half, half): t in (u - half, u + half]
        i0 = i1 = 0
        for p in range(starts[k], starts[k + 1]):
            u = flat[p]
            i0 = _gallop(ref, u - half, i0)
            i1 = _gallop(ref, u + half, max(i1, i0))
            for q in range(i0, i1):
                hists[k, (u - ref[q] + half) // bin_ps] += 1


@njit(cache=True, nogil=True)
def partition(ch, ts, slot_of, n_slots):
    '''Timestamps grouped by channel in one pass, each group keeping its order: slot_of maps a channel number
    (offset by 128, channels being int8) to its slot or -1; returns the grouped timestamps and slot offsets'''
    sizes = np.zeros(n_slots + 1, np.int64)
    for c in ch:
        s = slot_of[c + 128]
        if s >= 0:
            sizes[s + 1] += 1
    offsets = np.cumsum(sizes)
    out = np.empty(offsets[-1], np.int64)
    fill = offsets[:-1].copy()
    for i in range(ch.size):
        s = slot_of[ch[i] + 128]
        if s >= 0:
            out[fill[s]] = ts[i]
            fill[s] += 1
    return out, offsets


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
        # two-fold groups by reference channel, counted in one pass each; larger groups one by one
        self.by_ref: dict = {}
        col = {g['name']: len(plan.labels) + 2 * i for i, g in enumerate(plan.groups)}
        for g in self.pairs:
            self.by_ref.setdefault(g['channels'][0], []).append((g['channels'][1], col[g['name']], g['name']))
        self.larger = [(g, col[g['name']]) for g in plan.groups if len(g['channels']) > 2]
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
        # singles per window from the window edges: the clicks are sorted, so a search finds each edge
        edges = base + self.period * np.arange(n + 1, dtype=np.int64)
        for i, l in enumerate(self.plan.labels):
            if part[l].size:
                acc[:, i] = np.diff(np.searchsorted(part[l], edges))
        bins = 2 * HIST_HALF_PS // HIST_BIN_PS
        for ref, partners in self.by_ref.items():
            others = [self.buf[o] for o, _, _ in partners]
            flat = np.concatenate(others)
            starts = np.zeros(len(others) + 1, np.int64)
            starts[1:] = np.cumsum([m.size for m in others])
            counts = np.zeros((2 * len(partners), n), np.int64)
            hists = np.zeros((len(partners), bins), np.int64)
            _pair_pass(part[ref], flat, starts, *self.plan.roi, *self.plan.noise, HIST_HALF_PS, HIST_BIN_PS,
                       base, self.period, n, counts, hists)
            for k, (_, col, name) in enumerate(partners):
                acc[:, col], acc[:, col + 1] = counts[2 * k], counts[2 * k + 1]
                self.hist[name] += hists[k]
        for g, col in self.larger:
            others = [self.buf[c] for c in g['channels'][1:]]
            flat = np.concatenate(others)
            starts = np.zeros(len(others) + 1, np.int64)
            starts[1:] = np.cumsum([m.size for m in others])
            for j, (lo, hi) in enumerate((self.plan.roi, self.plan.noise)):
                acc[:, col + j] = _group_counts(part[g['channels'][0]], flat, starts, lo, hi, base, self.period, n)
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
