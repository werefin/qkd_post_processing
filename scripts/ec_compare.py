'''
Error-correction comparison on one simulated binary symmetric channel per QBER: the Go methods
(cmd/ecbench: polar, regular LDPC) and library-based 5G NR LDPC (ec/ldpc5g.py), same metrics
'''

import argparse
import json
import subprocess
import sys
import time
from pathlib import Path

import numpy as np

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / 'ec'))
import ldpc5g  # noqa: E402


def ldpc_rows(bits, qs):
    rows = []
    for q in qs:
        rng = np.random.default_rng(int(q * 1e6))
        a = rng.integers(0, 2, bits, dtype=np.uint8)
        b = a ^ (rng.random(bits) < q).astype(np.uint8)
        for name, method in (('5G NR LDPC (py3gpp + ldpc), sum-product', 'product_sum'), ('5G NR LDPC (py3gpp + ldpc), min-sum', 'minimum_sum')):
            t = time.time()
            out, res, p = ldpc5g.reconcile(a, b, np.full(bits, q), q, bp_method=method)
            el = time.time() - t
            h = ldpc5g.h2(q)
            f = res.leaked / (bits * h)
            fail = res.failed / res.frames
            rows.append({'method': name, 'qber': q, 'efficiency': f, 'block_fail': fail,
                         'residual_ber': float((out != a).mean()), 'mbit_per_s': bits / el / 1e6,
                         'secret_fraction': max(0.0, (1 - fail) * (1 - h) - f * h), 'blocks': res.frames})
            print(f'{name:44} q={q:.2f} f={f:.3f} fail={fail:.3f} {bits / el / 1e6:.3f} Mbit/s', file=sys.stderr)
    return rows


def go_rows(bits, qs):
    exe = ROOT / 'ecbench.bin'
    subprocess.run(['go', 'build', '-o', str(exe), './cmd/ecbench'], cwd=ROOT, check=True)
    try:
        out = subprocess.run([str(exe), '-bits', str(bits), '-q', ','.join(map(str, qs)), '-json'], check=True, capture_output=True, text=True)
    finally:
        exe.unlink(missing_ok=True)
    return json.loads(out.stdout)


def table(rows, key, fmt):
    qs = sorted({r['qber'] for r in rows})
    names = list(dict.fromkeys(r['method'] for r in rows))
    lines = ['| Method | ' + ' | '.join(f'{q:.0%}' for q in qs) + ' |', '|---|' + '---|' * len(qs)]
    for n in names:
        cells = []
        for q in qs:
            r = next((r for r in rows if r['method'] == n and r['qber'] == q), None)
            cells.append(fmt(r) if r else '')
        lines.append(f'| {n} | ' + ' | '.join(cells) + ' |')
    return '\n'.join(lines)


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--bits', type=int, default=1 << 20)
    ap.add_argument('--q', default='0.01,0.02,0.03,0.05,0.07,0.09,0.11')
    ap.add_argument('--json', help='also write every row to this file')
    a = ap.parse_args()
    qs = [float(x) for x in a.q.split(',')]
    rows = go_rows(a.bits, qs) + ldpc_rows(a.bits, qs)
    if a.json:
        Path(a.json).write_text(json.dumps(rows, indent=1))
    print('Efficiency f (block failure rate)\n')
    print(table(rows, 'efficiency', lambda r: f"{r['efficiency']:.3f}" + (f" ({r['block_fail']:.2f})" if r['block_fail'] else '')))
    print('\nSecret fraction per sifted bit\n')
    print(table(rows, 'secret_fraction', lambda r: f"{r['secret_fraction']:.3f}"))
    print('\nThroughput, Mbit/s, all cores\n')
    print(table(rows, 'mbit_per_s', lambda r: f"{r['mbit_per_s']:.3f}"))


if __name__ == '__main__':
    main()
