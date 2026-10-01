'''
qoj_post_processing dashboard: run the simulated pipeline and watch its
metrics, and check live coincidence-peak alignment on uploaded recordings
'''

import collections
import csv
import io
import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
from contextlib import contextmanager
from pathlib import Path

from flask import Flask, jsonify, render_template, request
from werkzeug.utils import secure_filename

BASE_DIR = Path(__file__).resolve().parent
REPO_ROOT = BASE_DIR.parent

# Reuse coincidence_peak.py directly (same process, no subprocess) for the
# live alignment check --> it's the exact same code tt_record_dual.py's
# live monitor and the acquisition CLI use, not a reimplementation
sys.path.insert(0, str(REPO_ROOT / 'acquisition'))
import coincidence_peak  # noqa: E402
import chsh_diagnostic  # noqa: E402

# /app/... below is Docker's own layout --> default, used whenever an env var is missing, has to work for a plain app.py with no
# env vars set at all, or every local run keeps needing them re-typed by hand after every restart, which is exactly what kept breaking here
_default_qoj_bin = REPO_ROOT / 'qoj_post_processing'
_qoj_bin_raw = os.environ.get('QOJ_BIN') or (
    str(_default_qoj_bin) if _default_qoj_bin.is_file() else 'qoj_post_processing'
)
# bare name (no path separator) is a real PATH lookup, left alone; a
# relative path is resolved for the same reason as the dirs below
QOJ_BIN = str(Path(_qoj_bin_raw).resolve()) if os.sep in _qoj_bin_raw else _qoj_bin_raw

# cmd/importtt: the real chain over an actual recording, used by
# /api/align/run once a peak is confirmed --> see api_align_run()
_default_importtt_bin = REPO_ROOT / 'importtt'
_importtt_bin_raw = os.environ.get('IMPORTTT_BIN') or (
    str(_default_importtt_bin) if _default_importtt_bin.is_file() else 'importtt'
)
IMPORTTT_BIN = str(Path(_importtt_bin_raw).resolve()) if os.sep in _importtt_bin_raw else _importtt_bin_raw

# Real keys (from /api/align/run) are never evicted to make room, so keystore needs a ceiling of its own
KEYSTORE_MAX = int(os.environ.get('KEYSTORE_MAX', '30000'))

# .resolve(): these get handed to tt_record_dual.py as a subprocess argument
# (api_acquire_start) launched with a different cwd (acquisition/), so a
# relative env var value here would resolve against the wrong directory
# there even though it looked fine wherever this process itself started
KEYSTORE_DIR = Path(os.environ.get('KEYSTORE_DIR') or (BASE_DIR / 'keys')).resolve()
RECORDINGS_DIR = Path(os.environ.get('RECORDINGS_DIR') or (BASE_DIR / 'recordings')).resolve()
CONFIG_DIR = Path(os.environ.get('CONFIG_DIR') or (BASE_DIR / 'config')).resolve()
KEYSTORE_DIR.mkdir(parents=True, exist_ok=True)
RECORDINGS_DIR.mkdir(parents=True, exist_ok=True)
CONFIG_DIR.mkdir(parents=True, exist_ok=True)
UPLOADED_CONFIG_PATH = CONFIG_DIR / 'config.json'

# Recordings (e.g., from Trnava: slow link, huge files) can be very large --> generous cap by default, not Flask's default 16MB
# set UPLOAD_MAX_MB=0 for no limit at all --> matched by gunicorn's --timeout in the Dockerfile CMD, so
# a big upload over a slow link doesn't get killed mid-transfer before it ever hits this size check
UPLOAD_MAX_MB = int(os.environ.get('UPLOAD_MAX_MB', '102400'))  # 100 GiB

# Unlike the keystore, recordings are raw, irreplaceable TimeTagger captures
# never auto-delete them by default. 0 = unlimited/disabled; set this to
# opt into oldest-first pruning once RECORDINGS_DIR passes the cap, see
# _enforce_recordings_cap()
RECORDINGS_MAX_GB = float(os.environ.get('RECORDINGS_MAX_GB', '0'))

app = Flask(__name__)
app.config['MAX_CONTENT_LENGTH'] = (UPLOAD_MAX_MB * 1024 * 1024) or None

_RECORDING_SUFFIXES = ('_timestamp_sequence.npy', '_channel_sequence.npy', '_meta.json')


