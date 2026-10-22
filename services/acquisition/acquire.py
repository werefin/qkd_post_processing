'''
One recording or replay at a time, through the live coincidence tracker: tt_record_dual.py records both sites
of a link, live_peak.py replays two stored recordings at any speed
'''

import collections
import sys
import time

from flask import Blueprint, jsonify, request

from ..core import procs
from ..core import recordings as rec
from ..core import settings
from . import config, tagger

bp = Blueprint('acquisition_acquire', __name__)

LIVE_PREFIX = 'LIVE '  # live_peak.emit: one JSON status of the live coincidence tracker per line
state = {'proc': None, 'log': collections.deque(maxlen=1000), 'started_at': None, 'live': None, 'kind': None}


def running():
    return procs.running(state['proc'])


def _on_line(line):
    live = procs.prefixed_json(line, LIVE_PREFIX)
    if live is not None:
        state['live'] = live
    elif not line.startswith(LIVE_PREFIX):
        state['log'].append(line)


def start(args, kind):
    '''Starts the one acquisition process; caller holds tagger.lock and has checked it is free'''
    state['log'].clear()
    state['live'] = None
    try:
        proc = procs.start(args, _on_line, on_exit=rec.enforce_cap)
    except OSError as e:
        return jsonify({'error': f'could not start: {e}'}), 500
    state.update(proc=proc, started_at=time.time(), kind=kind)
    return jsonify({'started': True})


@bp.route('/api/acquire/start', methods=['POST'])
def api_start():
    if not config.loaded():
        return jsonify({'error': 'load a config first'}), 400
    body = request.get_json(silent=True) or {}
    args = [sys.executable, str(settings.ACQUISITION_DIR / 'tt_record_dual.py'), str(settings.UPLOADED_CONFIG_PATH),
            '--duration', str(float(body.get('duration_s', 60))), '--output-dir', str(settings.RECORDINGS_DIR)]
    if body.get('link'):
        args += ['--link', str(body['link'])]
    with tagger.lock:
        if (mode := tagger.busy()):
            return jsonify(tagger.refusal(mode)), 409
        return start(args, 'recording')


@bp.route('/api/acquire/replay', methods=['POST'])
def api_replay():
    '''Streams two stored recordings through the live tracker at the given speed, with the loaded config's settings'''
    if not config.loaded():
        return jsonify({'error': 'load a config first'}), 400
    body = request.get_json(silent=True) or {}
    try:
        path_a, path_b = rec.resolve_pair(body.get('a'), body.get('b'))
        speed = float(body.get('speed', 1.0))
    except (ValueError, TypeError) as e:
        return jsonify({'error': str(e)}), 400
    args = [sys.executable, str(settings.ACQUISITION_DIR / 'live_peak.py'), str(path_a), str(path_b),
            '--speed', str(max(speed, 0.0)), '--json', '--config', str(settings.UPLOADED_CONFIG_PATH)]
    with tagger.lock:
        if (mode := tagger.busy()):
            return jsonify(tagger.refusal(mode)), 409
        return start(args, 'replay')


@bp.route('/api/acquire/stop', methods=['POST'])
def api_stop():
    with tagger.lock:
        if not running():
            return jsonify({'error': 'nothing is running'}), 409
        state['proc'].send_signal(2)  # SIGINT: tt_record_dual.py handles it like Ctrl+C
    return jsonify({'stopping': True})


@bp.route('/api/acquire/status')
def api_status():
    proc = state['proc']
    return jsonify({'running': running(), 'returncode': None if proc is None else proc.poll(),
                    'kind': state['kind'], 'started_at': state['started_at'], 'log': list(state['log'])[-200:]})


@bp.route('/api/acquire/live')
def api_live():
    '''The live coincidence tracker's latest status, from the running or last recording or replay'''
    return jsonify({'running': running(), 'kind': state['kind'], 'status': state['live']})


tagger.register('a recording or replay', running)
