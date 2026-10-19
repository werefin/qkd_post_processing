'''
qkd_post_processing dashboard: one node's web front end. Simulates the pipeline, records and aligns
real recordings, extracts keys into the KMS link pools, and shows the key management system
'''

import base64
import collections
import csv
import hashlib
import io
import itertools
import json
import os
import queue
import secrets
import shutil
import signal
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from contextlib import contextmanager
from pathlib import Path

from flask import Flask, jsonify, render_template, request, send_file
from werkzeug.utils import secure_filename

BASE_DIR = Path(__file__).resolve().parent
REPO_ROOT = BASE_DIR.parent

# coincidence_peak.py and chsh_diagnostic.py run in this process, the same code the CLI uses
sys.path.insert(0, str(REPO_ROOT / 'acquisition'))
import coincidence_peak  # noqa: E402
import chsh_diagnostic  # noqa: E402
import coincidences  # noqa: E402


def _binary(env, name):
    '''A binary from env, else one built at the repository root, else a PATH lookup'''
    raw = os.environ.get(env) or (str(REPO_ROOT / name) if (REPO_ROOT / name).is_file() else name)
    return str(Path(raw).resolve()) if os.sep in raw else raw


QKD_BIN = _binary('QKD_BIN', 'qkd_post_processing')
IMPORTTT_BIN = _binary('IMPORTTT_BIN', 'importtt')


def _dir(env, default):
    p = Path(os.environ.get(env) or default).resolve()
    p.mkdir(parents=True, exist_ok=True)
    return p


# Simulator keys are demo material and live apart from IMPORT_DIR, which the KMS imports from
KEYSTORE_DIR = _dir('KEYSTORE_DIR', BASE_DIR / 'keys')
RECORDINGS_DIR = _dir('RECORDINGS_DIR', BASE_DIR / 'recordings')
CONFIG_DIR = _dir('CONFIG_DIR', BASE_DIR / 'config')
IMPORT_DIR = _dir('IMPORT_DIR', BASE_DIR / 'import')
# continuous-QKD runs and evaluation reports: made from real data, so kept on this node only
CONTINUOUS_DIR = _dir('CONTINUOUS_DIR', BASE_DIR / 'continuous')
EVAL_DIR = _dir('EVAL_DIR', BASE_DIR / 'evaluations')
CLOG_DIR = _dir('CLOG_DIR', BASE_DIR / 'coincidence_logs')
UPLOADED_CONFIG_PATH = CONFIG_DIR / 'config.json'
DELAYS_PATH = CONFIG_DIR / 'detector_delays.json'
JOBS_PATH = CONFIG_DIR / 'jobs.jsonl'

KEYSTORE_MAX = int(os.environ.get('KEYSTORE_MAX', '30000'))
UPLOAD_MAX_MB = int(os.environ.get('UPLOAD_MAX_MB', '102400'))
# raw captures are irreplaceable: never deleted automatically unless a cap is set
RECORDINGS_MAX_GB = float(os.environ.get('RECORDINGS_MAX_GB', '0'))

NODE_ID = os.environ.get('NODE_ID', '')
KMS_URL = os.environ.get('KMS_URL', '').rstrip('/')
KMS_CA = os.environ.get('KMS_CA', '')
KMS_IDENTITIES = {
    'admin': (os.environ.get('KMS_ADMIN_CERT', ''), os.environ.get('KMS_ADMIN_KEY', '')),
    'sae': (os.environ.get('KMS_SAE_CERT', ''), os.environ.get('KMS_SAE_KEY', '')),
}
SAE_ID = f'dashboard-{NODE_ID}' if NODE_ID else ''

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
        "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; "
        "font-src 'self' https://fonts.gstatic.com; img-src 'self' data:; connect-src 'self'; "
        "object-src 'none'; base-uri 'self'; frame-ancestors 'none'; form-action 'self'"
    )
    if request.path.startswith('/api/'):
        resp.headers['Cache-Control'] = 'no-store'
    resp.headers.pop('Server', None)
    return resp


@app.route('/')
def index():
    return render_template('index.html')


@app.route('/healthz')
def healthz():
    return 'ok'


# Key management system client

class KMSError(Exception):
    pass


_ssl_contexts = {}


def _kms_context(identity):
    if identity not in _ssl_contexts:
        cert, key = KMS_IDENTITIES[identity]
        if not (KMS_URL and KMS_CA and cert and key):
            raise KMSError('this dashboard is not connected to a KMS (KMS_URL and its certificates are unset)')
        ctx = ssl.create_default_context(cafile=KMS_CA)
        ctx.minimum_version = ssl.TLSVersion.TLSv1_3
        ctx.load_cert_chain(cert, key)
        _ssl_contexts[identity] = ctx
    return _ssl_contexts[identity]


def _kms(identity, method, path, body=None, timeout=20):
    '''One call to this node's KMS as the dashboard's admin or SAE identity; returns (status, json)'''
    ctx = _kms_context(identity)
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(KMS_URL + path, data=data, method=method, headers={'Content-Type': 'application/json'})
    try:
        with urllib.request.urlopen(req, context=ctx, timeout=timeout) as r:
            return r.status, json.loads(r.read() or b'{}')
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read() or b'{}')
        except ValueError:
            return e.code, {'message': str(e.reason)}
    except (urllib.error.URLError, OSError) as e:
        raise KMSError(f'KMS unreachable: {getattr(e, "reason", e)}')


def _kms_json(identity, method, path, body=None):
    status, data = _kms(identity, method, path, body)
    if status != 200:
        raise KMSError(data.get('message') or f'KMS answered {status}')
    return data


def _q(s):
    return urllib.parse.quote(str(s), safe='')


def _fingerprint(key_b64):
    '''First 64 bits of SHA-256: enough to compare two ends by eye, nothing usable about the key'''
    return hashlib.sha256(base64.b64decode(key_b64)).hexdigest()[:16]


def _neighbors():
    try:
        return [l['neighbor'] for l in _kms_json('admin', 'GET', '/kme/v1/node').get('links') or []]
    except KMSError:
        return []


@app.errorhandler(KMSError)
def _kms_error(e):
    return jsonify({'error': str(e)}), 503


@app.route('/api/node')
def api_node():
    node = _kms_json('admin', 'GET', '/kme/v1/node?probe=1')
    node['network'] = _kms_json('admin', 'GET', '/kme/v1/network')['network']
    node['sae_id'] = SAE_ID
    return jsonify(node)


@app.route('/api/kms/audit')
def api_kms_audit():
    n = int(request.args.get('n') or 200)
    return jsonify(_kms_json('admin', 'GET', f'/kme/v1/audit?n={n}'))


@app.route('/api/kms/status')
def api_kms_status():
    return jsonify(_kms_json('sae', 'GET', f'/api/v1/keys/{_q(request.args.get("slave", ""))}/status'))


@app.route('/api/kms/enc', methods=['POST'])
def api_kms_enc():
    '''ETSI Get Key as this dashboard's SAE: returns key_IDs and fingerprints, never the keys'''
    body = request.get_json(silent=True) or {}
    slave = str(body.get('slave', ''))
    req = {'number': int(body.get('number') or 1), 'size': int(body.get('size') or 256)}
    data = _kms_json('sae', 'POST', f'/api/v1/keys/{_q(slave)}/enc_keys', req)
    return jsonify({'master': SAE_ID, 'slave': slave,
                    'keys': [{'key_ID': k['key_ID'], 'fingerprint': _fingerprint(k['key'])} for k in data['keys']]})


