'''
Evaluates recordings through every stage of the chain, writing a report with figures per pair and a summary
pooling each link; usage: evaluate.py <recording dir or parent dir> [...] [--out <dir>]
'''

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time
from collections import defaultdict
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime
from pathlib import Path

import numpy as np

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent
sys.path.insert(0, str(ROOT / 'acquisition'))
sys.path.insert(0, str(HERE))
import coincidence_peak as cp  # noqa: E402
import figures  # noqa: E402

TS_SUFFIX = '_timestamp_sequence.npy'
NAME_RE = re.compile(r'^(?P<start>\d{8}_\d{6})_(?P<ip>\d+_\d+_\d+_\d+)_p(?P<port>\d+)' + re.escape(TS_SUFFIX) + '$')
IP_RE = re.compile(r'(?<![\d.])\d{1,3}[._]\d{1,3}[._]\d{1,3}[._]\d{1,3}(?![\d.])')
SWEEP_PS = [50, 100, 150, 200, 300, 400, 500, 750, 1000, 1500, 2000]
# error correction: name, importtt flags, and the JSON field holding its disclosure
METHODS = {
    'cascade': ('Cascade, optimized', ['-cascade'], 'cascade_parities'),
}


def log(msg):
    print(f'[eval] {msg}', flush=True)


# inputs

def find_pairs(paths):
    '''Every two recordings in one directory sharing a start time form a pair; anything else is reported and skipped'''
    groups, skipped = defaultdict(list), []
    for p in paths:
        p = Path(p).resolve()
        files = [p] if p.is_file() else sorted(p.rglob('*' + TS_SUFFIX))
        for f in files:
            m = NAME_RE.match(f.name)
            if m:
                groups[(f.parent, m['start'])].append(f)
            else:
                skipped.append((f, 'name does not follow <start>_<ip>_p<port>_timestamp_sequence.npy'))
    pairs = []
    for (d, start), fs in sorted(groups.items()):
        if len(fs) == 2:
            pairs.append(sorted(fs))  # A is the first in sorted order, as tt_record_dual.py uses
        else:
            skipped += [(f, f'{len(fs)} recordings start at {start} in this directory, need exactly 2') for f in fs]
    return pairs, skipped


def site_names(cfg):
    return {s['tt_ip']: name.replace('_', ' ').title() for name, s in cfg.get('sites', {}).items() if 'tt_ip' in s}


def side_label(path, names, side):
    '''The site's name from config.json; never its IP, which must not reach a report or figure'''
    return names.get(NAME_RE.match(path.name)['ip'].replace('_', '.'), f'site {side}')


def public_name(path, names):
    '''A file name with its IP replaced by the site name, safe for reports'''
    return re.sub(r'_(\d+_\d+_\d+_\d+)_p\d+', lambda m: '_' + names.get(m[1].replace('_', '.'), 'site').replace(' ', '_'), Path(path).name)


def peak_kwargs(cfg):
    '''analyze_files settings from config.json's coincidence_peak section, the same the recorder uses'''
    return cp.kwargs_from_config(cfg)


# tools

def find_importtt(given):
    if given:
        return given
    out = HERE / '.bin' / 'importtt'
    if shutil.which('go'):
        out.parent.mkdir(exist_ok=True)
        subprocess.run(['go', 'build', '-o', str(out), './cmd/importtt'], cwd=ROOT, check=True)
        return str(out)
    for cand in (ROOT / 'importtt', shutil.which('importtt')):
        if cand and Path(cand).is_file():
            return str(cand)
    sys.exit('no importtt binary: install Go, build it (go build ./cmd/importtt) or pass --importtt')


class Importtt:
    def __init__(self, binary):
        self.binary, self.env = binary, dict(os.environ)

    def run(self, dirs, *flags):
        dirs = dirs if isinstance(dirs, list) else [dirs]
        r = subprocess.run([self.binary, '-dir', ','.join(str(d) for d in dirs), '-json', *flags],
                           capture_output=True, text=True, env=self.env)
        if r.returncode != 0:
            try:  # with -json the reason is in the JSON on stdout
                why = json.loads(r.stdout.strip().splitlines()[-1])['error']
            except (IndexError, ValueError, KeyError, TypeError):
                why = (r.stderr.strip().splitlines() or ['importtt failed'])[-1]
            raise RuntimeError(IP_RE.sub('<ip>', str(why))[:300])
        return json.loads(r.stdout)