@app.after_request
def _security_headers(resp):
    resp.headers['X-Content-Type-Options'] = 'nosniff'
    resp.headers['X-Frame-Options'] = 'DENY'
    resp.headers['Referrer-Policy'] = 'no-referrer'
    resp.headers['Permissions-Policy'] = 'geolocation=(), microphone=(), camera=()'
    resp.headers['Content-Security-Policy'] = (
        "default-src 'self'; "
        "script-src 'self' 'unsafe-inline'; "
        "style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; "
        "font-src 'self' https://fonts.gstatic.com; "
        "img-src 'self' data:; "
        "connect-src 'self'; "
        "object-src 'none'; base-uri 'self'; frame-ancestors 'none'"
    )
    resp.headers.pop('Server', None)
    return resp


@app.route('/')
def index():
    return render_template('index.html')


def _count_keys():
    return sum(1 for _ in KEYSTORE_DIR.glob('*.json'))


def _keystore_snapshot():
    return set(KEYSTORE_DIR.glob('*.json'))


# Tracks exactly which keystore files the last /api/run (the simulator)
# produced, so the NEXT simulation can deplete just that batch before it
# runs -- see api_run(). Real keys from /api/align/run are never added here
# and so are never touched by this
_last_sim_keys = set()


def _deplete_last_sim_keys():
    global _last_sim_keys
    for p in _last_sim_keys:
        try:
            p.unlink()
        except OSError:
            pass
    _last_sim_keys = set()


def _list_recordings():
    return sorted(p.name for p in RECORDINGS_DIR.glob('*_timestamp_sequence.npy'))


def _recording_base(ts_name):
    return ts_name.removesuffix('_timestamp_sequence.npy')


def _recording_group_paths(base):
    '''All files belonging to one recording (timestamp + channel + meta),
    whichever of them actually exist'''
    return [p for suffix in _RECORDING_SUFFIXES if (p := RECORDINGS_DIR / f'{base}{suffix}').is_file()]


def _recordings_total_bytes():
    return sum(p.stat().st_size for p in RECORDINGS_DIR.glob('*') if p.is_file())


def _delete_recording(base):
    '''Removes one recording's files (all of timestamp/channel/meta that
    exist). Returns how many bytes were freed'''
    freed = 0
    for p in _recording_group_paths(base):
        try:
            freed += p.stat().st_size
            p.unlink()
        except OSError:
            pass
    return freed


def _enforce_recordings_cap():
    '''Opt-in (RECORDINGS_MAX_GB > 0): deletes the OLDEST recordings, one at
    a time, until RECORDINGS_DIR is back under the cap --> raw TimeTagger
    captures are irreplaceable, so this never runs unless explicitly
    configured, see RECORDINGS_MAX_GB'''
    if RECORDINGS_MAX_GB <= 0:
        return
    cap_bytes = RECORDINGS_MAX_GB * 1024 ** 3
    names = _list_recordings()  # sorted --> oldest (by recording timestamp prefix) first
    i = 0
    while _recordings_total_bytes() > cap_bytes and i < len(names):
        base = _recording_base(names[i])
        freed = _delete_recording(base)
        if freed:
            print(f'[recordings] RECORDINGS_MAX_GB={RECORDINGS_MAX_GB:g} exceeded, '
                  f'deleted oldest recording {base!r} ({freed / 1024**2:.1f} MiB)')
        i += 1


@app.route('/api/status')
def api_status():
    return jsonify({
        'keys_saved': _count_keys(),
        'recordings': _list_recordings(),
        'recordings_bytes': _recordings_total_bytes(),
        'recordings_max_gb': RECORDINGS_MAX_GB,
        'timestamp': int(time.time()),
    })


