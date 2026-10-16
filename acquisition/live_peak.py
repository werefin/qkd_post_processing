'''
Live coincidence-peak tracking: locks onto the peak (lag and clock skew) from the first seconds of the two
streams, then follows it chunk by chunk as detections arrive, refitting the skew and the peak width
'''

import argparse
import json
import math
import sys
import time
from collections import deque
from pathlib import Path
from typing import Any, Optional

import numpy as np
from numba import njit, prange
from scipy.optimize import curve_fit

import coincidence_peak as cp

PS_PER_NS = cp.PS_PER_NS
PS_PER_S = cp.PS_PER_S
LOCK_SCORE = 8.0       # lock threshold: the joint lag and skew search tries millions of cells, so 6 is not enough
CHUNK_SCORE = 3.0      # a chunk below this carries no usable centroid
LIVE_PREFIX = 'LIVE '  # stdout lines starting with this hold one JSON status for the dashboard


@njit(cache=True, nogil=True)
def _pair_residuals(a, b, lag_ps, slope_ps_per_s, ref_ps, radius_ps):
    '''A time and residual (b - a - lag(t)) of every pair within radius of the lag model, both arrays sorted'''
    n_b = len(b)
    count = 0
    left = 0
    for i in range(len(a)):
        lag = lag_ps + slope_ps_per_s * ((a[i] - ref_ps) / PS_PER_S)
        lower = a[i] + lag - radius_ps
        while left < n_b and b[left] < lower:
            left += 1
        j = left
        while j < n_b and b[j] < a[i] + lag + radius_ps:
            count += 1
            j += 1
    times = np.empty(count, np.float64)
    residuals = np.empty(count, np.float64)
    k = 0
    left = 0
    for i in range(len(a)):
        lag = lag_ps + slope_ps_per_s * ((a[i] - ref_ps) / PS_PER_S)
        lower = a[i] + lag - radius_ps
        while left < n_b and b[left] < lower:
            left += 1
        j = left
        while j < n_b and b[j] < a[i] + lag + radius_ps:
            times[k] = a[i]
            residuals[k] = (b[j] - a[i]) - lag
            k += 1
            j += 1
    return times, residuals