# one pair

def stage(pair, inp, kwargs):
    '''A directory importtt reads: links to both recordings and a fresh coincidence report from the current code;
    temporary, since the links and the report carry the original file names and so the IPs'''
    inp.mkdir(parents=True, exist_ok=True)
    for ts in pair:
        base = ts.name[:-len(TS_SUFFIX)]
        for f in ts.parent.glob(base + '_*'):
            if f.name.endswith(('_timestamp_sequence.npy', '_channel_sequence.npy', '_meta.json')):
                link = inp / f.name
                if not link.exists():
                    link.symlink_to(f)
    report_path = inp / (pair[0].name[:-len(TS_SUFFIX)] + '_coincidence.json')
    t0 = time.time()
    report, diag = cp.analyze_files(inp / pair[0].name, inp / pair[1].name, strict=False, **kwargs)
    report_path.write_text(json.dumps(report, indent=1, default=float))
    return inp, report, diag, time.time() - t0


def evaluate_pair(pair, out, staging, it, cfg, args):
    names = site_names(cfg)
    name_a, name_b = side_label(pair[0], names, 'A'), side_label(pair[1], names, 'B')
    start = NAME_RE.match(pair[0].name)['start']
    pair_name = f'{start}_{name_a}-{name_b}'.replace(' ', '_')
    work = out / pair_name
    work.mkdir(parents=True, exist_ok=True)
    res = {'name': pair_name, 'start': start, 'a': {'site': name_a}, 'b': {'site': name_b},
           'link': f'{name_a}-{name_b}', 'notes': []}
    log(f'{pair_name}: loading')
    sides = []
    for ts_path, label, side in ((pair[0], f'A, {name_a}', 'a'), (pair[1], f'B, {name_b}', 'b')):
        ts = np.load(ts_path, mmap_mode='r')
        ch_path = Path(str(ts_path).replace('timestamp_sequence', 'channel_sequence'))
        ch = np.load(ch_path) if ch_path.is_file() else np.ones(len(ts), np.int8)
        dur = (int(ts[-1]) - int(ts[0])) / 1e12
        per_ch = {int(c): int((ch == c).sum()) for c in (1, 2, 3, 4)}
        res[side].update(clicks=int(len(ts)), duration_s=dur, rate=len(ts) / dur, per_channel=per_ch)
        meta_path = Path(str(ts_path).replace('timestamp_sequence.npy', 'meta.json'))
        if meta_path.is_file():
            meta = json.loads(meta_path.read_text())
            frames = meta.get('frames', meta if isinstance(meta, list) else [])
            res[side]['capped_reads'] = sum(1 for f in frames if isinstance(f, dict) and f.get('capped'))
        sides.append((label, np.asarray(ts), ch))
    figures.rates(sides, work / 'rates.png')
    del sides

    log(f'{pair_name}: offline coincidence-peak search')
    inp, report, diag, secs = stage(pair, staging / pair_name, peak_kwargs(cfg))
    res['input'] = str(inp)
    sk, cs = report['clock_skew'], report['constant_shift']
    res['peak'] = {'detected': report['detected'], 'lag_ms': cs['relative_lag_ps'] / 1e9, 'score': cs['window']['local_score'],
                   'constant_window_ps': cs['window']['width_ps'], 'skew_ppb': sk['skew_ppb'], 'skew_method': sk.get('skew_method'),
                   'corrected': sk['corrected_window'], 'seconds': secs}
    res['peak']['fit'] = figures.peak(report, diag, cp.MIN_PEAK_SCORE, work / 'peak.png')
    del diag
    if not report['detected']:
        res['notes'].append('no significant coincidence peak: the later stages cannot run')
        return res

    # every importtt run of the pair goes out at once
    base_method = 'cascade'
    sweep = [] if args.no_sweep else ['-window-sweep', ','.join(map(str, SWEEP_PS))]
    pool = ThreadPoolExecutor(args.jobs)

    def run(*flags):
        try:
            return it.run(inp, *flags)
        except RuntimeError as e:
            return e

    log(f'{pair_name}: sifting, window sweep and {len(args.methods)} error-correction methods, {args.jobs} at a time')
    base_job = pool.submit(run, *METHODS[base_method][1], *sweep)
    jobs = {}
    for key in args.methods:
        flags = METHODS[key][1]
        # the base run is this method's finite-key run already
        fin = base_job if key == base_method else pool.submit(run, *flags)
        jobs[key] = (fin, pool.submit(run, *flags, '-asymptotic'))

    base = base_job.result()
    if isinstance(base, Exception):
        pool.shutdown(cancel_futures=True)
        res['notes'].append(f'importtt failed: {base}')
        return res
    for key in ('a_recording', 'b_recording', 'recording_dir'):  # file names carry the IPs
        base.pop(key, None)
    res['base'] = base
    res['base_method'] = base_method

    if not args.no_sweep:
        # the skew-only curve also at the chosen window, which only the base run knew
        widths = sorted(set(SWEEP_PS + [int(base['window_ps'])]))
        skew_only = run('-filter=false', '-window-sweep', ','.join(map(str, widths)))
        tracked = {e['window_ps']: (e['window_ps'], e['sifted_bits'], e['qber_vis']) for e in base.pop('window_sweep', [])}
        tracked[int(base['window_ps'])] = (int(base['window_ps']), base['sifted_bits'], base['qber_vis'])
        other = {} if isinstance(skew_only, Exception) else {e['window_ps']: (e['window_ps'], e['sifted_bits'], e['qber_vis']) for e in skew_only['window_sweep']}
        res['sweep'] = {'drift tracked': [tracked.get(w, (w, None, None)) for w in widths],
                        'skew only': [other.get(w, (w, None, None)) for w in widths]}
        figures.window(res['sweep'], int(base['window_ps']), work / 'window.png')

    runs = {}
    for key in args.methods:
        label, flags, leak_key = METHODS[key]
        fin, asy = jobs[key][0].result(), jobs[key][1].result()
        if isinstance(fin, Exception) or isinstance(asy, Exception):
            runs[label] = {'leak': None, 'ok': False, 'key': 0, 'finite': 0, 'error': str(fin if isinstance(fin, Exception) else asy)[:120]}
            continue
        leak = fin.get(leak_key) if leak_key else fin['leaked_bits_total'] - fin['bits_leaked_crc']
        runs[label] = {'leak': int(leak), 'ok': fin['chunks_total'] > 0 and fin['chunks_kept'] == fin['chunks_total'],
                       'key': int(asy['final_key_bits']), 'finite': int(fin['final_key_bits']), 'qber': fin['qber'],
                       'kept': f'{fin["chunks_kept"]}/{fin["chunks_total"]}'}
    pool.shutdown()
    res['ec'] = runs
    n_bits = base['finite_key']['surviving_bits']
    res['shannon'] = figures.ec(runs, n_bits, base['qber'], work / 'ec.png')
    label = METHODS[base_method][0]
    leak = base.get(METHODS[base_method][2]) or base['leaked_bits_total'] - base['bits_leaked_crc']
    figures.budget(base['sifted_bits'], base['sifted_bits'] - n_bits, int(leak), base['bits_leaked_crc'],
                   runs.get(label, {}).get('key', 0), base['final_key_bits'], label, work / 'budget.png')
    return res