# API: run the simulated pipeline (cmd/qoj_post_processing -json) and
# return its metrics --> the JS keeps a rolling history for the sparklines
@app.route('/api/run', methods=['POST'])
def api_run():
    body = request.get_json(silent=True) or {}

    def add(args, flag, key, cast):
        v = body.get(key)
        if v not in (None, ''):
            args += [flag, str(cast(v))]

    method = body.get('method', 'discard')

    args = [QOJ_BIN, '-json']
    add(args, '-n', 'n', int)
    add(args, '-err', 'err', float)
    add(args, '-multiphoton', 'multiphoton', float)
    add(args, '-window', 'window', int)
    add(args, '-jitter', 'jitter', int)
    add(args, '-seed', 'seed', int)

    if method == 'winnow':
        args.append('-winnow')
        add(args, '-winnow-blocks', 'winnow_blocks', str)
        add(args, '-winnow-sample', 'winnow_sample', float)
    elif method == 'ldpc':
        args.append('-ldpc')
        add(args, '-ldpc-block', 'ldpc_block', int)
        add(args, '-ldpc-wc', 'ldpc_wc', int)
        add(args, '-ldpc-wr', 'ldpc_wr', int)
        add(args, '-ldpc-iterations', 'ldpc_iterations', int)
        add(args, '-ldpc-sample', 'ldpc_sample', float)
    else:
        if body.get('search_matrix'):
            args.append('-search-matrix')
            add(args, '-search-m', 'search_m', int)
            add(args, '-search-p', 'search_p', int)
        else:
            add(args, '-matrix', 'matrix', str)

    # Winnow/LDPC's own syndrome exchange needs a much finer CRC chunk than
    # block-discard's default (see -chunk's help text): only default it down
    # when the caller didn't pick one explicitly
    if 'chunk' in body and body['chunk'] not in (None, ''):
        add(args, '-chunk', 'chunk', int)
    elif method in ('winnow', 'ldpc'):
        args += ['-chunk', '128']

    save_keystore = body.get('save_keystore', True)
    if save_keystore:
        args += ['-keystore', str(KEYSTORE_DIR)]
        # A fresh simulation replaces the last one's demo keys rather than
        # piling up next to them: deplete the previous batch now, before
        # this run adds its own -- see _last_sim_keys. Real keys from
        # /api/align/run are never in that set, so they're never touched
        _deplete_last_sim_keys()

    before = _keystore_snapshot()
    try:
        proc = subprocess.run(args, capture_output=True, text=True, timeout=120)
    except subprocess.TimeoutExpired:
        return jsonify({'error': 'pipeline run timed out'}), 504
    except FileNotFoundError:
        return jsonify({'error': f'{QOJ_BIN} not found on PATH'}), 500

    if proc.returncode != 0:
        return jsonify({'error': (proc.stderr or 'pipeline failed').strip()[:400]}), 500
    try:
        metrics = json.loads(proc.stdout.strip().splitlines()[-1])
    except (ValueError, IndexError):
        return jsonify({'error': 'could not parse pipeline output'}), 500
    if metrics.get('error'):
        return jsonify({'error': metrics['error']}), 500

    # This batch of demo keys stays in the keystore (and in the gauge/count)
    # until the *next* simulation depletes it above -- unlike a one-off
    # purge-immediately-after approach, this actually lets the UI show what
    # a run produced instead of always reading back 0
    if save_keystore:
        global _last_sim_keys
        _last_sim_keys = _keystore_snapshot() - before
    metrics['keys_in_store'] = _count_keys()
    return jsonify(metrics)


# API: upload recording files (esp. trnava's, copied off the box by hand
# since its link is too slow/too much data for a live remote connection)
@app.route('/api/upload', methods=['POST'])
def api_upload():
    saved = []
    skipped = []
    for f in request.files.getlist('files'):
        name = secure_filename(f.filename or '')
        if not name or not name.endswith(_RECORDING_SUFFIXES):
            skipped.append(f.filename or '(unnamed)')
            continue
        f.save(RECORDINGS_DIR / name)
        saved.append(name)
    # No manual cache-bust needed: _get_analysis() keys on file mtime, so a
    # newly saved recording is picked up on the very next analysis call
    if saved:
        _enforce_recordings_cap()
    return jsonify({
        'saved': saved, 'skipped': skipped, 'recordings': _list_recordings(),
        'recordings_bytes': _recordings_total_bytes(),
    })


@app.route('/api/recordings')
def api_recordings():
    names = _list_recordings()
    items = [{
        'base': _recording_base(n),
        'name': n,
        'bytes': sum(p.stat().st_size for p in _recording_group_paths(_recording_base(n))),
    } for n in names]
    return jsonify({
        'files': names, 'items': items,
        'total_bytes': _recordings_total_bytes(), 'max_gb': RECORDINGS_MAX_GB,
    })


@app.route('/api/recordings', methods=['DELETE'])
def api_delete_all_recordings():
    '''Bulk cleanup: deletes every recording currently listed, same
    one-at-a-time removal as api_delete_recording(), just for all of them'''
    freed = 0
    deleted = []
    for n in _list_recordings():
        base = _recording_base(n)
        freed += _delete_recording(base)
        deleted.append(base)
    return jsonify({'deleted': deleted, 'freed_bytes': freed, 'recordings': _list_recordings()})


