'''
Coincidence-peak analysis of a recording pair, cached per pair and file version, with the plots the alignment page
draws, and the scratch layout cmd/importtt and chsh_diagnostic.py read a pair from
'''

import collections
import json
import os
import shutil
import tempfile
import threading
import time
from contextlib import contextmanager
from pathlib import Path

import coincidence_peak
from flask import Blueprint, jsonify, request

from ..core import recordings as rec

bp = Blueprint('processing_alignment', __name__)

CACHE_SIZE = 4
PLOT_MAX_POINTS = 500
_cache = collections.OrderedDict()  # pair and file versions, then report, diagnostics, plots
_lock = threading.Lock()


def analysis(path_a, path_b):
    '''Full analyze_arrays(strict=False) over one pair, computed once per pair and file version'''
    key = (path_a.name, path_a.stat().st_mtime_ns, path_b.name, path_b.stat().st_mtime_ns)
    with _lock:
        if key in _cache:
            _cache.move_to_end(key)
            return _cache[key]
        a, b = rec.load_timestamps(path_a), rec.load_timestamps(path_b)
        report, diagnostics = coincidence_peak.analyze_arrays(a, b, strict=False)
        _cache[key] = {'report': report, 'diagnostics': diagnostics, 'plots': None, 'n_a': int(a.shape[0]), 'n_b': int(b.shape[0])}
        while len(_cache) > CACHE_SIZE:
            _cache.popitem(last=False)
        return _cache[key]


def _downsample_max(values, max_points):
    '''Per-bucket max, not a stride: a narrow peak must survive downsampling'''
    n = len(values)
    if n <= max_points:
        return list(range(n)), [float(v) for v in values]
    bucket = -(-n // max_points)
    idx, out = [], []
    for start in range(0, n, bucket):
        chunk = values[start:start + bucket]
        i = max(range(len(chunk)), key=lambda j: chunk[j])
        idx.append(start + i)
        out.append(float(chunk[i]))
    return idx, out


def _histogram_series(counts, bin_ps, radius_ps, window):
    idx, ys = _downsample_max(counts, PLOT_MAX_POINTS)
    xs = [(-radius_ps + (i + 0.5) * bin_ps) / 1000 for i in idx]
    return {'x_ns': xs, 'y': ys, 'window_center_ns': window.center_ps / 1000, 'window_width_ns': window.width_ps / 1000}


def _plots(report, diagnostics):
    import numpy as np
    lags = (np.asarray(diagnostics['coarse_lags_ps']) / 1e9).tolist()
    scores = np.asarray(diagnostics['coarse_scores']).tolist()
    if len(lags) > PLOT_MAX_POINTS:
        idx, scores = _downsample_max(scores, PLOT_MAX_POINTS)
        lags = [lags[i] for i in idx]
    fine = diagnostics['fine_histogram']
    skew = report['clock_skew']
    seg_x = [v / 1e12 for v in skew.get('segment_elapsed_ps', [])]
    seg_y = [(v - skew['lag_at_reference_ps']) / 1000 for v in skew.get('segment_lags_ps', [])]
    ref = skew['reference_elapsed_ps'] / 1e12
    return {
        'coarse': {'x_ms': lags, 'y': scores},
        'fine': _histogram_series(fine.counts, fine.bin_ps, fine.radius_ps, fine.window),
        'corrected': _histogram_series(diagnostics['corrected_counts'], fine.bin_ps, fine.radius_ps, diagnostics['corrected_window']),
        'drift': {'seg_x_s': seg_x, 'seg_y_ns': seg_y, 'fit_y_ns': [skew['slope_ps_per_s'] * (t - ref) / 1000 for t in seg_x]},
    }


@bp.route('/api/align/live')
def api_align_live():
    try:
        path_a, path_b = rec.resolve_pair(request.args.get('a'), request.args.get('b'))
        entry = analysis(path_a, path_b)
    except Exception as e:
        return jsonify({'ready': False, 'reason': str(e)[:200], 'timestamp': int(time.time())})
    report = entry['report']
    if entry['plots'] is None:
        entry['plots'] = _plots(report, entry['diagnostics'])
    shift, skew = report['constant_shift'], report['clock_skew']
    return jsonify({
        'ready': True, 'detected': report['detected'],
        'lag_ns': shift['relative_lag_ps'] / 1000, 'window_ns': shift['window']['width_ps'] / 1000,
        'score': shift['window']['local_score'], 'skew_ppb': skew['skew_ppb'], 'skew_method': skew.get('skew_method'),
        'corrected_window_ns': skew['corrected_window']['width_ps'] / 1000,
        'a': path_a.name, 'b': path_b.name, 'n_a': entry['n_a'], 'n_b': entry['n_b'],
        'plots': entry['plots'], 'timestamp': int(time.time()),
    })


@contextmanager
def prepared_dirs(pairs):
    '''One scratch directory per pair, holding links to its files and its *_coincidence.json, the layout
    cmd/importtt and chsh_diagnostic.py read; refuses a pair without a confirmed peak'''
    dirs, reports = [], []
    try:
        for path_a, path_b in pairs:
            report = dict(analysis(path_a, path_b)['report'])
            if not report['detected']:
                score = report['constant_shift']['window']['local_score']
                raise ValueError(f'{path_a.name} + {path_b.name}: no significant coincidence peak '
                                 f'(score {score:.2f}, need {coincidence_peak.MIN_PEAK_SCORE:.2f})')
            report['input_a'], report['input_b'] = path_a.name, path_b.name
            d = Path(tempfile.mkdtemp(prefix='pair_'))
            dirs.append(d)
            for src in (path_a, path_b):
                for p in rec.group_paths(rec.base(src.name)):
                    os.symlink(p, d / p.name)
            (d / f'{rec.base(path_a.name)}_coincidence.json').write_text(json.dumps(report))
            reports.append(report)
        yield dirs, reports
    finally:
        for d in dirs:
            shutil.rmtree(d, ignore_errors=True)