# reports

def pct(x, digits=1):
    return '-' if x is None else f'{100 * x:.{digits}f}%'


def write_pair_report(res, work):
    a, b = res['a'], res['b']
    L = [f'### Evaluation of {res["name"]}', '',
         f'**Recording:** {res["start"][:4]}-{res["start"][4:6]}-{res["start"][6:8]} {res["start"][9:11]}:{res["start"][11:13]}:{res["start"][13:]}, '
         f'link {res["link"]}, evaluated on {datetime.now():%Y-%m-%d} by `evaluation/evaluate.py` with the code as it stands and the settings of `acquisition/config.json`. '
         'Error correction, verification and privacy amplification at $\\varepsilon_\\mathrm{sec} = 10^{-10}$, $\\varepsilon_\\mathrm{cor} = 10^{-15}$ as in `doc/security.md`.', '']
    if res['notes']:
        L += ['**Notes:** ' + '; '.join(res['notes']) + '.', '']
    L += ['### Data', '', '| Side | Site | Clicks | Length | Rate | Channels 1/2/3/4 |', '|---|---|---|---|---|---|']
    for side, s in (('A', a), ('B', b)):
        pc = s.get('per_channel', {})
        L.append(f'| {side} | {s["site"]} | {s.get("clicks", 0):,} | {s.get("duration_s", 0):.2f} s | '
                 f'{s.get("rate", 0) / 1e3:,.0f} kHz | {" / ".join(f"{pc.get(c, 0):,}" for c in (1, 2, 3, 4))} |')
    capped = [f'{side} {s["capped_reads"]}' for side, s in (('A', a), ('B', b)) if s.get('capped_reads')]
    L += ['', '![Detector singles](rates.png)', '']
    if capped:
        L += [f'Reads that hit the server\'s batch cap, so detections may be missing: {", ".join(capped)}.', '']
    p = res.get('peak')
    if p:
        fit = p.get('fit') or {}
        c = p['corrected']
        L += ['### Coincidence-peak finding', '',
              '| Quantity | Value |', '|---|---|',
              f'| Peak detected | {"yes" if p["detected"] else "no"} (score {p["score"]:.1f} with the lag held constant, threshold {cp.MIN_PEAK_SCORE:g}) |',
              f'| Lag, B minus A, from the first clicks | {p["lag_ms"]:.6f} ms |',
              f'| Clock skew | {p["skew_ppb"]:.2f} ppb ({p["skew_method"]}) |',
              f'| Clock-corrected peak | {c["estimated_excess"]:,.0f} pairs above {c["estimated_accidentals"]:,.0f} accidentals in {c["width_ps"]} ps, score {c["local_score"]:.0f} |']
        if fit:
            L.append(f'| Gaussian fit, all channels | $\\sigma = {fit["sigma_ps"]:.0f}$ ps, HWHM {fit["hwhm_ps"]:.0f} ps |')
        L += [f'| Search time | {p["seconds"]:.1f} s |', '', '![Offline coincidence-peak search](peak.png)', '']
    base = res.get('base')
    if base:
        L += ['### Coincidence filtering and sifting', '',
              '| Quantity | Value |', '|---|---|',
              f'| Window, fitted HWHM | ±{base["window_ps"]} ps ($\\sigma = {base.get("filter_sigma_ps", 0):.0f}$ ps) |',
              f'| Predicted accidentals in the window | {pct(base.get("filter_predicted_accidental_fraction"))} |',
              f'| Sifted bits | {base["sifted_bits"]:,} ({base["sifted_bits"] / max(a.get("duration_s", 1), 1e-9):,.0f} per second) |',
              f'| QBER over all sifted pairs | {pct(base.get("qber_vis"))} (H/V {pct(base.get("qber_bs_hv"))}, D/A {pct(base.get("qber_bs_da"))}) |',
              f'| Sample QBER | {pct(base["qber"])} on {base["sifted_bits"] - base["finite_key"]["surviving_bits"]:,} bits |',
              f'| Visibility H/V, D/A | {base.get("visibility_hv", 0):.3f}, {base.get("visibility_da", 0):.3f} |',
              f'| Multi-photon clicks rejected, A / B | {base.get("a_multiphoton_count", 0):,} / {base.get("b_multiphoton_count", 0):,} |', '']
        if res.get('sweep'):
            rows = res['sweep']['drift tracked']
            L += ['![Window choice](window.png)', '', '| Window | Sifted bits | QBER over all sifted pairs |', '|---|---|---|']
            for w, n, q in rows:
                bold = '**' if w == base['window_ps'] else ''
                L.append(f'| {bold}±{w} ps{bold} | {"-" if n is None else f"{n:,}"} | {pct(q)} |')
            L.append('')
    if res.get('ec'):
        L += ['### Error correction', '',
              f'Cascade sees the same {base["finite_key"]["surviving_bits"]:,} bits at {pct(base["qber"])} sample QBER; the Shannon limit is {res["shannon"]:,.0f} bits. Disclosure is everything error correction sends, before the CRC check.', '',
              '![Error correction](ec.png)', '',
              '| Method | Disclosed bits | Efficiency $f$ | Chunks kept | Asymptotic key | Finite key |', '|---|---|---|---|---|---|']
        for name, r in res['ec'].items():
            if r.get('error'):
                L.append(f'| {name} | did not run: {r["error"]} | | | | |')
                continue
            f_eff = r['leak'] / res['shannon'] if res['shannon'] else float('nan')
            L.append(f'| {name} | {r["leak"]:,} | {f_eff:.2f} | {r["kept"]} | {r["key"]:,} | {r["finite"]:,} |')
        L += ['', '### Key budget', '', '![Key budget](budget.png)', '']
        fk = base['finite_key']
        L += ['### Finite key', '',
              f'Phase-error estimate {pct(fk["phase_error_point"])}, upper bound at $\\varepsilon_\\mathrm{{sec}}$ {pct(fk["phase_error_bound"])}; '
              f'{base["final_key_bits"]:,} bits certified with {METHODS[res["base_method"]][0]}. '
              + ('A bound at 50% means the sample is too small to certify anything; pooling recordings of this link (see the summary) is the way to a finite key.'
                 if fk['phase_error_bound'] >= 0.5 else ''), '']
    (work / 'report.md').write_text('\n'.join(L))