@app.route('/api/kms/dec', methods=['POST'])
def api_kms_dec():
    '''ETSI Get Key with Key IDs as this dashboard's SAE: fingerprints only'''
    body = request.get_json(silent=True) or {}
    master = str(body.get('master', ''))
    ids = [str(i).strip() for i in body.get('key_ids') or [] if str(i).strip()]
    data = _kms_json('sae', 'POST', f'/api/v1/keys/{_q(master)}/dec_keys', {'key_IDs': [{'key_ID': i} for i in ids]})
    return jsonify({'master': master, 'slave': SAE_ID,
                    'keys': [{'key_ID': k['key_ID'], 'fingerprint': _fingerprint(k['key'])} for k in data['keys']]})


# Recordings

def _list_recordings():
    return sorted(p.name for p in RECORDINGS_DIR.glob('*_timestamp_sequence.npy'))


def _recording_base(ts_name):
    return ts_name.removesuffix('_timestamp_sequence.npy')


def _recording_group_paths(base):
    return [p for suffix in _RECORDING_SUFFIXES if (p := RECORDINGS_DIR / f'{base}{suffix}').is_file()]


def _recordings_total_bytes():
    return sum(p.stat().st_size for p in RECORDINGS_DIR.glob('*') if p.is_file())


def _delete_recording(base):
    freed = 0
    for p in _recording_group_paths(base):
        try:
            freed += p.stat().st_size
            p.unlink()
        except OSError:
            pass
    return freed


def _enforce_recordings_cap():
    '''Opt-in (RECORDINGS_MAX_GB > 0): deletes the oldest recordings until back under the cap'''
    if RECORDINGS_MAX_GB <= 0:
        return
    cap = RECORDINGS_MAX_GB * 1024 ** 3
    for name in _list_recordings():
        if _recordings_total_bytes() <= cap:
            break
        _delete_recording(_recording_base(name))


def _pairs():
    '''Recordings started together share their YYYYMMDD_HHMMSS prefix; those make a ready pair'''
    by_start = collections.defaultdict(list)
    for n in _list_recordings():
        by_start[n[:15]].append(n)
    return [{'a': g[0], 'b': g[1], 'started': s} for s, g in sorted(by_start.items()) if len(g) == 2]


def _resolve_pair(a=None, b=None):
    '''Validates a chosen pair of timestamp files; with none given, the two newest recordings'''
    names = _list_recordings()
    if not a and not b:
        if len(names) < 2:
            raise ValueError(f'need 2 recordings, have {len(names)}')
        a, b = names[-2], names[-1]
    if a not in names or b not in names or a == b:
        raise ValueError('choose two different stored recordings')
    return RECORDINGS_DIR / a, RECORDINGS_DIR / b


@app.route('/api/status')
def api_status():
    return jsonify({
        'node_id': NODE_ID,
        'kms': bool(KMS_URL),
        'sim_keys': sum(1 for _ in KEYSTORE_DIR.glob('*.json')),
        'recordings': _list_recordings(),
        'recordings_bytes': _recordings_total_bytes(),
        'job': _current_job_summary(),
        'timestamp': int(time.time()),
    })


@app.route('/api/upload', methods=['POST'])
def api_upload():
    saved, skipped = [], []
    for f in request.files.getlist('files'):
        name = secure_filename(f.filename or '')
        if not name or not name.endswith(_RECORDING_SUFFIXES):
            skipped.append(f.filename or '(unnamed)')
            continue
        f.save(RECORDINGS_DIR / name)
        saved.append(name)
    if saved:
        _enforce_recordings_cap()
    return jsonify({'saved': saved, 'skipped': skipped, 'recordings': _list_recordings()})


@app.route('/api/recordings')
def api_recordings():
    names = _list_recordings()
    items = [{'base': _recording_base(n), 'name': n,
              'bytes': sum(p.stat().st_size for p in _recording_group_paths(_recording_base(n)))} for n in names]
    return jsonify({'files': names, 'items': items, 'pairs': _pairs(),
                    'total_bytes': _recordings_total_bytes(), 'max_gb': RECORDINGS_MAX_GB})


@app.route('/api/recordings', methods=['DELETE'])
def api_delete_all_recordings():
    freed = sum(_delete_recording(_recording_base(n)) for n in _list_recordings())
    return jsonify({'freed_bytes': freed, 'recordings': _list_recordings()})


@app.route('/api/recordings/<base>', methods=['DELETE'])
def api_delete_recording(base):
    base = secure_filename(base)
    if not (RECORDINGS_DIR / f'{base}_timestamp_sequence.npy').is_file():
        return jsonify({'error': f'no such recording: {base}'}), 404
    return jsonify({'deleted': base, 'freed_bytes': _delete_recording(base), 'recordings': _list_recordings()})


# Simulator

# Default CRC chunk per method: polar and LDPC fix or fail whole blocks, so a coarse chunk keeps
# the 16-bit-per-chunk disclosure small; Winnow leaves scattered errors and needs fine chunks
_DEFAULT_CHUNK = {'ldpc5g': 1024, 'polar': 1024, 'ldpc': 1024}


def _add_arg(body, args, flag, key, cast):
    v = body.get(key)
    if v not in (None, ''):
        args += [flag, str(cast(v))]


def _method_args(body, args, real):
    '''Error-correction and key-length flags shared by the simulator and real extraction; real adds
    the coincidence-filter and timing options only cmd/importtt has'''
    method = body.get('method', 'cascade')
    if method == 'cascade':
        args.append('-cascade')
    elif method == 'ldpc5g':
        args.append('-ldpc5g')
    elif method == 'polar':
        args.append('-polar')
        _add_arg(body, args, '-polar-margin', 'polar_margin', float)
        _add_arg(body, args, '-polar-list', 'polar_list', int)
    elif method == 'ldpc':
        args.append('-ldpc')
        for flag, key in (('-ldpc-block', 'ldpc_block'), ('-ldpc-wc', 'ldpc_wc'), ('-ldpc-wr', 'ldpc_wr'), ('-ldpc-iterations', 'ldpc_iterations')):
            _add_arg(body, args, flag, key, int)
    elif method == 'winnow':
        args.append('-winnow')
        _add_arg(body, args, '-winnow-blocks', 'winnow_blocks', str)
    elif body.get('search_matrix'):
        args.append('-search-matrix')
    if real and method in ('ldpc5g', 'polar', 'ldpc'):
        _add_arg(body, args, '-soft-bins', 'soft_bins', int)
    if body.get('chunk') not in (None, ''):
        _add_arg(body, args, '-chunk', 'chunk', int)
    elif method in _DEFAULT_CHUNK:
        args += ['-chunk', str(_DEFAULT_CHUNK[method])]
    if body.get('asymptotic'):
        args.append('-asymptotic')
    if real:
        if body.get('filter') is False:
            args.append('-filter=false')
        _add_arg(body, args, '-filter-e0', 'filter_e0', float)
        _add_arg(body, args, '-window', 'window_ps', int)


_last_sim_keys = set()


