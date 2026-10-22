'''
Background jobs, one at a time: each kind registers how it runs and how its parameters are checked; the history
outlives a restart in jobs.jsonl
'''

import collections
import itertools
import json
import queue
import threading
import time

from flask import Blueprint, jsonify, request

from ..core import metrics, settings

bp = Blueprint('processing_jobs', __name__)

KEPT = 50
KINDS = {}  # kind: (run(job) returning the result, check(params) raising ValueError, or None if not started by hand)
_ids = itertools.count(1)
_jobs = collections.OrderedDict()
_queue = queue.Queue()


def kind(name, check=None):
    '''Registers a job kind; without check, only the node itself starts it'''
    def register(run):
        KINDS[name] = (run, check)
        return run
    return register


def _load_history():
    global _ids
    if not settings.JOBS_PATH.is_file():
        return
    for line in settings.JOBS_PATH.read_text().splitlines()[-KEPT:]:
        try:
            job = json.loads(line)
        except ValueError:
            continue
        job['log'] = collections.deque(job.get('log', []), maxlen=400)
        _jobs[job['id']] = job
    _ids = itertools.count(max([int(j) for j in _jobs] or [0]) + 1)


def view(job, with_log=True):
    v = {k: job[k] for k in ('id', 'kind', 'status', 'created', 'started', 'finished', 'params', 'result', 'error')}
    if with_log:
        v['log'] = list(job['log'])
    return v


def current():
    for job in reversed(_jobs.values()):
        if job['status'] in ('queued', 'running'):
            return {'id': job['id'], 'kind': job['kind'], 'status': job['status']}
    return None


def queue_job(name, params):
    job = {'id': str(next(_ids)), 'kind': name, 'status': 'queued', 'created': time.time(), 'started': None,
           'finished': None, 'params': params, 'result': None, 'error': None, 'log': collections.deque(maxlen=400)}
    _jobs[job['id']] = job
    while len(_jobs) > KEPT:
        _jobs.popitem(last=False)
    _queue.put(job)
    return job


def _worker():
    while True:
        job = _queue.get()
        job['status'], job['started'] = 'running', time.time()
        try:
            job['result'] = KINDS[job['kind']][0](job)
            job['status'] = 'done'
        except Exception as e:
            job['status'], job['error'] = 'failed', str(e)[:500]
        job['finished'] = time.time()
        try:
            with settings.JOBS_PATH.open('a') as f:
                f.write(json.dumps(view(job)) + '\n')
        except OSError:
            pass


@bp.route('/api/jobs', methods=['GET', 'POST'])
def api_jobs():
    if request.method == 'GET':
        return jsonify({'jobs': [view(j, with_log=False) for j in reversed(_jobs.values())],
                        'delays_calibrated': settings.DELAYS_PATH.is_file()})
    body = request.get_json(silent=True) or {}
    name = body.get('kind')
    if name not in KINDS:
        return jsonify({'error': f'kind must be one of {", ".join(k for k, (_, c) in KINDS.items() if c)}'}), 400
    check = KINDS[name][1]
    if check is None:
        return jsonify({'error': f'{name} starts by itself'}), 400
    params = body.get('params') or {}
    try:
        check(params)
    except ValueError as e:
        return jsonify({'error': str(e)}), 400
    return jsonify(view(queue_job(name, params)))


@bp.route('/api/jobs/<job_id>')
def api_job(job_id):
    job = _jobs.get(job_id)
    if job is None:
        return jsonify({'error': 'no such job'}), 404
    return jsonify(view(job))


def start_threads():
    _load_history()
    threading.Thread(target=_worker, daemon=True).start()


metrics.gauge('jobs_queued', lambda: _queue.qsize())
metrics.gauge('jobs_failed_recent', lambda: sum(1 for j in _jobs.values() if j['status'] == 'failed'))