def pooled(results, it):
    '''importtt over all evaluated recordings of each link at once: one privacy-amplification block, a larger sample'''
    by_link = defaultdict(list)
    for r in results:
        if r.get('base'):
            by_link[r['link']].append(r)
    out = {}
    for link, rs in by_link.items():
        if len(rs) < 2:
            continue
        dirs = [Path(r['input']) for r in rs]
        flags = ['-cascade']
        log(f'pooling {len(rs)} recordings of {link}')
        try:
            fin, asy = it.run(dirs, *flags), it.run(dirs, *flags, '-asymptotic')
            out[link] = {'n': len(rs), 'seconds': sum(r['a'].get('duration_s', 0) for r in rs), 'sifted': fin['sifted_bits'],
                         'qber': fin['qber'], 'finite': fin['final_key_bits'], 'asym': asy['final_key_bits'],
                         'bound': fin['finite_key']['phase_error_bound']}
        except RuntimeError as e:
            out[link] = {'n': len(rs), 'error': str(e)}
    return out


def write_summary(results, pools, skipped, out):
    rows = []
    for r in results:
        b = r.get('base') or {}
        rows.append({'name': r['name'], 'qber': b.get('qber'), 'skew_ppb': (r.get('peak') or {}).get('skew_ppb'),
                     'sifted_rate': b.get('sifted_bits', 0) / max(r['a'].get('duration_s', 1), 1e-9) if b else None,
                     'asym_key': next((v['key'] for v in (r.get('ec') or {}).values() if not v.get('error')), None)})
    if len(rows) > 1:
        figures.comparison(rows, out / 'comparison.png')
    L = ['### Evaluation summary', '',
         f'**Evaluated:** {len(results)} recording pair(s) on {datetime.now():%Y-%m-%d} by `evaluation/evaluate.py`. Each pair has its own report with figures; this page compares them and pools each link\'s recordings into one block.', '',
         '| Recording | Link | Length | Peak | Skew | Window | Sifted bits | Sample QBER | Asymptotic key | Finite key |',
         '|---|---|---|---|---|---|---|---|---|---|']
    for r in results:
        p, b = r.get('peak') or {}, r.get('base') or {}
        ec = next(((k, v) for k, v in (r.get('ec') or {}).items() if not v.get('error')), (None, {}))[1]
        L.append(f'| [{r["name"]}]({r["name"]}/report.md) | {r["link"]} | {r["a"].get("duration_s", 0):.1f} s | '
                 f'{"score " + format(p["corrected"]["local_score"], ".0f") if p.get("detected") else "not found"} | '
                 f'{"-" if not p else format(p["skew_ppb"], ".2f") + " ppb"} | '
                 f'{"-" if not b else "±" + str(b["window_ps"]) + " ps"} | {b.get("sifted_bits", "-")} | {pct(b.get("qber"))} | '
                 f'{ec.get("key", "-")} | {b.get("final_key_bits", "-")} |')
    if len(rows) > 1:
        L += ['', '![Recordings side by side](comparison.png)']
    if pools:
        L += ['', '### Pooled per link', '',
              'All evaluated recordings of a link in one privacy-amplification block: the QBER sample grows with the pool, so the finite-key bound tightens.', '',
              '| Link | Recordings | Data | Sifted bits | Sample QBER | Asymptotic key | Phase-error bound | Finite key |', '|---|---|---|---|---|---|---|---|']
        for link, p in pools.items():
            if p.get('error'):
                L.append(f'| {link} | {p["n"]} | failed: {p["error"]} | | | | | |')
            else:
                L.append(f'| {link} | {p["n"]} | {p["seconds"]:.0f} s | {p["sifted"]:,} | {pct(p["qber"])} | {p["asym"]:,} | {pct(p["bound"])} | **{p["finite"]:,}** |')
    if skipped:
        L += ['', '### Skipped files', '', '| File | Why |', '|---|---|'] + [f'| `{f}` | {why} |' for f, why in skipped]
    (out / 'summary.md').write_text('\n'.join(L) + '\n')


