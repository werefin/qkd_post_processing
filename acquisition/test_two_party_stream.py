'''
Continuous two-party QKD streamed per segment: the compact announcement round trips, and the reduction keeps
every pair the coincidence filter can see, with the double clicks squashed as importtt would on the full data
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
SQUASH_WINDOW_PS = 2_000  # importtt's default squashing window


def squash_groups(ts, ch, window=SQUASH_WINDOW_PS):
    '''importtt's squashClicks without the random outcome: each measurement click's group, as the index of its
    first click, and the channels each group saw'''
    first, seen = np.full(ts.size, -1), np.zeros(ts.size, np.uint8)
    anchor = -1
    for i in np.flatnonzero((ch >= 1) & (ch <= 4)):
        if anchor < 0 or not 0 <= ts[i] - ts[anchor] <= window:
            anchor = i
        first[i] = anchor
        seen[anchor] |= 1 << (int(ch[i]) - 1)
    return first, seen


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
        # every click of Bob's in such a pair falls in the same squashing group, with the same channels, on the
        # reduced clicks as on the full ones
        first_full, seen_full = squash_groups(b_on_a, chb[keep])
        first_red, seen_red = squash_groups(b_on_a[keep_b], chb[keep][keep_b])
        paired = np.array(sorted({j for _, j in full}))
        at = np.searchsorted(ib, paired)
        np.testing.assert_array_equal(ib[first_red[at]], first_full[paired])
        np.testing.assert_array_equal(seen_red[first_red[at]], seen_full[first_full[paired]])
        self.assertGreater(np.count_nonzero(np.bincount(first_full[first_full >= 0]) > 1), 0)

    def test_identity_report_leaves_clicks_on_alices_clock(self):
        ts = np.array([10**15 + 5, 10**15 + 7_000, 10**15 + 10**12], np.int64)
        rep = st.identity_report('a', 'b', a0=10**15, b0=int(ts[0]))
        np.testing.assert_array_equal(st.to_alice_clock(ts, 10**15, int(ts[0]), rep), ts)


if __name__ == '__main__':
    unittest.main()
