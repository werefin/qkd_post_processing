'''
qoj_post_processing dashboard: run the simulated pipeline and watch its
metrics, and check live coincidence-peak alignment on uploaded recordings
Same look/deployment approach as skQCI (Flask+gunicorn, Caddy, Docker)
'''

import collections
import json
import os
import signal
import subprocess
import sys
import threading
import time
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

# /app/... below is Docker's own layout --> default, used whenever an env var is missing, has to work for a plain app.py with no
# env vars set at all, or every local run keeps needing them re-typed by hand after every restart, which is exactly what kept breaking here
_default_qoj_bin = REPO_ROOT / 'qoj_post_processing'
_qoj_bin_raw = os.environ.get('QOJ_BIN') or (
    str(_default_qoj_bin) if _default_qoj_bin.is_file() else 'qoj_post_processing'
)
# bare name (no path separator) is a real PATH lookup, left alone; a
# relative path is resolved for the same reason as the dirs below
QOJ_BIN = str(Path(_qoj_bin_raw).resolve()) if os.sep in _qoj_bin_raw else _qoj_bin_raw
# .resolve(): these get handed to tt_record_dual.py as a subprocess argument
# (api_acquire_start) launched with a *different* cwd (acquisition/), so a
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


def _list_recordings():
    return sorted(p.name for p in RECORDINGS_DIR.glob('*_timestamp_sequence.npy'))


@app.route('/api/status')
def api_status():
    return jsonify({
        'keys_saved': _count_keys(),
        'recordings': _list_recordings(),
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

    if body.get('save_keystore', True):
        args += ['-keystore', str(KEYSTORE_DIR)]

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
    if saved:
        _align_cache['t'] = 0.0  # force the next /api/align/live to recompute
    return jsonify({'saved': saved, 'skipped': skipped, 'recordings': _list_recordings()})


@app.route('/api/recordings')
def api_recordings():
    return jsonify({'files': _list_recordings()})


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
_align_cache = {'t': 0.0, 'data': None}
_ALIGN_CACHE_S = 30.0
_align_lock = threading.Lock()


def _load_timestamps(path):
    import numpy as np
    arr = np.load(path, mmap_mode='r')
    if arr.ndim != 1 or not np.issubdtype(arr.dtype, np.integer) or arr.shape[0] < 2:
        raise ValueError(f'{path.name}: not a valid timestamp array')
    return np.asarray(arr)


def _run_alignment_check():
    files = [RECORDINGS_DIR / n for n in _list_recordings()]
    if len(files) < 2:
        return {'ready': False, 'reason': f'need 2 recordings, have {len(files)}'}
    path_a, path_b = files[-2], files[-1]
    try:
        a = _load_timestamps(path_a)
        b = _load_timestamps(path_b)
        report, _ = coincidence_peak.analyze_arrays(a, b, strict=False)
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
        }
    except Exception as e:
        return {'ready': False, 'reason': str(e)[:200]}


@app.route('/api/align/live')
def api_align_live():
    now = time.time()
    if _align_cache['data'] is not None and now - _align_cache['t'] < _ALIGN_CACHE_S:
        return jsonify(_align_cache['data'])
    with _align_lock:
        now = time.time()
        if _align_cache['data'] is not None and now - _align_cache['t'] < _ALIGN_CACHE_S:
            return jsonify(_align_cache['data'])
        data = _run_alignment_check()
        data['timestamp'] = int(time.time())
        _align_cache.update(t=time.time(), data=data)
    return jsonify(data)


if __name__ == '__main__':
    app.run(host='0.0.0.0', port=9823, debug=True)