@app.route('/api/run', methods=['POST'])
def api_run():
    '''Simulated pipeline (cmd/qkd_post_processing -json); its keys are demo keys, replace the
    previous run's, and never reach the KMS'''
    global _last_sim_keys
    body = request.get_json(silent=True) or {}
    args = [QKD_BIN, '-json', '-keystore', str(KEYSTORE_DIR)]
    for flag, key, cast in (('-n', 'n', int), ('-err', 'err', float), ('-multiphoton', 'multiphoton', float),
                            ('-window', 'window', int), ('-jitter', 'jitter', int), ('-seed', 'seed', int)):
        _add_arg(body, args, flag, key, cast)
    _method_args(body, args, real=False)
    for p in _last_sim_keys:
        p.unlink(missing_ok=True)
    before = set(KEYSTORE_DIR.glob('*.json'))
    try:
        proc = subprocess.run(args, capture_output=True, text=True, timeout=300)
    except subprocess.TimeoutExpired:
        return jsonify({'error': 'simulation timed out'}), 504
    except FileNotFoundError:
        return jsonify({'error': f'{QKD_BIN} not found'}), 500
    if proc.returncode != 0:
        return jsonify({'error': (proc.stderr or 'simulation failed').strip()[:400]}), 500
    try:
        metrics = json.loads(proc.stdout.strip().splitlines()[-1])
    except (ValueError, IndexError):
        return jsonify({'error': 'could not parse simulator output'}), 500
    _last_sim_keys = set(KEYSTORE_DIR.glob('*.json')) - before
    metrics['keys_in_store'] = len(_last_sim_keys)
    return jsonify(metrics)


# Acquisition config and recording

def _validate_config(cfg):
    if not isinstance(cfg.get('sites'), dict) or not cfg['sites']:
        return 'missing or empty "sites"'
    if not isinstance(cfg.get('links'), dict) or not cfg['links']:
        return 'missing or empty "links"'
    for name, link in cfg['links'].items():
        for key in ('site_a', 'site_b'):
            if link.get(key) not in cfg['sites']:
                return f'links.{name}.{key} not found in "sites"'
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
        'links': [{'name': n, 'site_a': l['site_a'], 'site_b': l['site_b']} for n, l in sorted(cfg.get('links', {}).items())],
        'default_link': (cfg.get('record_dual') or {}).get('link'),
    }


@app.route('/api/config', methods=['GET', 'POST'])
def api_config():
    if request.method == 'GET':
        if not UPLOADED_CONFIG_PATH.is_file():
            return jsonify({'loaded': False})
        return jsonify({'loaded': True, **_config_summary(json.loads(UPLOADED_CONFIG_PATH.read_text()))})
    raw = request.files['file'].read() if request.files.get('file') else request.data
    if not raw:
        return jsonify({'error': 'no config file given'}), 400
    try:
        cfg = json.loads(raw)
    except ValueError as e:
        return jsonify({'error': f'invalid JSON: {e}'}), 400
    err = _validate_config(cfg)
    if err:
        return jsonify({'error': err}), 400
    UPLOADED_CONFIG_PATH.write_text(json.dumps(cfg, indent=2))
    os.chmod(UPLOADED_CONFIG_PATH, 0o600)  # holds the TimeTaggers' auth keys
    return jsonify({'loaded': True, **_config_summary(cfg)})


_acquire_lock = threading.Lock()
_acquire = {'proc': None, 'log': collections.deque(maxlen=1000), 'started_at': None, 'live': None, 'kind': None}
_LIVE_PREFIX = 'LIVE '  # live_peak.emit: one JSON status of the live coincidence tracker per line


def _read_acquire_output(proc):
    for line in proc.stdout:
        if line.startswith(_LIVE_PREFIX):
            try:
                _acquire['live'] = json.loads(line[len(_LIVE_PREFIX):])
            except ValueError:
                pass
            continue
        _acquire['log'].append(line.rstrip('\n'))
    proc.wait()
    _enforce_recordings_cap()


def _start_acquire(args, kind):
    '''Runs a recording or a replay as the one acquisition process; caller holds _acquire_lock'''
    if _acquire['proc'] is not None and _acquire['proc'].poll() is None:
        return jsonify({'error': 'a recording or replay is already running'}), 409
    if _clog_running():
        return jsonify({'error': 'a coincidence log is running on the TimeTaggers'}), 409
    _acquire['log'].clear()
    _acquire['live'] = None
    try:
        proc = subprocess.Popen(args, cwd=str(REPO_ROOT / 'acquisition'), stdout=subprocess.PIPE,
                                stderr=subprocess.STDOUT, text=True, bufsize=1)
    except OSError as e:
        return jsonify({'error': f'could not start: {e}'}), 500
    _acquire['proc'], _acquire['started_at'], _acquire['kind'] = proc, time.time(), kind
    threading.Thread(target=_read_acquire_output, args=(proc,), daemon=True).start()
    return jsonify({'started': True})


@app.route('/api/acquire/start', methods=['POST'])
def api_acquire_start():
    if not UPLOADED_CONFIG_PATH.is_file():
        return jsonify({'error': 'load a config first'}), 400
    body = request.get_json(silent=True) or {}
    args = [sys.executable, str(REPO_ROOT / 'acquisition' / 'tt_record_dual.py'), str(UPLOADED_CONFIG_PATH),
            '--duration', str(float(body.get('duration_s', 60))), '--output-dir', str(RECORDINGS_DIR)]
    if body.get('link'):
        args += ['--link', str(body['link'])]
    with _acquire_lock:
        return _start_acquire(args, 'recording')


@app.route('/api/acquire/replay', methods=['POST'])
def api_acquire_replay():
    '''Streams two stored recordings through the live tracker at the given speed, with the loaded config's settings'''
    if not UPLOADED_CONFIG_PATH.is_file():
        return jsonify({'error': 'load a config first'}), 400
    body = request.get_json(silent=True) or {}
    try:
        path_a, path_b = _resolve_pair(body.get('a'), body.get('b'))
        speed = float(body.get('speed', 1.0))
    except (ValueError, TypeError) as e:
        return jsonify({'error': str(e)}), 400
    args = [sys.executable, str(REPO_ROOT / 'acquisition' / 'live_peak.py'), str(path_a), str(path_b),
            '--speed', str(max(speed, 0.0)), '--json', '--config', str(UPLOADED_CONFIG_PATH)]
    with _acquire_lock:
        return _start_acquire(args, 'replay')


@app.route('/api/acquire/stop', methods=['POST'])
def api_acquire_stop():
    with _acquire_lock:
        proc = _acquire['proc']
        if proc is None or proc.poll() is not None:
            return jsonify({'error': 'nothing is running'}), 409
        proc.send_signal(signal.SIGINT)  # tt_record_dual.py handles this like Ctrl+C
    return jsonify({'stopping': True})


@app.route('/api/acquire/status')
def api_acquire_status():
    proc = _acquire['proc']
    return jsonify({'running': proc is not None and proc.poll() is None,
                    'returncode': None if proc is None else proc.poll(),
                    'kind': _acquire['kind'], 'started_at': _acquire['started_at'], 'log': list(_acquire['log'])[-200:]})


@app.route('/api/acquire/live')
def api_acquire_live():
    '''The live coincidence tracker's latest status, from the running or last recording or replay'''
    proc = _acquire['proc']
    return jsonify({'running': proc is not None and proc.poll() is None, 'kind': _acquire['kind'],
                    'status': _acquire['live']})


# Continuous QKD: the recorder in continuous mode, and the processor distilling its blocks

_CONT_PREFIX = 'CONT '  # continuous_qkd.py: one JSON status per line
_cont = {'proc': None, 'run': None, 'status': None, 'log': collections.deque(maxlen=400), 'params': None, 'started_at': None}


