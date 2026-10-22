'''
Continuous two-party segments: chosen by UTC start, dropped when old, and Bob's clicks aligned to Alice's
announcement, by a full search the first time and from the previous clock model after
run: python3 -m unittest acquisition/test_two_party_blocks.py
'''

import os
import sys
import tempfile
import unittest
from datetime import datetime, timezone
from pathlib import Path

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import two_party_blocks as tp  # noqa: E402

PS = 10**12
T0 = 1_792_000_000  # a UTC second on a 10 s boundary


def write_segments(root, origin, ts, ch, seg_s, n_seg, offset_s, ip):
    '''One site's recorder output: a directory per segment named by its UTC start, the site's clock read at
    host times shifted by offset_s (NTP error and read granularity)'''
    for k in range(n_seg):
        lo, hi = origin + int((k * seg_s + offset_s) * PS), origin + int(((k + 1) * seg_s + offset_s) * PS)
        if k == 0:
            lo = ts[0]
        sel = (ts >= lo) & (ts < hi)
        name = datetime.fromtimestamp(T0 + k * seg_s, timezone.utc).strftime('%Y%m%d_%H%M%S')
        d = root / name
        d.mkdir(parents=True)
        base = f'{name}_{ip}_p40404'
        np.save(d / (base + tp.TS), ts[sel])
        np.save(d / (base + tp.CH), ch[sel])


def two_sites(seconds=40, seed=3, lag_ps=37_000_000, skew=31e-9):
    rng = np.random.default_rng(seed)
    pairs = np.sort(rng.uniform(0, seconds * PS, int(5000 * seconds))).astype(np.int64)
    a = np.concatenate([pairs, rng.uniform(0, seconds * PS, int(30_000 * seconds)).astype(np.int64)])
    b_pairs = np.round(pairs * (1 + skew)).astype(np.int64) + lag_ps + rng.normal(0, 100, pairs.size).astype(np.int64)
    b = np.concatenate([b_pairs, rng.uniform(0, seconds * PS, int(60_000 * seconds)).astype(np.int64) + lag_ps])
    oa, ob = 10**15, 4 * 10**15
    ia, ib = np.argsort(a, kind='stable'), np.argsort(b, kind='stable')
    cha = rng.integers(1, 5, a.size).astype(np.int8)
    chb = rng.integers(1, 5, b.size).astype(np.int8)
    return (a[ia] + oa, cha[ia], oa), (b[ib] + ob, chb[ib], ob), (pairs + oa, b_pairs + ob)


class BlockTest(unittest.TestCase):
    def test_segments_chosen_by_utc_start_and_dropped(self):
        (a, cha, oa), _, _ = two_sites(seconds=20)
        with tempfile.TemporaryDirectory() as d:
            root = Path(d) / 'seg'
            write_segments(root, oa, a, cha, 10, 2, 0.0, '192_0_2_1')
            (root / '.20261020_000000.tmp').mkdir()  # a segment still being written is never picked
            segs = tp.segments_in(root, T0, T0 + 20)
            self.assertEqual([s.name for s in segs], sorted(s.name for s in segs))
            self.assertEqual(len(segs), 2)
            self.assertEqual(tp.drop_old_segments(root, T0 + 10), 1)

    def test_segments_aligned_by_search_then_by_model(self):
        (a, cha, oa), (b, chb, ob), (pa, pb) = two_sites()
        model, hows = None, []
        for k in range(4):
            # Bob's host cuts 0.3 s later: segment edges differ a little, inside a segment nothing is lost
            sa = a[(a >= oa + k * 10 * PS) & (a < oa + (k + 1) * 10 * PS)]
            sb = b[(b >= ob + int((k * 10 + 0.3) * PS)) & (b < ob + int(((k + 1) * 10 + 0.3) * PS))]
            report, keep, model, how = tp.align_arrays(sa, sb, model, {'slope_range_ppb': 200.0})
            hows.append(how)
            self.assertTrue(report['detected'])
            self.assertAlmostEqual(report['clock_skew']['skew_ppb'], 31, delta=1.0)
            self.assertLess(keep.sum(), sb.size / 5)  # most of Bob's clicks cannot pair and are dropped
            # every genuine partner of an announced click in both segments survives the pruning
            inside = np.isin(pa, sa) & np.isin(pb, sb)
            self.assertTrue(np.isin(pb[inside], sb[keep]).all())
        self.assertEqual(hows, ['full search'] + ['previous clock model'] * 3)


if __name__ == '__main__':
    unittest.main()