@njit(cache=True, nogil=True, parallel=True)
def _skew_scan(times, residuals, ref_ps, slopes, radius_ps, bin_ps, width_bins):
    '''For each trial slope, the fullest window of width_bins after removing that slope, and where it sits'''
    n_bins = int(2 * radius_ps // bin_ps)
    best = np.zeros(len(slopes), np.int64)
    where = np.zeros(len(slopes), np.int64)
    for s in prange(len(slopes)):
        hist = np.zeros(n_bins, np.int64)
        for k in range(len(times)):
            r = residuals[k] - slopes[s] * ((times[k] - ref_ps) / PS_PER_S)
            idx = int(math.floor((r + radius_ps) / bin_ps))
            if 0 <= idx < n_bins:
                hist[idx] += 1
        run = 0
        for i in range(width_bins):
            run += hist[i]
        top, at = run, 0
        for i in range(width_bins, n_bins):
            run += hist[i] - hist[i - width_bins]
            if run > top:
                top, at = run, i - width_bins + 1
        best[s] = top
        where[s] = at
    return best, where


def _gauss(x, amp, mu, sigma, bg):
    return bg + amp * np.exp(-0.5 * ((x - mu) / sigma) ** 2)


def _downsample(xs, ys, n=500):
    if len(xs) <= n:
        return [float(v) for v in xs], [float(v) for v in ys]
    idx, _ = cp_downsample_max(ys, n)
    return [float(xs[i]) for i in idx], [float(ys[i]) for i in idx]


def cp_downsample_max(values, n):
    '''Per-bucket max, so a narrow peak survives downsampling'''
    bucket = -(-len(values) // n)
    idx = [start + int(np.argmax(values[start:start + bucket])) for start in range(0, len(values), bucket)]
    return idx, [values[i] for i in idx]


class LiveTracker:
    '''Feed it both streams' new timestamps as they arrive, call step(), read status()'''

    def __init__(self, *, max_lag_ps=PS_PER_S, coarse_bin_ps=100 * PS_PER_NS, candidate_count=48,
                 slope_range_ppb=200.0, acquire_min_s=1.5, acquire_max_s=8.0, search_every_s=1.0,
                 chunk_s=0.5, track_radius_ps=5 * PS_PER_NS, track_bin_ps=20, fit_span_s=30.0,
                 hist_chunks=20, lost_after=6, history=600, max_memory_gib=4.0, **_ignored):
        self.max_lag_ps, self.coarse_bin_ps, self.candidate_count = int(max_lag_ps), int(coarse_bin_ps), int(candidate_count)
        self.slope_range = float(slope_range_ppb) * 1_000.0  # ps per s
        self.acquire_min_ps, self.acquire_max_ps = int(acquire_min_s * PS_PER_S), int(acquire_max_s * PS_PER_S)
        self.search_every_ps, self.chunk_ps = int(search_every_s * PS_PER_S), int(chunk_s * PS_PER_S)
        self.radius_ps, self.bin_ps = int(track_radius_ps), int(track_bin_ps)
        self.fit_span_ps, self.lost_after, self.max_memory_gib = int(fit_span_s * PS_PER_S), int(lost_after), float(max_memory_gib)
        self.a0: Optional[int] = None
        self.b0: Optional[int] = None
        self.a = np.empty(0, np.int64)
        self.b = np.empty(0, np.int64)
        self.n_a = self.n_b = 0
        self.points: deque = deque(maxlen=history)  # (t_ps, lag_ps, err_ps, rate, score)
        self.pairs: deque = deque(maxlen=int(hist_chunks))  # (A time, B minus A) of each recent chunk's pairs
        self.search: dict[str, Any] = {}
        self.state = 'waiting'
        self.events: list[str] = []
        self._reset_lock()

    def _reset_lock(self):
        self.lag = self.slope = self.slope_err = 0.0
        self.ref = 0
        self.sigma = 250.0
        self.done = 0
        self.misses = 0
        self.last_search = None
        self.points.clear()
        self.pairs.clear()

    # Input

    def feed(self, a_parts, b_parts):
        '''Appends sorted new timestamps of A and B; times are kept relative to each stream's first one'''
        a_new = np.concatenate(a_parts).astype(np.int64) if a_parts else np.empty(0, np.int64)
        b_new = np.concatenate(b_parts).astype(np.int64) if b_parts else np.empty(0, np.int64)
        if self.a0 is None and a_new.size:
            self.a0 = int(a_new[0])
        if self.b0 is None and b_new.size:
            self.b0 = int(b_new[0])
        if a_new.size:
            self.a = np.concatenate([self.a, a_new - self.a0])
            self.n_a += a_new.size
        if b_new.size:
            self.b = np.concatenate([self.b, b_new - self.b0])
            self.n_b += b_new.size

    def lag_at(self, t_ps):
        return self.lag + self.slope * ((t_ps - self.ref) / PS_PER_S)

    # Acquisition: coarse FFT for lag candidates, then a joint lag and skew search on each

    def _acquire(self):
        a, b = self.a, self.b
        if a.size < 2 or b.size < 2 or min(a[-1] - a[0], b[-1] - b[0]) < self.acquire_min_ps:
            return
        end = int(a[-1])
        if self.last_search is not None and end - self.last_search < self.search_every_ps:
            return
        self.last_search = end
        coarse = cp._coarse_search(a, b, self.coarse_bin_ps, self.max_lag_ps, self.candidate_count,
                                   self.max_memory_gib, quiet=True)
        span = int(a[-1] - a[0])
        ref = int(a[0]) + span // 2
        # every candidate's pairs must include the smear of the largest skew over the window
        radius = 2 * coarse['coarse_bin_ps'] + int(self.slope_range * span / PS_PER_S / 2)
        # a 20 ns pass ranks every candidate cheaply; only the best few get the full scan
        ranked = []
        for cand, cscore in zip(coarse['candidate_lags_ps'], coarse['candidate_scores']):
            lag0 = float(cand)  # relative B minus A, both streams counted from their first timestamp
            times, res = _pair_residuals(a, b, lag0, 0.0, ref, radius)
            if times.size >= 10:
                pre = self._scan(times, res, ref, radius, span, passes=((20 * PS_PER_NS, 40 * PS_PER_NS),))
                ranked.append((pre['score'], lag0, cscore, times, res))
        ranked.sort(key=lambda item: -item[0])
        best = None
        for _, lag0, cscore, times, res in ranked[:4]:
            found = self._scan(times, res, ref, radius, span)
            if found and (best is None or found['score'] > best['score']):
                best = {**found, 'lag0': lag0, 'coarse_score': cscore}
        xs, ys = _downsample(np.asarray(coarse['plot_lags_ps']) / 1e9, np.asarray(coarse['plot_scores']))
        self.search = {'span_s': span / PS_PER_S, 'coarse': {'x_ms': xs, 'y': ys},
                       'best_score': None if best is None else best['score'], 'lock_score': LOCK_SCORE}
        if best is None or best['score'] < LOCK_SCORE:
            if a[-1] - a[0] > self.acquire_max_ps:  # slide: keep the trailing acquisition window
                self.a = a[np.searchsorted(a, a[-1] - self.acquire_max_ps):]
                self.b = b[np.searchsorted(b, b[-1] - self.acquire_max_ps):]
            return
        self.lag, self.slope, self.ref = best['lag0'] + best['offset'], best['slope'], ref
        self.sigma = max(best['width'] / 2.355, 2 * self.bin_ps)
        self.done = int(a[0])
        self.state = 'locked'
        self.events.append(f'locked after {self.elapsed_s():.1f}s of data: lag {self.lag / 1e3:.3f} ns, '
                           f'skew {self.slope / 1e3:.2f} ppb, score {best["score"]:.1f}')

    def _scan(self, times, res, ref, radius, span, passes=((2 * PS_PER_NS, 4 * PS_PER_NS), (100, 600))):
        '''Coarse then fine trial slopes; returns the best slope, its window offset and score'''
        base, lo, hi, found = 0.0, -self.slope_range, self.slope_range, None
        for bin_ps, width_ps in passes:
            # slope steps small enough that the smear over the window stays within half a bin
            step = max(1.0, bin_ps * PS_PER_S / max(span, 1) / 2)
            slopes = np.arange(lo, hi + step, step)
            width_bins = max(1, width_ps // bin_ps)
            counts, where = _skew_scan(times, res - base, ref, slopes, radius, bin_ps, width_bins)
            i = int(np.argmax(counts))
            bg = max(times.size * width_bins / (2 * radius // bin_ps), 1.0)
            offset = base - radius + (where[i] + width_bins / 2) * bin_ps
            found = {'slope': float(slopes[i]), 'offset': float(offset), 'width': width_ps,
                     'score': float((counts[i] - bg) / math.sqrt(bg))}
            # the fine pass looks only near the coarse line
            base, lo, hi = offset, slopes[i] - 2 * step, slopes[i] + 2 * step
            radius = 10 * PS_PER_NS
            keep = np.abs(res - base - slopes[i] * ((times - ref) / PS_PER_S)) < radius
            times, res = times[keep], res[keep]
            if times.size < 10:
                return found
        return found

    # Tracking: one centroid per chunk, a weighted line through the recent ones

    def _track(self):
        while True:
            t0, t1 = self.done, self.done + self.chunk_ps
            if self.a.size == 0 or self.a[-1] < t1 or self.b.size == 0 or self.b[-1] < t1 + self.lag_at(t1) + self.radius_ps:
                break
            a_c = self.a[np.searchsorted(self.a, t0):np.searchsorted(self.a, t1)]
            b_lo = np.searchsorted(self.b, t0 + self.lag_at(t0) - self.radius_ps - 1)
            b_hi = np.searchsorted(self.b, t1 + self.lag_at(t1) + self.radius_ps + 1)
            times, res = _pair_residuals(a_c, self.b[b_lo:b_hi], self.lag, self.slope, self.ref, self.radius_ps)
            self.pairs.append((times, res + self.lag_at(times)))
            self._chunk_point(t0, t1, res, a_c.size)
            self.done = t1
        if self.misses >= self.lost_after:
            self.events.append(f'lost the peak at {self.elapsed_s():.1f}s, searching again')
            self.state = 'searching'
            self._reset_lock()

    def _chunk_point(self, t0, t1, res, n_a):
        half = max(3 * self.sigma, 3 * self.bin_ps)
        central = np.abs(res) < half
        side = np.abs(res) > self.radius_ps / 2
        bg = side.sum() * (2 * half) / self.radius_ps  # sidebands cover half the range
        n = int(central.sum())
        excess = n - bg
        score = excess / math.sqrt(max(bg, 1.0))
        dur_s = (t1 - t0) / PS_PER_S
        if score < CHUNK_SCORE or excess <= 0:
            self.misses += 1
            self.points.append(((t0 + t1) / 2, None, None, 0.0, float(score)))
            return
        self.misses = 0
        # background-subtracted centroid: accidentals are flat, so their mean residual is zero
        offset = float(res[central].sum()) / excess
        mid = (t0 + t1) / 2
        err = max(self.sigma, self.bin_ps) / math.sqrt(excess)
        self.points.append((mid, self.lag_at(mid) + offset, err, excess / dur_s, float(score)))
        self._refit(mid)

    def _refit(self, now):
        pts = [p for p in self.points if p[1] is not None and now - p[0] <= self.fit_span_ps]
        if len(pts) < 3:
            t, lag, _, _, _ = pts[-1]
            self.lag, self.ref = lag, int(t)
            return
        t = np.array([p[0] for p in pts])
        y = np.array([p[1] for p in pts])
        w = 1 / np.array([p[2] for p in pts])
        x = (t - now) / PS_PER_S
        (slope, lag), cov = np.polyfit(x, y, 1, w=w, cov='unscaled')
        self.slope, self.lag, self.ref = float(slope), float(lag), int(now)
        self.slope_err = float(math.sqrt(max(cov[0, 0], 0)))
        acc = self.histogram()
        x_ps = -self.radius_ps + (np.arange(acc.size) + 0.5) * self.bin_ps
        try:
            bg0 = float(np.median(acc))
            p, _ = curve_fit(_gauss, x_ps, acc, p0=(max(acc.max() - bg0, 1), 0.0, self.sigma, bg0), maxfev=2000)
            if 0 < abs(p[2]) < self.radius_ps / 2:
                self.sigma = float(abs(p[2]))
        except (RuntimeError, ValueError):
            pass

    # Driver and status

    def histogram(self):
        '''Recent pairs binned against the current lag model, so the peak shows as the clocks are now corrected'''
        n_bins = 2 * self.radius_ps // self.bin_ps
        if not self.pairs:
            return np.zeros(n_bins, np.int64)
        res = np.concatenate([d - self.lag_at(t) for t, d in self.pairs])
        return np.histogram(res, bins=n_bins, range=(-self.radius_ps, self.radius_ps))[0]

    def step(self):
        if self.state == 'waiting' and self.a.size and self.b.size:
            self.state = 'searching'
        if self.state == 'searching':
            self._acquire()
        if self.state == 'locked':
            self._track()
            # keep only what the next chunks need
            self.a = self.a[np.searchsorted(self.a, self.done):]
            self.b = self.b[np.searchsorted(self.b, self.done + self.lag_at(self.done) - 2 * self.radius_ps):]

    def elapsed_s(self):
        return 0.0 if self.a.size == 0 else float(self.a[-1]) / PS_PER_S

    def status(self) -> dict[str, Any]:
        el = self.elapsed_s()
        out: dict[str, Any] = {
            'state': self.state, 'elapsed_s': el, 'n_a': self.n_a, 'n_b': self.n_b,
            'rate_a': self.n_a / el if el > 0 else 0.0, 'rate_b': self.n_b / el if el > 0 else 0.0,
            'events': self.events[-20:], 'search': self.search,
        }
        if self.state != 'locked':
            return out
        hwhm = self.sigma * math.sqrt(2 * math.log(2))
        acc = self.histogram()
        x_ns = (-self.radius_ps + (np.arange(acc.size) + 0.5) * self.bin_ps) / 1e3
        side = np.abs(x_ns * 1e3) > self.radius_ps / 2
        bg_bin = float(acc[side].mean()) if side.any() else 0.0
        inside = np.abs(x_ns * 1e3) <= hwhm
        n_in = float(acc[inside].sum())
        good = [p for p in self.points if p[1] is not None]
        now = self.done
        delay = (self.b0 - self.a0) + self.lag_at(now) if self.a0 is not None else None
        out.update({
            'lag_ns': self.lag_at(now) / 1e3, 'delay_b_minus_a_ps': None if delay is None else int(round(delay)),
            'skew_ppb': self.slope / 1e3, 'skew_err_ppb': self.slope_err / 1e3,
            'sigma_ps': self.sigma, 'hwhm_ps': hwhm,
            'pair_rate': float(np.mean([p[3] for p in good[-10:]])) if good else 0.0,
            'accidental_share': (bg_bin * inside.sum() / n_in) if n_in > 0 else None,
            'chunk_score': self.points[-1][4] if self.points else None,
            'tracked_s': (now - (self.points[0][0] if self.points else now)) / PS_PER_S,
            'histogram': {'x_ns': [float(v) for v in x_ns], 'y': [float(v) for v in acc], 'hwhm_ns': hwhm / 1e3,
                          'bg': bg_bin},
            'history': {
                't_s': [p[0] / PS_PER_S for p in good],
                # residual against the current line, so drift and jitter of the clocks show directly
                'residual_ps': [p[1] - self.lag_at(p[0]) for p in good],
                'err_ps': [p[2] for p in good], 'rate': [p[3] for p in good], 'score': [p[4] for p in good],
            },
        })
        return out


def tracker_kwargs(cfg: dict) -> dict:
    '''LiveTracker settings from config.json: the search from "coincidence_peak", the cadence from "live_coincidence"'''
    cpk, live = cfg.get('coincidence_peak', {}), cfg.get('live_coincidence', {})
    kw = {
        'max_lag_ps': int(round(cpk.get('max_lag_ms', 1000.0) * 1_000_000_000)),
        'coarse_bin_ps': int(round(cpk.get('coarse_bin_ns', 100.0) * PS_PER_NS)),
        'slope_range_ppb': float(cpk.get('slope_range_ppb', 200.0)),
        'max_memory_gib': float(cpk.get('max_memory_gib', 4.0)),
        # window_s is the trailing data the search looks at; interval_s is only how often a status is shown
        'acquire_max_s': float(live.get('window_s', 30.0)),
    }
    for key in ('chunk_s', 'acquire_min_s', 'search_every_s', 'fit_span_s', 'lost_after', 'candidate_count'):
        if key in live:
            kw[key] = live[key]
    if 'track_radius_ns' in live:
        kw['track_radius_ps'] = int(round(live['track_radius_ns'] * PS_PER_NS))
    return kw


def emit(status: dict) -> None:
    '''One status line for the dashboard, which reads them from this process's output'''
    print(LIVE_PREFIX + json.dumps(status, separators=(',', ':')), flush=True)


def summary(st):
    if st['state'] != 'locked':
        best = st['search'].get('best_score')
        return f"{st['state']}: {st['elapsed_s']:.1f}s of data" + ('' if best is None else f', best score {best:.1f} (lock at {LOCK_SCORE:g})')
    return (f"locked: lag {st['lag_ns']:.3f} ns, skew {st['skew_ppb']:.2f} +- {st['skew_err_ppb']:.2f} ppb, "
            f"HWHM {st['hwhm_ps']:.0f} ps, {st['pair_rate']:.0f} pairs/s, chunk score {st['chunk_score']:.1f}")


def replay(path_a: Path, path_b: Path, speed: float, feed_s: float, json_out: bool, kwargs: dict) -> dict:
    '''Plays two recordings into the tracker as if they were streaming, speed 0 meaning as fast as possible'''
    a, b = np.load(path_a, mmap_mode='r'), np.load(path_b, mmap_mode='r')
    tracker = LiveTracker(**kwargs)
    feed_ps = int(feed_s * PS_PER_S)
    ia = ib = 0
    t = 0
    started = time.monotonic()
    seen = 0
    while ia < len(a) or ib < len(b):
        t += feed_ps
        ja = int(np.searchsorted(a, int(a[0]) + t))
        jb = int(np.searchsorted(b, int(b[0]) + t))
        tracker.feed([np.asarray(a[ia:ja])], [np.asarray(b[ib:jb])])
        ia, ib = ja, jb
        if speed > 0:
            time.sleep(max(0.0, started + t / PS_PER_S / speed - time.monotonic()))
        tracker.step()
        st = tracker.status()
        for e in tracker.events[seen:]:
            cp.log('INFO', f'[live] {e}')
        seen = len(tracker.events)
        if json_out:
            emit(st)
        cp.log('INFO', f'[live] {summary(st)}')
    return tracker.status()


def main(argv: Optional[list] = None) -> int:
    parser = argparse.ArgumentParser(description='Replays two recordings through the live coincidence-peak tracker')
    parser.add_argument('a', type=Path, help='A timestamp_sequence.npy')
    parser.add_argument('b', type=Path, help='B timestamp_sequence.npy')
    parser.add_argument('--speed', type=float, default=1.0, help='playback speed, 0 for as fast as possible')
    parser.add_argument('--feed-s', type=float, default=0.25, help='data handed to the tracker per update')
    parser.add_argument('--config', type=Path, default=Path(__file__).with_name('config.json'),
                        help='config.json whose coincidence_peak and live_coincidence sections set the tracker')
    parser.add_argument('--json', action='store_true', help='also print one LIVE JSON status per update')
    args = parser.parse_args(argv)
    cfg = json.loads(args.config.read_text()) if args.config.is_file() else {}
    try:
        st = replay(args.a, args.b, args.speed, args.feed_s, args.json, tracker_kwargs(cfg))
    except KeyboardInterrupt:  # the dashboard's Stop
        cp.log('INFO', '[live] replay stopped')
        return 130
    return 0 if st['state'] == 'locked' else 1


if __name__ == '__main__':
    sys.exit(main())