def _read_cont_output(proc):
    for line in proc.stdout:
        if line.startswith(_CONT_PREFIX):
            try:
                _cont['status'] = json.loads(line[len(_CONT_PREFIX):])
            except ValueError:
                pass
            continue
        # the processor's own lines and problems; the peak search's progress lines would drown them
        if '[continuous]' in line or line.startswith(('[WARN]', '[ERROR]', 'Traceback')):
            _cont['log'].append(line.rstrip('\n'))
    proc.wait()


def _cont_running():
    return _cont['proc'] is not None and _cont['proc'].poll() is None


@app.route('/api/continuous/start', methods=['POST'])
def api_continuous_start():
    '''Records continuously in segments and distils a key block every block_s seconds of data'''
    if not UPLOADED_CONFIG_PATH.is_file():
        return jsonify({'error': 'load a config first'}), 400
    body = request.get_json(silent=True) or {}
    try:
        segment_s = float(body.get('segment_s', 10))
        block_s = float(body.get('block_s', 300))
        speed = float((body.get('replay') or {}).get('speed', 1))
    except (TypeError, ValueError):
        return jsonify({'error': 'segment, block and speed must be numbers'}), 400
    method = body.get('method', 'cascade')
    neighbor = body.get('neighbor') or ''
    if method not in ('cascade', 'ldpc5g', 'polar') or not 1 <= segment_s <= 600 or not 10 <= block_s <= 86400 or speed <= 0:
        return jsonify({'error': 'method cascade, ldpc5g or polar, segments of 1-600 s, blocks of 10 s to a day'}), 400
    if neighbor and neighbor not in _neighbors():
        return jsonify({'error': f'{neighbor} is not a QKD link of this node'}), 400
    with _acquire_lock:
        if _cont_running() or (_acquire['proc'] is not None and _acquire['proc'].poll() is None):
            return jsonify({'error': 'a recording, replay or continuous run is already going'}), 409
        run = CONTINUOUS_DIR / time.strftime('%Y%m%d_%H%M%S')
        run.mkdir(parents=True, exist_ok=True)
        rec = [sys.executable, str(REPO_ROOT / 'acquisition' / 'tt_record_dual.py'), str(UPLOADED_CONFIG_PATH),
               '--stop-mode', 'continuous', '--segment-s', str(segment_s), '--output-dir', str(run)]
        replay = body.get('replay')
        if replay:
            try:
                path_a, path_b = _resolve_pair(replay.get('a'), replay.get('b'))
            except ValueError as e:
                return jsonify({'error': str(e)}), 400
            rec += ['--replay', str(path_a), str(path_b), '--replay-speed', str(speed)]
        elif body.get('link'):
            rec += ['--link', str(body['link'])]
        resp = _start_acquire(rec, 'continuous')
        if _acquire['proc'] is None or _acquire['proc'].poll() is not None:
            return resp
        proc = [sys.executable, str(REPO_ROOT / 'acquisition' / 'continuous_qkd.py'), str(run), '--importtt', IMPORTTT_BIN,
                '--config', str(UPLOADED_CONFIG_PATH), '--block-s', str(block_s), '--method', method]
        if neighbor:
            proc += ['--keystore', str(IMPORT_DIR / neighbor), '--keystore-max', str(KEYSTORE_MAX)]
        _cont['log'].clear()
        _cont['status'] = None
        try:
            p = subprocess.Popen(proc, cwd=str(REPO_ROOT / 'acquisition'), stdout=subprocess.PIPE,
                                 stderr=subprocess.STDOUT, text=True, bufsize=1)
        except OSError as e:
            _acquire['proc'].send_signal(signal.SIGINT)
            return jsonify({'error': f'could not start the processor: {e}'}), 500
        _cont.update(proc=p, run=run.name, started_at=time.time(),
                     params={'segment_s': segment_s, 'block_s': block_s, 'method': method, 'neighbor': neighbor,
                             'source': 'replay' if replay else (body.get('link') or 'default link'), 'speed': speed if replay else 1})
        threading.Thread(target=_read_cont_output, args=(p,), daemon=True).start()
    return jsonify({'started': True, 'run': run.name})


@app.route('/api/continuous/stop', methods=['POST'])
def api_continuous_stop():
    '''Stops recording; the processor finishes the segments already written and distils the last block'''
    with _acquire_lock:
        rec = _acquire['proc']
        if _acquire['kind'] == 'continuous' and rec is not None and rec.poll() is None:
            rec.send_signal(signal.SIGINT)
            return jsonify({'stopping': True})
        if _cont_running() and request.args.get('abort') == '1':
            _cont['proc'].send_signal(signal.SIGINT)
            return jsonify({'aborting': True})
    return jsonify({'error': 'no continuous recording is running'}), 409


@app.route('/api/continuous/status')
def api_continuous_status():
    rec = _acquire['proc']
    return jsonify({'recording': _acquire['kind'] == 'continuous' and rec is not None and rec.poll() is None,
                    'processing': _cont_running(), 'run': _cont['run'], 'params': _cont['params'],
                    'started_at': _cont['started_at'], 'status': _cont['status'], 'log': list(_cont['log'])[-150:]})


# Coincidence log: singles and coincidences per integration window, appended to a CSV for the photonics datasets

_CLOG_PREFIX = coincidences.STATUS_PREFIX
_CLOG_LIVE = 2400  # live points kept for the page, 20 minutes of half-second reads
_clog = {'proc': None, 'status': None, 'live': collections.deque(maxlen=_CLOG_LIVE), 'live_n': 0, 'hist': None,
         'log': collections.deque(maxlen=400), 'params': None, 'started_at': None}


def _clog_running():
    return _clog['proc'] is not None and _clog['proc'].poll() is None


def _read_clog_output(proc):
    for line in proc.stdout:
        if line.startswith(_CLOG_PREFIX):
            try:
                st = json.loads(line[len(_CLOG_PREFIX):])
            except ValueError:
                continue
            if st.get('live'):
                _clog['live'].append(st['live'])
                _clog['live_n'] += 1
            if st.get('histograms'):
                _clog['hist'] = st.pop('histograms')
            _clog['status'] = st
            continue
        # the logger's own lines and problems; one line per TimeTagger read would drown them
        if '[log]' in line or line.startswith(('[WARN]', '[ERROR]', 'Traceback')):
            _clog['log'].append(line.rstrip('\n'))
    proc.wait()


def _clog_config():
    return json.loads(UPLOADED_CONFIG_PATH.read_text()) if UPLOADED_CONFIG_PATH.is_file() else {}


@app.route('/api/clog/options')
def api_clog_options():
    '''Every option's default, the loaded config's section, and each site's channels'''
    cfg = _clog_config()
    return jsonify({'defaults': coincidences.DEFAULTS, 'config': cfg.get('coincidence_log', {}),
                    'site_channels': {n: s.get('channels', [1, 2, 3, 4]) for n, s in sorted(cfg.get('sites', {}).items())},
                    'loaded': bool(cfg)})


