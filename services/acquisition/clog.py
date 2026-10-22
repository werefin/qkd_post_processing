'''
Coincidence log: singles and coincidences per integration window, appended to a CSV for the photonics datasets,
live from the TimeTaggers or offline from stored recordings
'''

import collections
import json
import signal
import sys
import time

import coincidences
from flask import Blueprint, jsonify, request, send_file
from werkzeug.utils import secure_filename

from ..core import procs
from ..core import recordings as rec
from ..core import settings
from . import config, tagger

bp = Blueprint('acquisition_clog', __name__)

LIVE_POINTS = 2400  # live points kept for the page, 20 minutes of half-second reads
state = {'proc': None, 'status': None, 'live': collections.deque(maxlen=LIVE_POINTS), 'live_n': 0, 'hist': None,
         'log': collections.deque(maxlen=400), 'params': None, 'started_at': None}


def running():
    return procs.running(state['proc'])


def _on_line(line):
    st = procs.prefixed_json(line, coincidences.STATUS_PREFIX)
    if st is not None:
        if st.get('live'):
            state['live'].append(st['live'])
            state['live_n'] += 1
        if st.get('histograms'):
            state['hist'] = st.pop('histograms')
        state['status'] = st
    # the logger's own lines and problems; one line per TimeTagger read would drown them
    elif '[log]' in line or line.startswith(('[WARN]', '[ERROR]', 'Traceback')):
        state['log'].append(line)


def _source_args(body, merged, cfg):
    '''The logger's source flags and a description: stored recordings offline, or one or two sites live'''
    kind = body.get('source', 'sites')
    if kind == 'recordings':
        names = [n for n in (body.get('a'), body.get('b')) if n]
        have = set(rec.names())
        if not names or any(n not in have for n in names) or len(set(names)) != len(names):
            raise ValueError('pick one or two different stored recordings')
        return kind, ['--recordings', *[str(settings.RECORDINGS_DIR / n) for n in names]], f'{len(names)} stored recording(s), offline'
    sites = [s for s in body.get('sites') or merged['sites'] or [] if s]
    if not 1 <= len(sites) <= 2 or any(s not in cfg.get('sites', {}) for s in sites) or len(set(sites)) != len(sites):
        raise ValueError('one site, or two different sites, of the loaded config')
    return kind, ['--sites', *sites], ' + '.join(sites)


@bp.route('/api/clog/options')
def api_options():
    '''Every option's default, the loaded config's section, and each site's channels'''
    cfg = config.load()
    return jsonify({'defaults': coincidences.DEFAULTS, 'config': cfg.get('coincidence_log', {}),
                    'site_channels': {n: s.get('channels', [1, 2, 3, 4]) for n, s in sorted(cfg.get('sites', {}).items())},
                    'loaded': bool(cfg)})


@bp.route('/api/clog/start', methods=['POST'])
def api_start():
    '''Logs the TimeTaggers, or stored recordings offline, one row per integration window'''
    if not config.loaded():
        return jsonify({'error': 'load a config first'}), 400
    body = request.get_json(silent=True) or {}
    options = body.get('options') or {}
    if not isinstance(options, dict):
        return jsonify({'error': 'options must be an object'}), 400
    options.pop('output_dir', None)  # logs stay in this node's directory
    cfg = config.load()
    args = [sys.executable, str(settings.ACQUISITION_DIR / 'coincidence_logger.py'), str(settings.UPLOADED_CONFIG_PATH),
            '--json', '--output-dir', str(settings.CLOG_DIR), '--options', json.dumps(options)]
    try:
        merged = coincidences.merge(cfg.get('coincidence_log', {}), options)
        if body.get('windows'):
            args += ['--windows', str(int(body['windows']))]
        if body.get('duration_s'):
            args += ['--duration-s', str(float(body['duration_s']))]
        kind, source_args, source = _source_args(body, merged, cfg)
    except (TypeError, ValueError) as e:
        return jsonify({'error': str(e)}), 400
    with tagger.lock:
        # stored recordings need no TimeTagger, only the log itself
        mode = ('a coincidence log' if running() else None) if kind == 'recordings' else tagger.busy()
        if mode:
            return jsonify(tagger.refusal(mode)), 409
        state['log'].clear()
        state['live'].clear()
        state.update(status=None, hist=None, live_n=0)
        try:
            proc = procs.start(args + source_args, _on_line)
        except OSError as e:
            return jsonify({'error': f'could not start: {e}'}), 500
        state.update(proc=proc, started_at=time.time(), params={'source': source, 'integration_s': merged['integration_s'], 'kind': kind})
    return jsonify({'started': True})


@bp.route('/api/clog/stop', methods=['POST'])
def api_stop():
    '''Stops the log; the window in progress is written as a last, incomplete row'''
    if not running():
        return jsonify({'error': 'no coincidence log is running'}), 409
    state['proc'].send_signal(signal.SIGINT)
    return jsonify({'stopping': True})


@bp.route('/api/clog/status')
def api_status():
    proc = state['proc']
    # points past the client's count; the oldest drop off the deque, so it may get fewer than it missed
    since = request.args.get('since', type=int) or 0
    live = list(state['live'])
    live = live[max(since - (state['live_n'] - len(live)), 0):]
    return jsonify({'running': running(), 'returncode': None if proc is None else proc.poll(),
                    'params': state['params'], 'started_at': state['started_at'], 'status': state['status'],
                    'histograms': state['hist'], 'live': live, 'live_total': state['live_n'],
                    'log': list(state['log'])[-150:]})


def _path(name):
    path = settings.CLOG_DIR / secure_filename(name)
    return path if name.endswith('_coincidences.csv') and path.is_file() else None


@bp.route('/api/clog/files')
def api_files():
    files = []
    for p in sorted(settings.CLOG_DIR.glob('*_coincidences.csv'), key=lambda p: p.stat().st_mtime, reverse=True):
        st = p.stat()
        with p.open(encoding='utf-8') as f:
            rows = sum(1 for line in f if line.strip() and not line.startswith('#')) - 1
        files.append({'name': p.name, 'bytes': st.st_size, 'rows': max(rows, 0), 'modified': st.st_mtime})
    return jsonify({'files': files})


@bp.route('/api/clog/files/<name>', methods=['GET', 'DELETE'])
def api_file(name):
    path = _path(name)
    if path is None:
        return jsonify({'error': 'no such log'}), 404
    if request.method == 'DELETE':
        if running() and state['status'] and state['status'].get('file') == path.name:
            return jsonify({'error': 'this log is being written'}), 409
        path.unlink()
        return jsonify({'deleted': name})
    return send_file(path, mimetype='text/csv', as_attachment=True, download_name=path.name)


@bp.route('/api/clog/files/<name>/data')
def api_file_data(name):
    '''A log's metadata, columns and rows, for the page's plots and table'''
    path = _path(name)
    if path is None:
        return jsonify({'error': 'no such log'}), 404
    try:
        meta, columns, rows = coincidences.read_log(path)
    except (ValueError, OSError) as e:
        return jsonify({'error': f'unreadable log: {e}'}), 400
    return jsonify({'name': path.name, 'meta': meta, 'columns': columns, 'rows': rows})


tagger.register('a coincidence log', running)