@app.route('/api/recordings/<base>', methods=['DELETE'])
def api_delete_recording(base):
    '''Manual cleanup: removes one recording's files regardless of
    RECORDINGS_MAX_GB, so disk space can be managed deliberately once a
    recording's key has been extracted and it's no longer needed'''
    base = secure_filename(base)
    if not (RECORDINGS_DIR / f'{base}_timestamp_sequence.npy').is_file():
        return jsonify({'error': f'no such recording: {base}'}), 404
    freed = _delete_recording(base)
    return jsonify({'deleted': base, 'freed_bytes': freed, 'recordings': _list_recordings()})


# API: load config.json (uploaded, or pasted as raw JSON) --> same shape
# acquisition/config.json documents, validated the same way tt_record_dual.py's
# load_config() does, so "start" below can hand it straight to that script
def _validate_config(cfg):
    if not isinstance(cfg.get('sites'), dict) or not cfg['sites']:
        return 'missing or empty "sites"'
    if not isinstance(cfg.get('links'), dict) or not cfg['links']:
        return 'missing or empty "links"'
    for name, link in cfg['links'].items():
        for key in ('site_a', 'site_b'):
            if link.get(key) not in cfg['sites']:
                return f'links.{name}.{key} not found in "sites"'
    # tt_record_dual.py's main() indexes these directly (not just load_config()'s checks)
    # so a config missing them crashes the acquisition subprocess with a
    # raw traceback instead of a clean error --> catch it here first
    rec = cfg.get('record_dual')
    if not isinstance(rec, dict):
        return 'missing "record_dual"'
    for key in ('stop_mode', 'record_duration_s', 'target_detection_count', 'output_dir'):
        if key not in rec:
            return f'record_dual.{key} is required'
    return None


def _config_summary(cfg):
    return {
        'sites': sorted(cfg.get('sites', {})),
        'links': [{'name': n, 'site_a': l['site_a'], 'site_b': l['site_b']}
                  for n, l in sorted(cfg.get('links', {}).items())],
        'default_link': (cfg.get('record_dual') or {}).get('link'),
    }


@app.route('/api/config', methods=['GET', 'POST'])
def api_config():
    if request.method == 'GET':
        if not UPLOADED_CONFIG_PATH.is_file():
            return jsonify({'loaded': False})
        cfg = json.loads(UPLOADED_CONFIG_PATH.read_text())
        return jsonify({'loaded': True, **_config_summary(cfg)})

    raw = None
    if request.files.get('file'):
        raw = request.files['file'].read()
    elif request.data:
        raw = request.data
    else:
        body = request.get_json(silent=True)
        if body is not None:
            raw = json.dumps(body).encode()
    if raw is None:
        return jsonify({'error': 'no config file or JSON body given'}), 400

    try:
        cfg = json.loads(raw)
    except ValueError as e:
        return jsonify({'error': f'invalid JSON: {e}'}), 400
    err = _validate_config(cfg)
    if err:
        return jsonify({'error': err}), 400

    UPLOADED_CONFIG_PATH.write_text(json.dumps(cfg, indent=2))
    return jsonify({'loaded': True, **_config_summary(cfg)})


# API: start/stop/watch a real tt_record_dual.py acquisition run, driven by
# the uploaded config, writing into the same RECORDINGS_DIR the upload and
# alignment endpoints already use --> "load the config, then start everything"
# Needs real network reachability to the configured TimeTaggers from
# wherever this container runs; it won't do anything useful otherwise (see
# the README note on trnava's link being too slow/too large to drive remotely)
_acquire_lock = threading.Lock()
_acquire = {'proc': None, 'log': collections.deque(maxlen=1000), 'started_at': None}


def _read_acquire_output(proc):
    for line in proc.stdout:
        _acquire['log'].append(line.rstrip('\n'))
    proc.wait()
    # tt_record_dual.py just wrote this recording's *.npy files (or decided
    # not to, on a failed run) --> runs once per completed recording, not on
    # every status poll, see RECORDINGS_MAX_GB
    _enforce_recordings_cap()


