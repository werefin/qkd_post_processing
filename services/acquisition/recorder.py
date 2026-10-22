'''
Two-party recorder: this node's TimeTagger recorded without end in UTC-aligned segments, which the block work of
continuous two-party QKD reads; singles per read and per segment for the monitor, and old segments dropped
'''

import collections
import os
import signal
import subprocess
import sys
import threading
import time
from pathlib import Path

import two_party_blocks as tpb
from flask import Blueprint, jsonify, request

from ..core import metrics, procs, settings
from ..core import recordings as rec
from . import config, tagger

bp = Blueprint('acquisition_recorder', __name__)

state = {'proc': None, 'since': None, 'params': None, 'keep_s': None, 'log': collections.deque(maxlen=300),
         'fast': collections.deque(maxlen=600), 'live': collections.deque(maxlen=1440), 'live_seen': set()}


def running():
    return procs.running(state['proc'])


def _on_line(line):
    if line.startswith('RATES '):
        r = procs.prefixed_json(line, 'RATES ') or {}
        if r.get('dt_s', 0) > 0:
            state['fast'].append({'t': r['t'], 'singles': [round(c / r['dt_s'], 1) for c in r['counts']]})
    elif 'detections this cycle' not in line:
        state['log'].append(line)


def _stop_stale():
    '''A recorder left by a service that died without its container would write the same segments twice'''
    try:
        pid = int(settings.TP_RECORDER_PID.read_text())
        cmdline = Path(f'/proc/{pid}/cmdline').read_bytes().decode(errors='replace')
    except (OSError, ValueError):
        return
    if 'tt_record_dual.py' not in cmdline or str(settings.TP_SEGMENTS) not in cmdline:
        return
    # asked politely first; an orphan whose output pipe broke with its parent may not get through that
    for sig, wait_s in ((signal.SIGINT, 15), (signal.SIGTERM, 5), (signal.SIGKILL, 5)):
        try:
            os.kill(pid, sig)
        except ProcessLookupError:
            return
        for _ in range(int(wait_s * 2)):
            if not Path(f'/proc/{pid}').exists():
                return
            time.sleep(0.5)


def _check(body):
    '''The recorder's settings from a start request'''
    cfg = config.load()
    site = body.get('site')
    if site not in cfg.get('sites', {}):
        raise ValueError("choose this node's site from the loaded config")
    segment_s = float(body.get('segment_s', 5))
    if not 1 <= segment_s <= 600:
        raise ValueError('segments of 1-600 s')
    replay = body.get('replay') or None
    if replay and replay not in rec.names():
        raise ValueError('the replay recording is not stored on this node')
    keep_s = float(body.get('keep_s') or 0) or None
    return {'site': site, 'segment_s': segment_s, 'replay': replay}, keep_s


def _start(params):
    '''Caller holds tagger.lock'''
    settings.TP_SEGMENTS.mkdir(parents=True, exist_ok=True)
    (settings.TP_SEGMENTS / '.recording_done').unlink(missing_ok=True)
    args = [sys.executable, str(settings.ACQUISITION_DIR / 'tt_record_dual.py'), str(settings.UPLOADED_CONFIG_PATH),
            '--site', params['site'], '--stop-mode', 'continuous', '--segment-s', str(params['segment_s']),
            '--output-dir', str(settings.TP_SEGMENTS), '--no-live']
    if params['replay']:
        args += ['--replay', str(settings.RECORDINGS_DIR / params['replay'])]  # a stored recording standing in, for tests
    _stop_stale()
    proc = procs.start(args, _on_line)
    state.update(proc=proc, since=time.time(), params=params)
    settings.TP_RECORDER_PID.write_text(str(proc.pid))


def stop():
    if running():
        state['proc'].send_signal(signal.SIGINT)
        try:
            state['proc'].wait(timeout=60)  # it writes its last segment first; a start right after must find it gone
        except subprocess.TimeoutExpired:
            state['proc'].kill()


def _segments():
    d = settings.TP_SEGMENTS
    return sorted(s.name for s in d.iterdir() if s.is_dir() and not s.name.startswith('.')) if d.is_dir() else []


def _live_singles():
    '''Singles per channel of every new segment, for the monitor's live view'''
    if not settings.TP_SEGMENTS.is_dir():
        return
    for d in sorted(settings.TP_SEGMENTS.iterdir()):
        if not d.is_dir() or d.name.startswith('.') or d.name in state['live_seen']:
            continue
        files = list(d.glob('*_timestamp_sequence.npy'))
        if len(files) != 1:
            continue
        try:
            rates, secs = rec.singles_per_s(files[0])
            state['live'].append({'t': tpb.segment_start(d.name), 'singles': rates, 'seconds': secs})
        except (OSError, ValueError):
            pass
        state['live_seen'].add(d.name)
    if len(state['live_seen']) > 5000:
        state['live_seen'] = set(sorted(state['live_seen'])[-2000:])


def _housekeeping():
    while True:
        time.sleep(2)
        try:
            _live_singles()
            if state['keep_s']:
                tpb.drop_old_segments(settings.TP_SEGMENTS, time.time() - state['keep_s'])
        except OSError:
            pass


@bp.route('/v1/recorder')
def api_status():
    '''singles_now averages the last four reads; brief=1 leaves out the series and the log'''
    segs = _segments()
    fast = list(state['fast'])[-4:]
    now = [round(sum(p['singles'][c] for p in fast) / len(fast), 1) for c in range(4)] if fast else None
    out = {'recording': running(), 'since': state['since'], 'params': state['params'], 'singles_now': now,
           'segments': len(segs), 'last_segment': segs[-1] if segs else None}
    if request.args.get('brief') != '1':
        out.update(live=list(state['live'])[-720:], fast=list(state['fast'])[-300:], log=list(state['log'])[-80:])
    return jsonify(out)


@bp.route('/v1/recorder/start', methods=['POST'])
def api_start():
    if not config.loaded():
        return jsonify({'error': 'load a config first'}), 400
    try:
        params, keep_s = _check(request.get_json(silent=True) or {})
    except (TypeError, ValueError) as e:
        return jsonify({'error': str(e)}), 400
    with tagger.lock:
        state['keep_s'] = keep_s or state['keep_s']
        if running():
            return jsonify({'started': False, 'recording': True, 'params': state['params']})
        if (mode := tagger.busy('the two-party recorder')):
            return jsonify(tagger.refusal(mode)), 409
        _start(params)
    return jsonify({'started': True, 'recording': True, 'params': params})


@bp.route('/v1/recorder/stop', methods=['POST'])
def api_stop():
    with tagger.lock:
        stop()
    return jsonify({'stopped': True})


def start_threads():
    threading.Thread(target=_housekeeping, daemon=True).start()


tagger.register('the two-party recorder', running)
metrics.gauge('recorder_running', lambda: int(running()))
metrics.gauge('recorder_segments', lambda: len(_segments()))
metrics.gauge('recorder_singles_per_s', lambda: {f'ch{c + 1}': v for c, v in enumerate(state['fast'][-1]['singles'])} if state['fast'] else {})
