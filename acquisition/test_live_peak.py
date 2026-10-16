'''
Live tracker tests: synthetic streams with a known lag and clock skew, and the example recording when present
run: python3 -m unittest acquisition/test_live_peak.py
'''

import glob
import os
import sys
import unittest

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import live_peak as lp  # noqa: E402

PS = lp.PS_PER_S


def synthetic(seconds, rate_a, rate_b, pair_rate, lag_ps, skew_ppb, sigma_ps, seed=1):
    '''Two click streams: uniform singles plus pairs whose B time is A time + lag + skew * t + jitter'''
    rng = np.random.default_rng(seed)
    pairs = np.sort(rng.uniform(0, seconds * PS, int(pair_rate * seconds)))
    a = np.concatenate([rng.uniform(0, seconds * PS, int(rate_a * seconds)), pairs])
    b_pairs = pairs + lag_ps + skew_ppb * 1e3 * (pairs / PS) + rng.normal(0, sigma_ps, pairs.size)
    b = np.concatenate([rng.uniform(lag_ps, seconds * PS + lag_ps, int(rate_b * seconds)), b_pairs])
    # arbitrary, very different clock origins, as two independent TimeTaggers have
    return np.sort(a).astype(np.int64) + 33_660_080_291_993_502, np.sort(b).astype(np.int64) + 2_239_056_212_085_936_586


def play(tracker, a, b, feed_s=0.25):
    t, ia, ib = 0, 0, 0
    while ia < len(a) or ib < len(b):
        t += int(feed_s * PS)
        ja, jb = int(np.searchsorted(a, a[0] + t)), int(np.searchsorted(b, b[0] + t))
        tracker.feed([a[ia:ja]], [b[ib:jb]])
        ia, ib = ja, jb
        tracker.step()
    return tracker.status()


class LiveTrackerTest(unittest.TestCase):
    def test_locks_and_recovers_lag_and_skew(self):
        a, b = synthetic(6, 200_000, 150_000, 400, lag_ps=3_200_000_000, skew_ppb=-42.0, sigma_ps=200)
        st = play(lp.LiveTracker(), a, b)
        self.assertEqual(st['state'], 'locked')
        self.assertAlmostEqual(st['skew_ppb'], -42.0, delta=0.5)
        self.assertAlmostEqual(st['sigma_ps'], 200, delta=40)
        # relative lag at the last processed chunk: lag + skew * t, both clocks counted from their first click
        t_now = st['history']['t_s'][-1] + 0.25
        origin = (int(b[0]) - 2_239_056_212_085_936_586) - (int(a[0]) - 33_660_080_291_993_502)
        expected = 3_200_000_000 - 42.0 * 1e3 * t_now - origin
        self.assertLess(abs(st['lag_ns'] * 1e3 - expected), 1_000)

    def test_no_lock_without_pairs(self):
        a, b = synthetic(4, 200_000, 150_000, 0, lag_ps=0, skew_ppb=0, sigma_ps=200)
        st = play(lp.LiveTracker(), a, b)
        self.assertEqual(st['state'], 'searching')

    def test_config_mapping(self):
        kw = lp.tracker_kwargs({'coincidence_peak': {'max_lag_ms': 500, 'coarse_bin_ns': 50, 'slope_range_ppb': 100},
                                'live_coincidence': {'interval_s': 5, 'window_s': 12, 'chunk_s': 0.25}})
        tracker = lp.LiveTracker(**kw)
        self.assertEqual(tracker.max_lag_ps, 500 * 10**9)
        self.assertEqual(tracker.coarse_bin_ps, 50_000)
        self.assertEqual(tracker.acquire_max_ps, 12 * PS)
        self.assertEqual(tracker.chunk_ps, PS // 4)

    @unittest.skipUnless(glob.glob(os.path.join(os.path.dirname(__file__), 'example', '*', '*_timestamp_sequence.npy')),
                         'example recording not present')
    def test_example_recording(self):
        files = sorted(glob.glob(os.path.join(os.path.dirname(__file__), 'example', '20260909', '*_timestamp_sequence.npy')))
        st = play(lp.LiveTracker(), np.load(files[0]), np.load(files[1]))
        self.assertEqual(st['state'], 'locked')
        self.assertAlmostEqual(st['lag_ns'] / 1e6, 13.544, delta=0.001)
        self.assertAlmostEqual(st['skew_ppb'], 19.1, delta=0.2)


if __name__ == '__main__':
    unittest.main()