@app.route('/api/acquire/start', methods=['POST'])
def api_acquire_start():
    if not UPLOADED_CONFIG_PATH.is_file():
        return jsonify({'error': 'no config loaded yet, POST one to /api/config first'}), 400
    body = request.get_json(silent=True) or {}
    link = body.get('link')
    duration_s = body.get('duration_s', 60)

    with _acquire_lock:
        if _acquire['proc'] is not None and _acquire['proc'].poll() is None:
            return jsonify({'error': 'a recording is already running'}), 409

        script = REPO_ROOT / 'acquisition' / 'tt_record_dual.py'
        args = [sys.executable, str(script), str(UPLOADED_CONFIG_PATH),
                '--duration', str(duration_s), '--output-dir', str(RECORDINGS_DIR)]
        if link:
            args += ['--link', link]

        _acquire['log'].clear()
        try:
            proc = subprocess.Popen(
                args, cwd=str(REPO_ROOT / 'acquisition'),
                stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                text=True, bufsize=1,
            )
        except OSError as e:
            return jsonify({'error': f'could not start recording: {e}'}), 500
        _acquire['proc'] = proc
        _acquire['started_at'] = time.time()
        threading.Thread(target=_read_acquire_output, args=(proc,), daemon=True).start()
    return jsonify({'started': True})


@app.route('/api/acquire/stop', methods=['POST'])
def api_acquire_stop():
    with _acquire_lock:
        proc = _acquire['proc']
        if proc is None or proc.poll() is not None:
            return jsonify({'error': 'no recording is running'}), 409
        proc.send_signal(signal.SIGINT)  # tt_record_dual.py handles this like Ctrl+C
    return jsonify({'stopping': True})


@app.route('/api/acquire/status')
def api_acquire_status():
    proc = _acquire['proc']
    running = proc is not None and proc.poll() is None
    return jsonify({
        'running': running,
        'returncode': None if proc is None else proc.poll(),
        'started_at': _acquire['started_at'],
        'log': list(_acquire['log'])[-200:],
    })


# API: coincidence-peak alignment check over the two most recently
# uploaded/acquired recordings --> the real, full analyze_arrays() call,
# clock-skew fit included, same as coincidence_peak.py's own CLI and the
# final report cmd/importtt reads --> not the fast fit_skew=False variant
# tt_record_dual.py's live monitor uses while still recording, since these
# are complete, already-uploaded files: full result is what matters
_analysis_cache = {'key': None, 'report': None, 'diagnostics': None}
_analysis_lock = threading.Lock()


def _load_timestamps(path):
    import numpy as np
    arr = np.load(path, mmap_mode='r')
    if arr.ndim != 1 or not np.issubdtype(arr.dtype, np.integer) or arr.shape[0] < 2:
        raise ValueError(f'{path.name}: not a valid timestamp array')
    return np.asarray(arr)


def _get_analysis(path_a, path_b):
    '''Cached analyze_arrays(strict=False) over this exact pair of files
    always non-strict so one computation serves every caller; a caller that
    needs strict (refuse an unconfirmed peak) checks report["detected"]
    itself, see _prepared_recording_dir()'''
    key = (path_a.name, path_a.stat().st_mtime_ns, path_b.name, path_b.stat().st_mtime_ns)
    if _analysis_cache['key'] == key:
        return _analysis_cache['report'], _analysis_cache['diagnostics']
    with _analysis_lock:
        if _analysis_cache['key'] == key:
            return _analysis_cache['report'], _analysis_cache['diagnostics']
        a = _load_timestamps(path_a)
        b = _load_timestamps(path_b)
        report, diagnostics = coincidence_peak.analyze_arrays(a, b, strict=False)
        _analysis_cache.update(key=key, report=report, diagnostics=diagnostics)
    return report, diagnostics


# Max points sent per plot series --> these feed <canvas> line charts in the
# browser, not a print-resolution figure, so a few hundred points is plenty
_PLOT_MAX_POINTS = 500