def main(argv=None):
    ap = argparse.ArgumentParser(description='Evaluates TimeTagger recordings through the whole chain, with figures')
    ap.add_argument('paths', nargs='+', help='recording directories, parent directories (searched recursively) or timestamp files')
    ap.add_argument('--out', type=Path, help='output directory, default <first recording directory>/evaluation/<date>')
    ap.add_argument('--config', type=Path, default=ROOT / 'acquisition' / 'config.json', help='site names and search settings')
    ap.add_argument('--importtt', help='importtt binary; default: built from this checkout with go')
    ap.add_argument('--no-sweep', action='store_true', help='skip the coincidence-window sweep')
    ap.add_argument('--no-pool', action='store_true', help='skip pooling each link\'s recordings')
    ap.add_argument('--jobs', type=int, default=4, help='importtt runs in parallel, about 0.6 GB each on a 300 MB recording')
    args = ap.parse_args(argv)
    args.methods = list(METHODS)

    cfg = json.loads(args.config.read_text()) if args.config.is_file() else {}
    if not cfg:
        log(f'{args.config} not found: sites are named by IP and the search uses its defaults')
    pairs, skipped = find_pairs(args.paths)
    if not pairs:
        sys.exit('no recording pairs found (two *_timestamp_sequence.npy files with the same start time in one directory)')
    # by default next to the recordings, outside this repository: results never belong in it
    first = Path(args.paths[0]).resolve()
    out = args.out or (first if first.is_dir() else first.parent) / 'evaluation' / f'{datetime.now():%Y%m%d_%H%M%S}'
    try:
        out.mkdir(parents=True, exist_ok=True)
    except PermissionError:  # read-only install, as in the tools image: write next to where it was run
        out = Path.cwd() / 'evaluation_results' / out.name
        out.mkdir(parents=True, exist_ok=True)
    it = Importtt(find_importtt(args.importtt))
    log(f'{len(pairs)} pair(s), output in {out}')

    results = []
    staging = Path(tempfile.mkdtemp(prefix='qkd_eval_'))
    try:
        for pair in pairs:
            try:
                res = evaluate_pair(pair, out, staging, it, cfg, args)
            except Exception as e:  # one bad recording must not stop the others
                res = {'name': public_name(pair[0], site_names(cfg)), 'link': '?', 'a': {}, 'b': {},
                       'notes': [f'evaluation failed: {type(e).__name__}: {e}']}
                log(res['notes'][0])
            results.append(res)
            if 'start' in res:
                write_pair_report(res, out / res['name'])
        pools = {} if args.no_pool else pooled(results, it)
    finally:
        shutil.rmtree(staging, ignore_errors=True)
    for r in results:
        r.pop('input', None)
    write_summary(results, pools, [(public_name(f, site_names(cfg)), why) for f, why in skipped], out)
    (out / 'results.json').write_text(json.dumps({'pairs': results, 'pooled': pools}, indent=1, default=float))
    # last line of defence: error messages can quote file names, which carry the IPs
    for f in out.rglob('*'):
        if f.suffix in ('.md', '.json'):
            f.write_text(IP_RE.sub('x.x.x.x', f.read_text()))
    log(f'done: {out / "summary.md"}')
    return 0


if __name__ == '__main__':
    sys.exit(main())
