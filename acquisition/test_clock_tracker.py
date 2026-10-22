'''
Clock tracker: follows a clock delay whose skew changes over time better than a straight line per segment,
refuses peak centres that do not fit, survives a restart, and reads the peak centres off a segment's clicks
run: python3 -m unittest acquisition/test_clock_tracker.py
'''

import os
import sys
import unittest

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import clock_tracker as ct  # noqa: E402
import test_two_party_blocks as tpt  # noqa: E402
import two_party_blocks as tp  # noqa: E402
import two_party_stream as st  # noqa: E402

PS = ct.PS_PER_S
SEGMENT_S = 5.0
SLICE_S = 0.25


def drift(t_s):
    '''A delay whose skew climbs from 31 to 41 ppb over ten minutes, as a warming fibre or oscillator does,
    with a slow wobble on top'''
    return 37e6 + 31e3 * t_s + 0.5 * (10e3 / 600) * t_s ** 2 + 40 * np.sin(2 * np.pi * t_s / 180)


class TrackerTest(unittest.TestCase):
    def test_follows_changing_skew_better_than_a_line_per_segment(self):
        rng = np.random.default_rng(3)
        noise_ps = 25.0
        t = np.arange(0, 600, SLICE_S) + SLICE_S / 2
        meas = drift(t) + rng.normal(0, noise_ps, t.size)
        tracker = ct.ClockTracker(int(t[0] * PS), meas[0], 31e3)
        per_seg = int(SEGMENT_S / SLICE_S)
        kf_err, line_err = [], []
        for k in range(t.size // per_seg - 1):
            seg = slice(k * per_seg, (k + 1) * per_seg)
            nxt = slice((k + 1) * per_seg, (k + 2) * per_seg)
            tracker.feed([(int(ti * PS), float(m), noise_ps) for ti, m in zip(t[seg], meas[seg])])
            if k < 12:
                continue  # both settle in the first minute
            # each predicts the whole next segment: the tracker from everything so far, the line from this segment
            slope, icpt = np.polyfit(t[seg], meas[seg], 1)
            kf_err += [tracker.predict(int(ti * PS))[0] - drift(ti) for ti in t[nxt]]
            line_err += list(slope * t[nxt] + icpt - drift(t[nxt]))
        kf_rms, line_rms = np.sqrt(np.mean(np.square(kf_err))), np.sqrt(np.mean(np.square(line_err)))
        self.assertLess(kf_rms, 20.0)            # under the 25 ps noise of a single centre
        self.assertLess(kf_rms, line_rms / 5)    # and several times closer than a line per segment
        self.assertFalse(tracker.lost)

    def test_refuses_centres_that_do_not_fit_and_loses_lock(self):
        tracker = ct.ClockTracker(0, 0.0, 0.0, delay_sd=20.0, skew_sd=10.0)
        self.assertTrue(tracker.update(int(0.25 * PS), 5.0, 20.0))
        self.assertFalse(tracker.update(int(0.5 * PS), 2_000.0, 20.0))  # 2 ns off: another peak, or none
        for k in range(ct.LOST_AFTER):
            tracker.miss()
        self.assertTrue(tracker.lost)

    def test_state_survives_a_restart(self):
        tracker = ct.ClockTracker(10**15, 37e6, 31e3)
        tracker.feed([(10**15 + k * ct.SLICE_PS, 37e6 + 31e3 * k * SLICE_S, 20.0) for k in range(40)])
        again = ct.ClockTracker.from_dict(tracker.to_dict())
        t = 10**15 + 20 * PS
        self.assertAlmostEqual(again.predict(t)[0], tracker.predict(t)[0], places=6)
        model = tracker.model_at(t)
        self.assertAlmostEqual(model['slope'], 31e3, delta=50)

    def test_slice_centres_read_the_known_delay(self):
        (a, cha, oa), (b, chb, ob), _ = tpt.two_sites(seconds=20)
        report, keep, model, _ = tp.align_arrays(a, b, None, {'slope_range_ppb': 200.0})
        pb = b[keep]
        b_on_a = st.to_alice_clock(pb, int(a[0]), int(pb[0]), report)
        centres, sigma = ct.slice_centres(a, b_on_a, pb)
        good = [c for c in centres if c is not None]
        self.assertGreater(len(good), 0.9 * len(centres))
        # two_sites puts Bob at (1 + 31e-9) times Alice's time since its start, 37 us later, on another origin
        truth = np.array([ob - oa + 37_000_000 + 31e-9 * (t - oa) for t, _, _ in good])
        err = np.array([d for _, d, _ in good]) - truth
        self.assertLess(np.abs(err).max(), 30.0)
        self.assertAlmostEqual(sigma, 100.0, delta=30.0)  # two_sites jitters each pair by 100 ps
        tracker = ct.ClockTracker.from_model(model)
        sync = tracker.feed(centres)
        self.assertEqual(sync['locked_slices'], len(good))
        self.assertAlmostEqual(sync['skew_ppb'], 31.0, delta=1.0)


if __name__ == '__main__':
    unittest.main()