def _downsample_max(values, max_points):
    '''Per-bucket max, not a stride: a real coincidence peak is a handful of
    bins tall against a flat background, and naive striding can step right
    over it. Returns (indices, values) so the caller can still compute each
    kept point's real x position'''
    n = len(values)
    if n <= max_points:
        return list(range(n)), [float(v) for v in values]
    bucket = -(-n // max_points)  # ceil
    idx, out = [], []
    for start in range(0, n, bucket):
        chunk = values[start:start + bucket]
        local_i = max(range(len(chunk)), key=lambda i: chunk[i])
        idx.append(start + local_i)
        out.append(float(chunk[local_i]))
    return idx, out


def _histogram_series(hist):
    '''hist: a coincidence_peak.HistogramResult (or corrected_counts + the
    same fine bin/radius) --> (x_ns, y, window_center_ns, window_width_ns),
    downsampled the same way the coarse search's own plot points already are'''
    idx, counts = _downsample_max(hist.counts, _PLOT_MAX_POINTS)
    xs_ns = [(-hist.radius_ps + (i + 0.5) * hist.bin_ps) / 1000 for i in idx]
    return xs_ns, counts, hist.window.center_ps / 1000, hist.window.width_ps / 1000


def _serialize_plots(report, diagnostics):
    '''Same underlying data coincidence_peak.py's save_plot() draws with
    matplotlib (coarse correlation, best constant-shift peak, clock-rate
    drift, clock-corrected peak), reshaped for the dashboard's own canvas
    charts instead --> see main.js's drawCoarsePlot/drawHistPlot/drawDriftPlot'''
    import numpy as np

    coarse_lags_ms = (np.asarray(diagnostics['coarse_lags_ps']) / 1e9).tolist()
    coarse_scores = np.asarray(diagnostics['coarse_scores']).tolist()
    if len(coarse_lags_ms) > _PLOT_MAX_POINTS:
        idx, coarse_scores = _downsample_max(coarse_scores, _PLOT_MAX_POINTS)
        coarse_lags_ms = [coarse_lags_ms[i] for i in idx]

    fine = diagnostics['fine_histogram']
    fine_xs_ns, fine_counts, fine_center_ns, fine_width_ns = _histogram_series(fine)

    # corrected_counts is a bare array sharing fine's bin/radius (see save_plot's own corrected_centers_ns computation)
    class _Corrected:
        pass
    corr = _Corrected()
    corr.counts = diagnostics['corrected_counts']
    corr.bin_ps = fine.bin_ps
    corr.radius_ps = fine.radius_ps
    corr.window = diagnostics['corrected_window']
    corr_xs_ns, corr_counts, corr_center_ns, corr_width_ns = _histogram_series(corr)

    skew = report['clock_skew']
    seg_elapsed_s = [v / 1e12 for v in skew.get('segment_elapsed_ps', [])]
    seg_lag_ns = [(v - skew['lag_at_reference_ps']) / 1000 for v in skew.get('segment_lags_ps', [])]
    reference_s = skew['reference_elapsed_ps'] / 1e12
    fit_ns = [skew['slope_ps_per_s'] * (t - reference_s) / 1000 for t in seg_elapsed_s]

    return {
        'coarse': {'x_ms': coarse_lags_ms, 'y': coarse_scores},
        'fine': {'x_ns': fine_xs_ns, 'y': fine_counts, 'window_center_ns': fine_center_ns, 'window_width_ns': fine_width_ns},
        'corrected': {'x_ns': corr_xs_ns, 'y': corr_counts, 'window_center_ns': corr_center_ns, 'window_width_ns': corr_width_ns},
        'drift': {'seg_x_s': seg_elapsed_s, 'seg_y_ns': seg_lag_ns, 'fit_y_ns': fit_ns},
    }


def _run_alignment_check():
    files = [RECORDINGS_DIR / n for n in _list_recordings()]
    if len(files) < 2:
        return {'ready': False, 'reason': f'need 2 recordings, have {len(files)}'}
    path_a, path_b = files[-2], files[-1]
    try:
        # mmap'd, so this is just a header read --> the expensive part is the
        # FFT search inside _get_analysis(), which is cached by file identity
        a = _load_timestamps(path_a)
        b = _load_timestamps(path_b)
        report, diagnostics = _get_analysis(path_a, path_b)
        shift = report['constant_shift']
        skew = report['clock_skew']
        return {
            'ready': True,
            'detected': report['detected'],
            'lag_ns': shift['relative_lag_ps'] / 1000,
            'window_ns': shift['window']['width_ps'] / 1000,
            'score': shift['window']['local_score'],
            'skew_ppb': skew['skew_ppb'],
            'skew_method': skew.get('skew_method'),
            'corrected_window_ns': skew['corrected_window']['width_ps'] / 1000,
            'a': path_a.name,
            'b': path_b.name,
            'n_a': int(a.shape[0]),
            'n_b': int(b.shape[0]),
            'plots': _serialize_plots(report, diagnostics),
        }
    except Exception as e:
        return {'ready': False, 'reason': str(e)[:200]}


@app.route('/api/align/live')
def api_align_live():
    # No time-based cache needed here any more: _get_analysis() already
    # caches the expensive part by file identity, so a cache hit here is
    # just cheap dict/array bookkeeping, and a genuinely new upload is
    # reflected on the very next poll instead of waiting out a timer
    data = _run_alignment_check()
    data['timestamp'] = int(time.time())
    return jsonify(data)


@contextmanager
def _prepared_recording_dir():
    '''Both cmd/importtt and chsh_diagnostic.py's own find_recording() want
    a directory holding exactly this pair's files plus one matching
    *_coincidence.json report; RECORDINGS_DIR can hold many uploaded
    recordings over time, so this builds a scoped scratch dir instead of
    writing the report next to everything else in there. Yields
    (run_dir, path_a, path_b); raises ValueError/whatever analyze_arrays
    raises on any precondition failure -- see api_align_run()/api_chsh_run()'''
    files = [RECORDINGS_DIR / n for n in _list_recordings()]
    if len(files) < 2:
        raise ValueError(f'need 2 recordings, have {len(files)}')
    path_a, path_b = files[-2], files[-1]

    # Same cached analysis /api/align/live already (maybe) computed for this
    # exact pair --> strict=True's only real job is refusing an unconfirmed
    # peak, reproduced here instead of recomputing just to get that check
    cached_report, _diagnostics = _get_analysis(path_a, path_b)
    if not cached_report['detected']:
        score = cached_report['constant_shift']['window']['local_score']
        raise ValueError(
            f'no significant coincidence peak found (best local score {score:.2f}, '
            f'required {coincidence_peak.MIN_PEAK_SCORE:.2f})'
        )
    report = dict(cached_report)  # don't let input_a/input_b below leak into the shared cache
    report['input_a'] = path_a.name
    report['input_b'] = path_b.name

    run_dir = Path(tempfile.mkdtemp(prefix='align_run_', dir=str(BASE_DIR)))
    try:
        for src in (path_a, path_b):
            base = src.name.removesuffix('_timestamp_sequence.npy')
            for suffix in _RECORDING_SUFFIXES:
                sibling = RECORDINGS_DIR / f'{base}{suffix}'
                if sibling.is_file():
                    shutil.copy2(sibling, run_dir / sibling.name)
        report_base = path_a.name.removesuffix('_timestamp_sequence.npy')
        (run_dir / f'{report_base}_coincidence.json').write_text(json.dumps(report))
        yield run_dir, path_a, path_b, report
    finally:
        shutil.rmtree(run_dir, ignore_errors=True)


# API: run the real chain (cmd/importtt -json) over the same two recordings
# /api/align/live checks --> sift, error-correct with the chosen method,
# privacy-amplify, save the distilled key. Unlike api_run() this is a real
# recording's key, so it is never purged, see _last_sim_keys
@app.route('/api/align/run', methods=['POST'])
def api_align_run():
    body = request.get_json(silent=True) or {}
    try:
        with _prepared_recording_dir() as (run_dir, path_a, path_b, _report):
            method = body.get('method', 'discard')
            args = [IMPORTTT_BIN, '-json', '-dir', str(run_dir), '-keystore', str(KEYSTORE_DIR), '-keystore-max', str(KEYSTORE_MAX)]

            def add(args, flag, key, cast):
                v = body.get(key)
                if v not in (None, ''):
                    args += [flag, str(cast(v))]

            if method == 'winnow':
                args.append('-winnow')
                add(args, '-winnow-blocks', 'winnow_blocks', str)
                add(args, '-winnow-sample', 'winnow_sample', float)
            elif method == 'ldpc':
                args.append('-ldpc')
                add(args, '-ldpc-block', 'ldpc_block', int)
                add(args, '-ldpc-wc', 'ldpc_wc', int)
                add(args, '-ldpc-wr', 'ldpc_wr', int)
                add(args, '-ldpc-iterations', 'ldpc_iterations', int)
                add(args, '-ldpc-sample', 'ldpc_sample', float)
            else:
                add(args, '-matrix', 'matrix', str)

            if 'chunk' in body and body['chunk'] not in (None, ''):
                add(args, '-chunk', 'chunk', int)
            elif method in ('winnow', 'ldpc'):
                args += ['-chunk', '128']

            try:
                proc = subprocess.run(args, capture_output=True, text=True, timeout=600)
            except subprocess.TimeoutExpired:
                return jsonify({'error': 'real pipeline run timed out'}), 504
            except FileNotFoundError:
                return jsonify({'error': f'{IMPORTTT_BIN} not found on PATH'}), 500
    except ValueError as e:
        return jsonify({'error': str(e)}), 400
    except Exception as e:
        return jsonify({'error': str(e)[:300]}), 400

    if proc.returncode != 0:
        return jsonify({'error': (proc.stderr or 'real pipeline failed').strip()[:400]}), 500
    try:
        metrics = json.loads(proc.stdout.strip().splitlines()[-1])
    except (ValueError, IndexError):
        return jsonify({'error': 'could not parse importtt output'}), 500
    if metrics.get('error'):
        return jsonify({'error': metrics['error']}), 500

    metrics['keys_in_store'] = _count_keys()
    return jsonify(metrics)


# CHSH/Bell diagnostic: reuses acquisition/chsh_diagnostic.py's build_blocks()
# in-process (same pattern as coincidence_peak above) over the same two
# recordings, and caches the full per-block table for /api/chsh/download
_chsh_last = {'columns': None, 'rows': None, 'filename': None}


def num(body, key, cast):
    v = body.get(key)
    return None if v in (None, '') else cast(v)


# chsh_diagnostic.py's own CLI treats --roi2-offset-ns as required, with no
# safe default, because a PULSED source needs the offset to land exactly one
# (or more) rep periods away
_ROI2_OFFSET_WINDOW_MULTIPLE = 200
_ROI2_OFFSET_FLOOR_NS = 20.0


@app.route('/api/chsh/run', methods=['POST'])
def api_chsh_run():
    body = request.get_json(silent=True) or {}

    try:
        with _prepared_recording_dir() as (run_dir, path_a, path_b, report):
            roi2_offset_ns = num(body, 'roi2_offset_ns', float)
            auto_roi2_offset = roi2_offset_ns is None
            if auto_roi2_offset:
                window_ns = report['clock_skew']['corrected_window']['width_ps'] / 1000
                roi2_offset_ns = max(window_ns * _ROI2_OFFSET_WINDOW_MULTIPLE, _ROI2_OFFSET_FLOOR_NS)

            metadata, columns, rows = chsh_diagnostic.build_blocks(
                run_dir,
                block_s=num(body, 'block_s', float) or 1.0,
                roi1_width_ns=num(body, 'roi1_width_ns', float),
                roi1_center_ns=num(body, 'roi1_center_ns', float),
                roi2_offset_ns=roi2_offset_ns,
                roi2_width_ns=num(body, 'roi2_width_ns', float),
            )
    except ValueError as e:
        return jsonify({'error': str(e)}), 400
    except Exception as e:
        return jsonify({'error': str(e)[:300]}), 400

    # Per-pair totals across all blocks --> roi1 is signal+accidental, roi2 is
    # an equal-rate accidental-only baseline at the same width (scaled if
    # not), see build_blocks' own metadata note on background subtraction
    roi1_w = metadata['roi1']['width_ps']
    roi2_w = metadata['roi2']['width_ps']
    bg_scale = roi1_w / roi2_w if roi2_w else 1.0
    pairs = []
    for name in metadata['pair_order']:
        i1 = columns.index(f'roi1_{name}')
        i2 = columns.index(f'roi2_{name}')
        roi1_total = sum(r[i1] for r in rows)
        roi2_total = sum(r[i2] for r in rows)
        pairs.append({
            'pair': name,
            'roi1_total': roi1_total,
            'roi2_total': roi2_total,
            'subtracted': round(roi1_total - roi2_total * bg_scale, 2),
        })

    _chsh_last['columns'] = columns
    _chsh_last['rows'] = rows
    _chsh_last['filename'] = f'{path_a.name.removesuffix("_timestamp_sequence.npy")}_chsh_blocks.csv'

    return jsonify({
        'metadata': metadata,
        'pairs': pairs,
        'a': path_a.name,
        'b': path_b.name,
        'roi2_offset_auto': auto_roi2_offset,
    })


@app.route('/api/chsh/download')
def api_chsh_download():
    if _chsh_last['columns'] is None:
        return jsonify({'error': 'no CHSH diagnostic run yet'}), 404
    buf = io.StringIO()
    writer = csv.writer(buf)
    writer.writerow(_chsh_last['columns'])
    writer.writerows(_chsh_last['rows'])
    resp = app.response_class(buf.getvalue(), mimetype='text/csv')
    resp.headers['Content-Disposition'] = f'attachment; filename="{_chsh_last["filename"]}"'
    return resp


if __name__ == '__main__':
    app.run(host='0.0.0.0', port=9823, debug=True)