@app.route('/api/clog/start', methods=['POST'])
def api_clog_start():
    '''Logs the TimeTaggers, or stored recordings offline, one row per integration window'''
    if not UPLOADED_CONFIG_PATH.is_file():
        return jsonify({'error': 'load a config first'}), 400
    body = request.get_json(silent=True) or {}
    options = body.get('options') or {}
    if not isinstance(options, dict):
        return jsonify({'error': 'options must be an object'}), 400
    options.pop('output_dir', None)  # logs stay in this node's directory
    cfg = _clog_config()
    try:
        merged = coincidences.merge(cfg.get('coincidence_log', {}), options)
    except (ValueError, TypeError) as e:
        return jsonify({'error': str(e)}), 400
    args = [sys.executable, str(REPO_ROOT / 'acquisition' / 'coincidence_logger.py'), str(UPLOADED_CONFIG_PATH),
            '--json', '--output-dir', str(CLOG_DIR), '--options', json.dumps(options)]
    try:
        if body.get('windows'):
            args += ['--windows', str(int(body['windows']))]
        if body.get('duration_s'):
            args += ['--duration-s', str(float(body['duration_s']))]
        kind = body.get('source', 'sites')
        if kind == 'recordings':
            names = [n for n in (body.get('a'), body.get('b')) if n]
            have = set(_list_recordings())
            if not names or any(n not in have for n in names) or len(set(names)) != len(names):
                raise ValueError('pick one or two different stored recordings')
            args += ['--recordings', *[str(RECORDINGS_DIR / n) for n in names]]
            source = f'{len(names)} stored recording(s), offline'
        else:
            sites = [s for s in body.get('sites') or merged['sites'] or [] if s]
            if not 1 <= len(sites) <= 2 or any(s not in cfg.get('sites', {}) for s in sites) or len(set(sites)) != len(sites):
                raise ValueError('one site, or two different sites, of the loaded config')
            args += ['--sites', *sites]
            source = ' + '.join(sites)
    except (TypeError, ValueError) as e:
        return jsonify({'error': str(e)}), 400
    live_source = kind != 'recordings'
    with _acquire_lock:
        busy = _clog_running() or (live_source and (_cont_running() or (_acquire['proc'] is not None and _acquire['proc'].poll() is None)))
        if busy:
            return jsonify({'error': 'busy: a log is running, or the TimeTaggers are recording'}), 409
        _clog['log'].clear()
        _clog['live'].clear()
        _clog.update(status=None, hist=None, live_n=0)
        try:
            proc = subprocess.Popen(args, cwd=str(REPO_ROOT / 'acquisition'), stdout=subprocess.PIPE,
                                    stderr=subprocess.STDOUT, text=True, bufsize=1)
        except OSError as e:
            return jsonify({'error': f'could not start: {e}'}), 500
        _clog.update(proc=proc, started_at=time.time(), params={'source': source, 'integration_s': merged['integration_s'], 'kind': kind})
        threading.Thread(target=_read_clog_output, args=(proc,), daemon=True).start()
    return jsonify({'started': True})


@app.route('/api/clog/stop', methods=['POST'])
def api_clog_stop():
    '''Stops the log; the window in progress is written as a last, incomplete row'''
    if not _clog_running():
        return jsonify({'error': 'no coincidence log is running'}), 409
    _clog['proc'].send_signal(signal.SIGINT)
    return jsonify({'stopping': True})


@app.route('/api/clog/status')
def api_clog_status():
    proc = _clog['proc']
    # points past the client's count; the oldest drop off the deque, so it may get fewer than it missed
    since = request.args.get('since', type=int) or 0
    live = list(_clog['live'])
    live = live[max(since - (_clog['live_n'] - len(live)), 0):]
    return jsonify({'running': _clog_running(), 'returncode': None if proc is None else proc.poll(),
                    'params': _clog['params'], 'started_at': _clog['started_at'], 'status': _clog['status'],
                    'histograms': _clog['hist'], 'live': live, 'live_total': _clog['live_n'],
                    'log': list(_clog['log'])[-150:]})


def _clog_path(name):
    path = CLOG_DIR / secure_filename(name)
    return path if name.endswith('_coincidences.csv') and path.is_file() else None


@app.route('/api/clog/files')
def api_clog_files():
    files = []
    for p in sorted(CLOG_DIR.glob('*_coincidences.csv'), key=lambda p: p.stat().st_mtime, reverse=True):
        st = p.stat()
        with p.open(encoding='utf-8') as f:
            rows = sum(1 for line in f if line.strip() and not line.startswith('#')) - 1
        files.append({'name': p.name, 'bytes': st.st_size, 'rows': max(rows, 0), 'modified': st.st_mtime})
    return jsonify({'files': files})


@app.route('/api/clog/files/<name>', methods=['GET', 'DELETE'])
def api_clog_file(name):
    path = _clog_path(name)
    if path is None:
        return jsonify({'error': 'no such log'}), 404
    if request.method == 'DELETE':
        if _clog_running() and _clog['status'] and _clog['status'].get('file') == path.name:
            return jsonify({'error': 'this log is being written'}), 409
        path.unlink()
        return jsonify({'deleted': name})
    return send_file(path, mimetype='text/csv', as_attachment=True, download_name=path.name)


@app.route('/api/clog/files/<name>/data')
def api_clog_file_data(name):
    '''A log's metadata, columns and rows, for the page's plots and table'''
    path = _clog_path(name)
    if path is None:
        return jsonify({'error': 'no such log'}), 404
    try:
        meta, columns, rows = coincidences.read_log(path)
    except (ValueError, OSError) as e:
        return jsonify({'error': f'unreadable log: {e}'}), 400
    return jsonify({'name': path.name, 'meta': meta, 'columns': columns, 'rows': rows})


# Coincidence-peak analysis, cached per recording pair

_ANALYSIS_CACHE_SIZE = 4
_analysis_cache = collections.OrderedDict()  # pair and file versions, then report, diagnostics, plots
_analysis_lock = threading.Lock()


def _load_timestamps(path):
    import numpy as np
    arr = np.load(path, mmap_mode='r')
    if arr.ndim != 1 or not np.issubdtype(arr.dtype, np.integer) or arr.shape[0] < 2:
        raise ValueError(f'{path.name}: not a valid timestamp array')
    return arr


def _get_analysis(path_a, path_b):
    '''Full analyze_arrays(strict=False) over one pair, computed once per pair and file version'''
    key = (path_a.name, path_a.stat().st_mtime_ns, path_b.name, path_b.stat().st_mtime_ns)
    with _analysis_lock:
        if key in _analysis_cache:
            _analysis_cache.move_to_end(key)
            return _analysis_cache[key]
        a, b = _load_timestamps(path_a), _load_timestamps(path_b)
        report, diagnostics = coincidence_peak.analyze_arrays(a, b, strict=False)
        _analysis_cache[key] = {'report': report, 'diagnostics': diagnostics, 'plots': None,
                                'n_a': int(a.shape[0]), 'n_b': int(b.shape[0])}
        while len(_analysis_cache) > _ANALYSIS_CACHE_SIZE:
            _analysis_cache.popitem(last=False)
        return _analysis_cache[key]


_PLOT_MAX_POINTS = 500


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
    idx, ys = _downsample_max(counts, _PLOT_MAX_POINTS)
    xs = [(-radius_ps + (i + 0.5) * bin_ps) / 1000 for i in idx]
    return {'x_ns': xs, 'y': ys, 'window_center_ns': window.center_ps / 1000, 'window_width_ns': window.width_ps / 1000}


