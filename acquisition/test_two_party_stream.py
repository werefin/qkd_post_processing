'''
Continuous two-party QKD streamed per segment: the compact announcement round trips, and the reduction keeps
every pair the coincidence filter can see, with the multi-photon flags importtt would compute on the full data
run: python3 -m unittest acquisition/test_two_party_stream.py
'''

import os
import sys
import unittest

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import two_party_blocks as tp  # noqa: E402
import two_party_stream as st  # noqa: E402
from test_two_party_blocks import PS, two_sites  # noqa: E402

FILTER_RADIUS_PS = 3_000  # the coincidence filter's search radius in importtt
MP_WINDOW_PS = 2_000      # importtt's default multi-photon window


def multiphoton(ts, ch, window=MP_WINDOW_PS):
    '''importtt's detectMultiPhoton: a click with another on its basis partner channel within window'''
    flag = np.zeros(ts.size, bool)
    for c, partner in ((1, 2), (3, 4)):
        i, j = np.flatnonzero(ch == c), np.flatnonzero(ch == partner)
        flag[i] |= st.near(ts[i], ts[j], window)
        flag[j] |= st.near(ts[j], ts[i], window)
    return flag


def pairs_within(a, b, radius):
    '''Every (index in a, index in b) with |b - a| <= radius'''
    lo, hi = np.searchsorted(b, a - radius), np.searchsorted(b, a + radius, side='right')
    return {(i, j) for i in np.flatnonzero(hi > lo) for j in range(lo[i], hi[i])}


class StreamTest(unittest.TestCase):
    def test_announcement_round_trips_at_any_rate(self):
        rng = np.random.default_rng(5)
        for rate_hz in (3e3, 3e6):
            ts = np.cumsum(rng.exponential(PS / rate_hz, 200_000)).astype(np.int64) + 10**15
            ts[100] -= 40  # detector delays can put two clicks out of order
            ts[5000] += 10 * PS  # and a long gap escapes to 64 bits
            ts[5001:] += 10 * PS
            flags = rng.integers(0, 4, ts.size).astype(np.int8)
            blob = st.encode(ts, flags)
            back_ts, back_flags = st.decode(blob)
            np.testing.assert_array_equal(back_ts, ts)
            np.testing.assert_array_equal(back_flags, flags)
            self.assertLess(len(blob), 4.4 * ts.size)
            self.assertEqual(st.join_parts(list(reversed(st.parts_of(blob)))), blob)
        empty = st.decode(st.encode(np.empty(0, np.int64), np.empty(0, np.int8)))
        self.assertEqual(empty[0].size, 0)
        with self.assertRaises(ValueError):
            st.decode(blob[:-1])

    def test_flags_pack_four_to_a_byte(self):
        flags = np.random.default_rng(1).integers(0, 4, 100_001).astype(np.uint8)
        packed = st.pack_flags(flags)
        self.assertEqual(packed.size, 25_001)
        back = np.stack([(packed >> s) & 3 for s in (0, 2, 4, 6)], axis=1).reshape(-1)[:flags.size]
        np.testing.assert_array_equal(back, flags)

    def test_reduction_keeps_every_pair_the_filter_sees(self):
        (a, cha, _), (b, chb, _), _ = two_sites(seconds=20)
        report, keep, _, _ = tp.align_arrays(a, b, None, {'slope_range_ppb': 200.0})
        pb = b[keep]
        b_on_a = st.to_alice_clock(pb, int(a[0]), int(pb[0]), report)
        keep_a, keep_b = st.masks(a, b_on_a)
        self.assertLess(keep_a.mean(), 0.5)
        # the same pairs inside the filter's radius, on the full and on the reduced clicks
        full = pairs_within(a, b_on_a, FILTER_RADIUS_PS)
        ia, ib = np.flatnonzero(keep_a), np.flatnonzero(keep_b)
        reduced = {(ia[i], ib[j]) for i, j in pairs_within(a[keep_a], b_on_a[keep_b], FILTER_RADIUS_PS)}
        self.assertEqual(reduced, full)
        # Bob's multi-photon flags of every click in such a pair are the same on the reduced clicks
        mp_full = multiphoton(b_on_a, chb[keep])
        mp_reduced = multiphoton(b_on_a[keep_b], chb[keep][keep_b])
        paired = np.array(sorted({j for _, j in full}))
        np.testing.assert_array_equal(mp_reduced[np.searchsorted(ib, paired)], mp_full[paired])

    def test_identity_report_leaves_clicks_on_alices_clock(self):
        ts = np.array([10**15 + 5, 10**15 + 7_000, 10**15 + 10**12], np.int64)
        rep = st.identity_report('a', 'b', a0=10**15, b0=int(ts[0]))
        np.testing.assert_array_equal(st.to_alice_clock(ts, 10**15, int(ts[0]), rep), ts)


if __name__ == '__main__':
    unittest.main()
