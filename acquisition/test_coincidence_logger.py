'''
Coincidence logger tests: counts against a brute force on synthetic clicks however the stream is chunked, the
plan and options checks, the derived-value sandbox, the file format, the QBER of a synthetic BBM92 pair processed
offline, and a replayed run end to end
run: python3 -m unittest acquisition/test_coincidence_logger.py
'''

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import numpy as np

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import coincidences as co  # noqa: E402

PS = co.PS_PER_S
LOGGER = os.path.join(HERE, 'coincidence_logger.py')


def opts(**kw):
    return co.merge({'integration_s': 0.25, 'window_ps': 1000, 'noise_offset_ps': 50_000}, kw)


def clicks(seconds=1.0, seed=2, t0=5_000_000_000_000_000):
    '''Four channels on one clock: 1 and 2 share pairs offset by 3 ns, 3 a third photon of some of them, 4 noise'''
    rng = np.random.default_rng(seed)
    pairs = np.sort(rng.uniform(0, seconds * PS, int(20_000 * seconds))).astype(np.int64)
    noise = lambda n: rng.uniform(0, seconds * PS, int(n * seconds)).astype(np.int64)
    ch = {
        '1': np.concatenate([pairs, noise(30_000)]),
        '2': np.concatenate([pairs + 3000 + rng.normal(0, 100, pairs.size).astype(np.int64), noise(30_000)]),
        '3': np.concatenate([pairs[::4] - 2000, noise(10_000)]),
        '4': noise(50_000),
    }
    return {k: np.sort(v) + t0 for k, v in ch.items()}, t0


def brute(ref, others, lo, hi, origin, period, n):
    '''The slow way: reference clicks with a partner of every other channel in its ROI'''
    out = np.zeros(n, np.int64)
    for t in ref:
        k = (t - origin) // period
        if 0 <= k < n and all(((o >= t + lo) & (o < t + hi)).any() for o in others):
            out[k] += 1
    return out


def save_recording(path_base, ch):
    ts = np.concatenate(list(ch.values()))
    chan = np.concatenate([np.full(v.size, int(k), np.int8) for k, v in ch.items()])
    order = np.argsort(ts, kind='stable')
    np.save(f'{path_base}_timestamp_sequence.npy', ts[order])
    np.save(f'{path_base}_channel_sequence.npy', chan[order])
    return f'{path_base}_timestamp_sequence.npy'