def _serialize_plots(report, diagnostics):
    import numpy as np
    lags = (np.asarray(diagnostics['coarse_lags_ps']) / 1e9).tolist()
    scores = np.asarray(diagnostics['coarse_scores']).tolist()
    if len(lags) > _PLOT_MAX_POINTS:
        idx, scores = _downsample_max(scores, _PLOT_MAX_POINTS)
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


@app.route('/api/align/live')
def api_align_live():
    try:
        path_a, path_b = _resolve_pair(request.args.get('a'), request.args.get('b'))
        entry = _get_analysis(path_a, path_b)
    except Exception as e:
        return jsonify({'ready': False, 'reason': str(e)[:200], 'timestamp': int(time.time())})
    report = entry['report']
    if entry['plots'] is None:
        entry['plots'] = _serialize_plots(report, entry['diagnostics'])
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
def _prepared_dirs(pairs):
    '''One scratch directory per pair, holding links to its files and its *_coincidence.json, the
    layout cmd/importtt and chsh_diagnostic.py read; refuses a pair without a confirmed peak'''
    dirs, reports = [], []
    try:
        for path_a, path_b in pairs:
            report = dict(_get_analysis(path_a, path_b)['report'])
            if not report['detected']:
                score = report['constant_shift']['window']['local_score']
                raise ValueError(f'{path_a.name} + {path_b.name}: no significant coincidence peak '
                                 f'(score {score:.2f}, need {coincidence_peak.MIN_PEAK_SCORE:.2f})')
            report['input_a'], report['input_b'] = path_a.name, path_b.name
            d = Path(tempfile.mkdtemp(prefix='pair_'))
            dirs.append(d)
            for src in (path_a, path_b):
                for p in _recording_group_paths(_recording_base(src.name)):
                    os.symlink(p, d / p.name)
            (d / f'{_recording_base(path_a.name)}_coincidence.json').write_text(json.dumps(report))
            reports.append(report)
        yield dirs, reports
    finally:
        for d in dirs:
            shutil.rmtree(d, ignore_errors=True)


# Background jobs: key extraction and delay calibration, one at a time

_job_ids = itertools.count(1)
_jobs = collections.OrderedDict()
_job_queue = queue.Queue()
_JOBS_KEPT = 50


def _load_job_history():
    global _job_ids
    if not JOBS_PATH.is_file():
        return
    for line in JOBS_PATH.read_text().splitlines()[-_JOBS_KEPT:]:
        try:
            job = json.loads(line)
        except ValueError:
            continue
        job['log'] = collections.deque(job.get('log', []), maxlen=400)
        _jobs[job['id']] = job
    _job_ids = itertools.count(max([int(j) for j in _jobs] or [0]) + 1)


def _job_view(job, with_log=True):
    v = {k: job[k] for k in ('id', 'kind', 'status', 'created', 'started', 'finished', 'params', 'result', 'error')}
    if with_log:
        v['log'] = list(job['log'])
    return v


def _current_job_summary():
    for job in reversed(_jobs.values()):
        if job['status'] in ('queued', 'running'):
            return {'id': job['id'], 'kind': job['kind'], 'status': job['status']}
    return None


def _job_worker():
    while True:
        job = _job_queue.get()
        job['status'], job['started'] = 'running', time.time()
        try:
            job['result'] = _JOB_KINDS[job['kind']](job)
            job['status'] = 'done'
        except Exception as e:
            job['status'], job['error'] = 'failed', str(e)[:500]
        job['finished'] = time.time()
        try:
            with JOBS_PATH.open('a') as f:
                f.write(json.dumps(_job_view(job)) + '\n')
        except OSError:
            pass


def _run_logged(job, args, timeout=3600):
    '''Runs a pipeline command, streaming its stderr into the job log; returns its stdout'''
    job['log'].append('$ ' + ' '.join([Path(args[0]).name] + args[1:]))
    proc = subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    out = []
    reader = threading.Thread(target=lambda: out.append(proc.stdout.read()), daemon=True)
    reader.start()
    killer = threading.Timer(timeout, proc.kill)
    killer.start()
    for line in proc.stderr:
        job['log'].append(line.rstrip('\n'))
    proc.wait()
    killer.cancel()
    reader.join()
    if proc.returncode != 0:
        raise RuntimeError(f'exit status {proc.returncode}: ' + ' / '.join(list(job['log'])[-3:]))
    return out[0] if out else ''


def _pairs_from(params):
    pairs = [_resolve_pair(p.get('a'), p.get('b')) for p in params.get('pairs') or []]
    if not pairs:
        raise ValueError('choose at least one recording pair')
    return pairs


def _job_extract(job):
    p = job['params']
    pairs = _pairs_from(p)
    link = p.get('link') or ''
    job['log'].append(f'analysing {len(pairs)} recording pair(s)')
    with _prepared_dirs(pairs) as (dirs, _):
        args = [IMPORTTT_BIN, '-json', '-dir', ','.join(str(d) for d in dirs)]
        _method_args(p, args, real=True)
        if p.get('delays'):
            if not DELAYS_PATH.is_file():
                raise ValueError('no detector delay calibration yet')
            args += ['-detector-delays', str(DELAYS_PATH)]
        if link:
            args += ['-keystore', str(IMPORT_DIR / link), '-keystore-max', str(KEYSTORE_MAX)]
        out = _run_logged(job, args)
    metrics = json.loads(out.strip().splitlines()[-1])
    if metrics.get('error'):
        raise RuntimeError(metrics['error'])
    metrics['link'] = link
    if link and metrics.get('keys_saved'):
        job['log'].append(f'{metrics["keys_saved"]} key(s) handed to the KMS pool of link {NODE_ID}-{link}')
    return metrics


def _job_calibrate(job):
    pairs = _pairs_from(job['params'])
    with _prepared_dirs(pairs) as (dirs, _):
        out = _run_logged(job, [IMPORTTT_BIN, '-json', '-dir', ','.join(str(d) for d in dirs), '-calibrate-delays', str(DELAYS_PATH)])
    try:
        return json.loads(out.strip().splitlines()[-1])
    except (ValueError, IndexError):
        return {'written': DELAYS_PATH.is_file()}


# ---------- two-party extraction: each node runs its half on its own recording only

TWO_PARTY_AUTO_ACCEPT = os.environ.get('TWO_PARTY_AUTO_ACCEPT', '1') == '1'
_PP_WAIT_S = 900  # how long one side waits for the other's next step


def _pp_send(peer, session, kind, body):
    status, data = _kms('admin', 'POST', '/kme/v1/pp/send', {'peer': peer, 'session': session, 'kind': kind, 'body': body}, timeout=300)
    if status != 200:
        raise RuntimeError(f'sending {kind} to {peer}: {data.get("message") or status}')


def _pp_recv(peer, session, timeout=_PP_WAIT_S):
    '''Next message of a session from peer, waiting up to timeout seconds'''
    deadline = time.time() + timeout
    q = urllib.parse.urlencode({'peer': peer, 'session': session, 'wait': 30})
    while time.time() < deadline:
        status, data = _kms('admin', 'GET', f'/kme/v1/pp/recv?{q}', timeout=60)
        if status == 200:
            return data['kind'], data['body']
        if status != 204:
            raise RuntimeError(f'receiving from {peer}: {data.get("message") or status}')
    raise RuntimeError(f'{peer} did not answer within {timeout} s')


_TWO_PARTY_PARAMS = ('method', 'polar_margin', 'polar_list', 'soft_bins', 'asymptotic', 'filter', 'chunk', 'delays')


def _two_party_args(p):
    '''The error-correction and key options both halves must agree on, as importtt flags'''
    method = p.get('method') or 'ldpc5g'
    if method not in ('ldpc5g', 'polar'):
        raise ValueError('two-party extraction runs 5G NR LDPC or polar codes')
    args = ['-' + method, '-chunk', str(int(p.get('chunk') or 1024))]
    _add_arg(p, args, '-polar-margin', 'polar_margin', float)
    _add_arg(p, args, '-polar-list', 'polar_list', int)
    _add_arg(p, args, '-soft-bins', 'soft_bins', int)
    if p.get('asymptotic'):
        args.append('-asymptotic')
    if p.get('filter') is False:
        args.append('-filter=false')
    if p.get('delays') and DELAYS_PATH.is_file():
        args += ['-detector-delays', str(DELAYS_PATH)]
    return args


def _link_files(dst, name):
    for f in _recording_group_paths(_recording_base(name)):
        os.symlink(f, dst / f.name)


def _job_two_party_alice(job):
    '''This node announces its timing, one way; the neighbor matches and decodes; both store the key'''
    p = job['params']
    peer = p['peer']
    names = p.get('recordings') or []
    if not names or any(n not in _list_recordings() for n in names):
        raise ValueError("choose this node's own recordings")
    session = f'{NODE_ID}-{int(time.time())}-{secrets.token_hex(3)}'
    p['session'] = session
    work = Path(tempfile.mkdtemp(prefix='alice_'))
    try:
        dirs = []
        for i, name in enumerate(names):
            d = work / f'r{i}'
            d.mkdir()
            _link_files(d, name)
            out = work / f'pub{i}'
            _run_logged(job, [IMPORTTT_BIN, '-role', 'alice', '-dir', str(d), '-export-public', str(out), '-json'])
            files = {f.name: base64.b64encode(f.read_bytes()).decode() for f in out.iterdir()}
            job['log'].append(f'announcing {name} to {peer}: timing, bases and multi-photon flags, no bits')
            _pp_send(peer, session, 'offer', {'index': i, 'total': len(names), 'base': _recording_base(name),
                                              'files': files, 'params': {k: p.get(k) for k in _TWO_PARTY_PARAMS}})
            dirs.append(str(d))
        job['log'].append(f'waiting for {peer} to align its own recordings')
        kind, body = _pp_recv(peer, session)
        if kind != 'accept':
            raise RuntimeError(f'{peer} declined: {body.get("reason", kind)}')
        job['log'].append(f"{peer} accepted; running Alice's half")
        args = [IMPORTTT_BIN, '-role', 'alice', '-dir', ','.join(dirs), '-peer', peer, '-session', session, '-json',
                '-keystore', str(IMPORT_DIR / peer)] + _two_party_args(p)
        out = _run_logged(job, args)
    finally:
        shutil.rmtree(work, ignore_errors=True)
    metrics = json.loads(out.strip().splitlines()[-1])
    metrics['link'] = peer
    return metrics


def _job_two_party_bob(job):
    '''Answers a neighbor's announcement with this node's own recording of the same start time'''
    p = job['params']
    peer, session = p['peer'], p['session']
    work = Path(tempfile.mkdtemp(prefix='bob_'))
    try:
        offers = []
        while True:
            kind, body = _pp_recv(peer, session, timeout=120)
            if kind != 'offer':
                raise RuntimeError(f'expected an announcement from {peer}, got {kind}')
            offers.append(body)
            if body['index'] + 1 >= body['total']:
                break
        params = {k: v for k, v in (offers[-1].get('params') or {}).items() if k in _TWO_PARTY_PARAMS}
        p.update(params, recordings=[o['base'] for o in offers])
        try:
            dirs = []
            for i, offer in enumerate(offers):
                started = offer['base'][:15]
                mine = [n for n in _list_recordings() if n.startswith(started) and _recording_base(n) != offer['base']]
                if len(mine) != 1:
                    raise ValueError(f'no single own recording started at {started} to answer {offer["base"]}')
                d = work / f'r{i}'
                d.mkdir()
                _link_files(d, mine[0])
                for name, data in offer['files'].items():
                    if Path(name).name != name or not name.endswith(('_public_timestamp_sequence.npy', '_public_flags.npy')):
                        raise ValueError(f'unexpected announcement file {name}')
                    (d / name).write_bytes(base64.b64decode(data))
                pub_ts = next(d.glob('*_public_timestamp_sequence.npy'))
                job['log'].append(f"aligning {mine[0]} with {peer}'s announcement {offer['base']}")
                report, _ = coincidence_peak.analyze_arrays(_load_timestamps(pub_ts), _load_timestamps(RECORDINGS_DIR / mine[0]), strict=False)
                if not report['detected']:
                    raise ValueError(f'no coincidence peak between {offer["base"]} and {mine[0]}')
                report['input_a'], report['input_b'] = pub_ts.name, mine[0]
                (d / 'pair_coincidence.json').write_text(json.dumps(report))
                dirs.append(str(d))
        except Exception as e:
            _pp_send(peer, session, 'reject', {'reason': str(e)[:300]})
            raise
        _pp_send(peer, session, 'accept', {})
        job['log'].append("accepted; running Bob's half")
        args = [IMPORTTT_BIN, '-role', 'bob', '-dir', ','.join(dirs), '-peer', peer, '-session', session, '-json',
                '-keystore', str(IMPORT_DIR / peer)] + _two_party_args(params)
        out = _run_logged(job, args)
    finally:
        shutil.rmtree(work, ignore_errors=True)
    metrics = json.loads(out.strip().splitlines()[-1])
    metrics['link'] = peer
    return metrics


def _two_party_listener():
    '''Notices a neighbor starting a two-party extraction and queues this node's half'''
    seen = set()
    while True:
        time.sleep(3)
        if not (TWO_PARTY_AUTO_ACCEPT and KMS_URL):
            continue
        try:
            sessions = _kms_json('admin', 'GET', '/kme/v1/pp/sessions').get('sessions') or []
        except Exception:
            continue
        for s in sessions:
            key = (s['peer'], s['session'])
            if s.get('first_kind') == 'offer' and key not in seen:
                seen.add(key)
                _queue_job('twoparty-bob', {'peer': s['peer'], 'session': s['session']})


def _job_evaluate(job):
    '''evaluation/evaluate.py on the chosen pairs: every stage with figures, kept in EVAL_DIR on this node'''
    p = job['params']
    pairs = _pairs_from(p)
    out = EVAL_DIR / f'{time.strftime("%Y%m%d_%H%M%S")}_job{job["id"]}'
    args = [sys.executable, str(REPO_ROOT / 'evaluation' / 'evaluate.py'), *[str(x) for pair in pairs for x in pair],
            '--out', str(out), '--importtt', IMPORTTT_BIN, '--jobs', str(int(p.get('jobs', 2)))]
    if UPLOADED_CONFIG_PATH.is_file():
        args += ['--config', str(UPLOADED_CONFIG_PATH)]
    if not p.get('live', True):
        args.append('--no-live')
    job['log'].append('$ evaluate.py ' + ' '.join(Path(a).name for a in args[2:]))
    proc = subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, cwd=str(REPO_ROOT))
    for line in proc.stdout:
        if line.startswith('[eval]') or 'Error' in line:
            job['log'].append(line.rstrip('\n'))
    if proc.wait() != 0:
        raise RuntimeError('evaluation failed: ' + ' / '.join(list(job['log'])[-3:]))
    res = json.loads((out / 'results.json').read_text())
    return {'evaluation': out.name, 'pairs': len(res.get('pairs', [])), 'pooled': res.get('pooled')}