class CounterTest(unittest.TestCase):
    def test_counts_match_brute_force_however_chunked(self):
        ch, t0 = clicks()
        plan = co.Plan(opts(delays_ps={'2': -3000, '3': 2000}, higher_order=[['1', '2', '3']]), [('lab', [1, 2, 3, 4])])
        period, n = int(0.25 * PS), 4
        shifted = {k: v + plan.delay[k] for k, v in ch.items()}
        want = {f'S_{k}': np.bincount((v[(v >= t0) & (v < t0 + n * period)] - t0) // period, minlength=n) for k, v in shifted.items()}
        for g in plan.groups:
            ref, others = shifted[g['channels'][0]], [shifted[c] for c in g['channels'][1:]]
            sub = ref[ref < t0 + n * period]
            want[g['name']] = brute(sub, others, *plan.roi, t0, period, n)
            want[g['name'] + '_noise'] = brute(sub, others, *plan.noise, t0, period, n)
        self.assertGreater(want['C_1_2'].sum(), 19_000)  # the pairs, once the delay lines them up
        self.assertGreater(want['C_1_2_3'].sum(), 4_000)
        for pieces in (1, 7, 40):
            c = co.WindowCounter(plan, t0)
            edges = np.linspace(t0, t0 + n * period + plan.reach, pieces + 1).astype(np.int64)
            rows = []
            for lo_t, hi_t in zip(edges[:-1], edges[1:]):
                for k, v in shifted.items():
                    c.feed(k, v[(v >= lo_t) & (v < hi_t)])
                rows += c.advance(hi_t - plan.reach)
            self.assertEqual([r[0] for r in rows], list(range(n)))
            for i, name in enumerate(plan.counts):
                np.testing.assert_array_equal([r[1][i] for r in rows], want[name], err_msg=f'{name}, {pieces} chunks')
        h = c.histograms()['groups']['C_1_2']
        self.assertGreater(sum(h[len(h) // 2 - 10:len(h) // 2 + 10]), 0.8 * sum(h))  # the peak in the middle bins

    def test_closing_gives_the_partial_window(self):
        ch, t0 = clicks()
        plan = co.Plan(opts(), [('lab', [1, 2, 3, 4])])
        c = co.WindowCounter(plan, t0)
        for k, v in ch.items():
            c.feed(k, v[v < t0 + int(0.4 * PS)])
        self.assertEqual([r[0] for r in c.advance(t0 + int(0.4 * PS))], [0])
        self.assertAlmostEqual(c.close()[0][2] / PS, 0.15)


class PlanTest(unittest.TestCase):
    def test_groups_and_qber(self):
        plan = co.Plan(opts(), [('a', [1, 2, 3, 4]), ('b', [1, 2, 3, 4])])
        self.assertEqual(len(plan.groups), 16)
        self.assertEqual(plan.groups[1]['name'], 'C_A1_B2')
        self.assertEqual(list(plan.derived), ['QBER_HV', 'QBER_DA', 'QBER'])
        self.assertIn('aligned', plan.columns)
        self.assertEqual(len(co.Plan(opts(), [('lab', [1, 2, 3, 4])]).groups), 6)
        self.assertEqual(co.Plan(opts(qber=False), [('a', [1, 2, 3, 4]), ('b', [1, 2, 3, 4])]).derived, {})
        row = {f'C_A{a}_B{b}{s}': ((100 if a == b else 5) if not s else 1) for a in range(1, 5) for b in range(1, 5) for s in ('', '_noise')}
        plan.with_derived(row)
        self.assertAlmostEqual(row['QBER_HV'], 8 / (8 + 198))  # noise taken off every count

    def test_mistakes_are_refused(self):
        sites = [('a', [1, 2]), ('b', [1, 2])]
        for bad in (dict(twofold=[['A1', 'B7']]), dict(delays_ps={'9': 5}), dict(derived={'S_A1': 'S_A2'}), dict(derived={'q': 'S_A1 +'})):
            with self.assertRaises(ValueError, msg=str(bad)):
                co.Plan(opts(**bad), sites)
        for bad in ({'colour': 1}, {'integration_s': 0}, {'sites': 'a'}):
            with self.assertRaises(ValueError, msg=str(bad)):
                co.merge(bad)


class DerivedTest(unittest.TestCase):
    def test_arithmetic_only(self):
        f = co.compile_expr('(C_1_2 - C_1_2_noise) / sqrt(S_1 * S_2)', {'C_1_2', 'C_1_2_noise', 'S_1', 'S_2'})
        self.assertAlmostEqual(f({'C_1_2': 110, 'C_1_2_noise': 10, 'S_1': 1000, 'S_2': 400}), 100 / 632.4555, places=5)
        self.assertIsNone(f({'C_1_2': 1, 'C_1_2_noise': 0, 'S_1': 0, 'S_2': 5}))
        for bad in ('__import__("os")', 'S_1.real', 'open("x")', 'S_9 + 1', '[1][0]'):
            with self.assertRaises(ValueError, msg=bad):
                co.compile_expr(bad, {'S_1'})


class FileTest(unittest.TestCase):
    def test_round_trip(self):
        plan = co.Plan(opts(derived={'eff': 'C_1_2 / S_1'}, metadata={'experiment': 'test'}), [('lab', [1, 2])])
        with tempfile.TemporaryDirectory() as d:
            log = co.CsvLog(Path(d) / 'log.csv')
            log.start(plan, plan.metadata([('lab', {'channels': [1, 2], 'tt_ip': '192.0.2.1'})], 'test', 'one clock'))
            log.row(plan.with_derived({'window': 0, 'duration_s': 0.25, 'complete': 1, 'S_1': 200, 'S_2': 180, 'C_1_2': 50,
                                       'C_1_2_noise': 1, 'capped_reads': 0}))
            log.close()
            meta, columns, rows = co.read_log(log.path)
            text = log.path.read_text()
        self.assertEqual(columns, plan.columns)
        self.assertEqual(meta['experiment'], {'experiment': 'test'})
        self.assertEqual(rows[0]['eff'], 0.25)
        self.assertNotIn('192.0.2.1', text)  # sites by name only, never addresses


class OfflineQberTest(unittest.TestCase):
    def test_bbm92_pair_gives_its_qber(self):
        '''Two TimeTaggers 40 us and 25 ppb apart; same-basis pairs carry 4% bit flips'''
        rng = np.random.default_rng(1)
        secs = 12.0
        pairs = np.sort(rng.uniform(0, secs * PS, int(4000 * secs))).astype(np.int64)
        basis, bit = rng.integers(0, 2, pairs.size), rng.integers(0, 2, pairs.size)
        b_basis = np.where(rng.random(pairs.size) < 0.5, basis, 1 - basis)
        b_bit = np.where(b_basis == basis, bit ^ (rng.random(pairs.size) < 0.04), rng.integers(0, 2, pairs.size))
        a_ch, b_ch = 1 + 2 * basis + bit, 1 + 2 * b_basis + b_bit
        a, b = {}, {}
        for c in (1, 2, 3, 4):
            a[str(c)] = np.sort(np.concatenate([pairs[a_ch == c], rng.uniform(0, secs * PS, int(15000 * secs)).astype(np.int64)])) + 10**15
            tb = np.concatenate([pairs[b_ch == c] + rng.normal(0, 120, (b_ch == c).sum()).astype(np.int64),
                                 rng.uniform(0, secs * PS, int(12000 * secs)).astype(np.int64)])
            b[str(c)] = np.sort(np.round(tb * (1 + 25e-9)).astype(np.int64) + 40_000_000) + 7 * 10**15
        with tempfile.TemporaryDirectory() as d:
            pa = save_recording(Path(d) / '20260101_120000_192_0_2_1_p40404', a)
            pb = save_recording(Path(d) / '20260101_120000_192_0_2_2_p40404', b)
            out = Path(d) / 'log.csv'
            r = subprocess.run([sys.executable, LOGGER, str(Path(d) / 'none.json'), '--recordings', pa, pb, '--integration-s', '4',
                                '--metadata', 'experiment=offline test', '--output', str(out)], capture_output=True, text=True, timeout=180)
            self.assertEqual(r.returncode, 0, r.stdout[-2000:] + r.stderr[-2000:])
            meta, columns, rows = co.read_log(out)
        self.assertEqual(meta['experiment'], {'experiment': 'offline test'})
        full = [row for row in rows if row['complete']]
        self.assertGreaterEqual(len(full), 2)
        for row in full:
            self.assertEqual(row['aligned'], 1)
            for q in ('QBER_HV', 'QBER_DA', 'QBER'):
                self.assertAlmostEqual(row[q], 0.04, delta=0.012, msg=f'{q} in window {row["window"]}')


class ReplayTest(unittest.TestCase):
    def test_replayed_run_writes_complete_rows(self):
        ch, t0 = clicks(seconds=3.0)
        with tempfile.TemporaryDirectory() as d:
            p = save_recording(Path(d) / '20260101_000000_192_0_2_1_p40404', ch)
            cfg = Path(d) / 'config.json'
            cfg.write_text('{"coincidence_log": {"delays_ps": {"2": -3000}}}')
            out = Path(d) / 'log.csv'
            r = subprocess.run([sys.executable, LOGGER, str(cfg), '--replay', p, '--integration-s', '0.5', '--windows', '3',
                                '--output', str(out), '--json'], capture_output=True, text=True, timeout=120)
            self.assertEqual(r.returncode, 0, r.stdout[-2000:] + r.stderr[-2000:])
            meta, columns, rows = co.read_log(out)
        status = [json.loads(l[len(co.STATUS_PREFIX):]) for l in r.stdout.splitlines() if l.startswith(co.STATUS_PREFIX)]
        self.assertEqual(status[-1]['state'], 'finished')
        self.assertIn('histograms', status[-1])
        self.assertTrue(any(s.get('live') for s in status))
        self.assertEqual(len(rows), 3)
        for row in rows:
            self.assertEqual(row['complete'], 1)
            self.assertGreater(row['C_1_2'], 5_000)  # all the pairs once delayed
            self.assertLess(row['C_1_2_noise'], row['C_1_2'] / 20)


if __name__ == '__main__':
    unittest.main()