def _evaluation_dir(name):
    d = (EVAL_DIR / name).resolve()
    if d.parent != EVAL_DIR.resolve() or not (d / 'results.json').is_file():
        raise FileNotFoundError(name)
    return d


@app.route('/api/evaluations')
def api_evaluations():
    out = []
    for d in sorted(EVAL_DIR.iterdir(), reverse=True):
        if (d / 'results.json').is_file():
            try:
                res = json.loads((d / 'results.json').read_text())
            except ValueError:
                continue
            out.append({'name': d.name, 'pairs': [p.get('name') for p in res.get('pairs', [])], 'created': d.stat().st_mtime})
    return jsonify({'evaluations': out})


@app.route('/api/evaluations/<name>')
def api_evaluation(name):
    try:
        d = _evaluation_dir(name)
    except FileNotFoundError:
        return jsonify({'error': 'no such evaluation'}), 404
    res = json.loads((d / 'results.json').read_text())
    for p in res.get('pairs', []):
        p['figures'] = sorted(f.name for f in (d / p.get('name', '')).glob('*.png')) if p.get('name') else []
    res['comparison'] = (d / 'comparison.png').is_file()
    return jsonify(res)


@app.route('/api/evaluations/<name>/<path:rel>')
def api_evaluation_file(name, rel):
    try:
        d = _evaluation_dir(name)
    except FileNotFoundError:
        return jsonify({'error': 'no such evaluation'}), 404
    f = (d / rel).resolve()
    if d not in f.parents or f.suffix not in ('.png', '.md', '.json') or not f.is_file():
        return jsonify({'error': 'no such file'}), 404
    return send_file(f)


_JOB_KINDS = {'evaluate': _job_evaluate, 'extract': _job_extract, 'calibrate': _job_calibrate,
              'twoparty-alice': _job_two_party_alice, 'twoparty-bob': _job_two_party_bob}


@app.route('/api/jobs', methods=['GET', 'POST'])
def api_jobs():
    if request.method == 'GET':
        return jsonify({'jobs': [_job_view(j, with_log=False) for j in reversed(_jobs.values())],
                        'delays_calibrated': DELAYS_PATH.is_file()})
    body = request.get_json(silent=True) or {}
    kind = body.get('kind')
    if kind not in _JOB_KINDS:
        return jsonify({'error': 'kind must be extract or calibrate'}), 400
    params = body.get('params') or {}
    if kind == 'twoparty-bob':
        return jsonify({'error': "Bob's half starts by itself when a neighbor announces"}), 400
    if kind == 'twoparty-alice':
        if params.get('peer') not in _neighbors():
            return jsonify({'error': 'choose a QKD link of this node'}), 400
        if not params.get('recordings') or any(n not in _list_recordings() for n in params['recordings']):
            return jsonify({'error': "tick this node's own recordings"}), 400
    else:
        try:
            _pairs_from(params)
        except ValueError as e:
            return jsonify({'error': str(e)}), 400
    link = params.get('link') or ''
    if kind == 'extract' and link and link not in _neighbors():
        return jsonify({'error': f'{link} is not a QKD link of this node'}), 400
    return jsonify(_job_view(_queue_job(kind, params)))


def _queue_job(kind, params):
    job = {'id': str(next(_job_ids)), 'kind': kind, 'status': 'queued', 'created': time.time(), 'started': None,
           'finished': None, 'params': params, 'result': None, 'error': None, 'log': collections.deque(maxlen=400)}
    _jobs[job['id']] = job
    while len(_jobs) > _JOBS_KEPT:
        _jobs.popitem(last=False)
    _job_queue.put(job)
    return job


@app.route('/api/jobs/<job_id>')
def api_job(job_id):
    job = _jobs.get(job_id)
    if job is None:
        return jsonify({'error': 'no such job'}), 404
    return jsonify(_job_view(job))


# CHSH diagnostic

_chsh_last = {'columns': None, 'rows': None, 'filename': None}


def _num(body, key, cast):
    v = body.get(key)
    return None if v in (None, '') else cast(v)


# a pulsed source needs ROI2 a whole number of repetition periods away; far beyond the peak is safe
_ROI2_OFFSET_WINDOW_MULTIPLE = 200
_ROI2_OFFSET_FLOOR_NS = 20.0


@app.route('/api/chsh/run', methods=['POST'])
def api_chsh_run():
    body = request.get_json(silent=True) or {}
    try:
        pair = _resolve_pair(body.get('a'), body.get('b'))
        with _prepared_dirs([pair]) as (dirs, reports):
            roi2_offset_ns = _num(body, 'roi2_offset_ns', float)
            auto = roi2_offset_ns is None
            if auto:
                window_ns = reports[0]['clock_skew']['corrected_window']['width_ps'] / 1000
                roi2_offset_ns = max(window_ns * _ROI2_OFFSET_WINDOW_MULTIPLE, _ROI2_OFFSET_FLOOR_NS)
            metadata, columns, rows = chsh_diagnostic.build_blocks(
                dirs[0], block_s=_num(body, 'block_s', float) or 1.0,
                roi1_width_ns=_num(body, 'roi1_width_ns', float), roi1_center_ns=_num(body, 'roi1_center_ns', float),
                roi2_offset_ns=roi2_offset_ns, roi2_width_ns=_num(body, 'roi2_width_ns', float))
    except Exception as e:
        return jsonify({'error': str(e)[:300]}), 400
    scale = metadata['roi1']['width_ps'] / metadata['roi2']['width_ps'] if metadata['roi2']['width_ps'] else 1.0
    pairs = []
    for name in metadata['pair_order']:
        i1, i2 = columns.index(f'roi1_{name}'), columns.index(f'roi2_{name}')
        r1, r2 = sum(r[i1] for r in rows), sum(r[i2] for r in rows)
        pairs.append({'pair': name, 'roi1_total': r1, 'roi2_total': r2, 'subtracted': round(r1 - r2 * scale, 2)})
    _chsh_last.update(columns=columns, rows=rows, filename=f'{_recording_base(pair[0].name)}_chsh_blocks.csv')
    return jsonify({'metadata': metadata, 'pairs': pairs, 'a': pair[0].name, 'b': pair[1].name, 'roi2_offset_auto': auto})


@app.route('/api/chsh/download')
def api_chsh_download():
    if _chsh_last['columns'] is None:
        return jsonify({'error': 'no CHSH diagnostic run yet'}), 404
    buf = io.StringIO()
    w = csv.writer(buf)
    w.writerow(_chsh_last['columns'])
    w.writerows(_chsh_last['rows'])
    resp = app.response_class(buf.getvalue(), mimetype='text/csv')
    resp.headers['Content-Disposition'] = f'attachment; filename="{_chsh_last["filename"]}"'
    return resp


_load_job_history()
threading.Thread(target=_job_worker, daemon=True).start()
threading.Thread(target=_two_party_listener, daemon=True).start()

if __name__ == '__main__':
    # Werkzeug's debugger runs arbitrary code for whoever reaches it: off unless asked, then localhost only
    debug = os.environ.get('FLASK_DEBUG') == '1'
    app.run(host='127.0.0.1' if debug else '0.0.0.0', port=9823, debug=debug)
