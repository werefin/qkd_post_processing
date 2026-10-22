'''
Evaluations: evaluation/evaluate.py on chosen pairs, every stage with figures, kept in EVAL_DIR on this node
'''

import json
import subprocess
import sys
import time
from pathlib import Path

from flask import Blueprint, jsonify, send_file

from ..core import settings
from . import jobs
from .extraction import pairs_from

bp = Blueprint('processing_evaluations', __name__)


@jobs.kind('evaluate', check=pairs_from)
def evaluate(job):
    p = job['params']
    pairs = pairs_from(p)
    out = settings.EVAL_DIR / f'{time.strftime("%Y%m%d_%H%M%S")}_job{job["id"]}'
    args = [sys.executable, str(settings.REPO_ROOT / 'evaluation' / 'evaluate.py'), *[str(x) for pair in pairs for x in pair],
            '--out', str(out), '--importtt', settings.IMPORTTT_BIN, '--jobs', str(int(p.get('jobs', 2)))]
    if settings.UPLOADED_CONFIG_PATH.is_file():
        args += ['--config', str(settings.UPLOADED_CONFIG_PATH)]
    if not p.get('live', True):
        args.append('--no-live')
    job['log'].append('$ evaluate.py ' + ' '.join(Path(a).name for a in args[2:]))
    proc = subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, cwd=str(settings.REPO_ROOT))
    for line in proc.stdout:
        if line.startswith('[eval]') or 'Error' in line:
            job['log'].append(line.rstrip('\n'))
    if proc.wait() != 0:
        raise RuntimeError('evaluation failed: ' + ' / '.join(list(job['log'])[-3:]))
    res = json.loads((out / 'results.json').read_text())
    return {'evaluation': out.name, 'pairs': len(res.get('pairs', [])), 'pooled': res.get('pooled')}


def _dir(name):
    d = (settings.EVAL_DIR / name).resolve()
    if d.parent != settings.EVAL_DIR.resolve() or not (d / 'results.json').is_file():
        raise FileNotFoundError(name)
    return d


@bp.route('/api/evaluations')
def api_list():
    out = []
    for d in sorted(settings.EVAL_DIR.iterdir(), reverse=True):
        if (d / 'results.json').is_file():
            try:
                res = json.loads((d / 'results.json').read_text())
            except ValueError:
                continue
            out.append({'name': d.name, 'pairs': [p.get('name') for p in res.get('pairs', [])], 'created': d.stat().st_mtime})
    return jsonify({'evaluations': out})


@bp.route('/api/evaluations/<name>')
def api_one(name):
    try:
        d = _dir(name)
    except FileNotFoundError:
        return jsonify({'error': 'no such evaluation'}), 404
    res = json.loads((d / 'results.json').read_text())
    for p in res.get('pairs', []):
        p['figures'] = sorted(f.name for f in (d / p.get('name', '')).glob('*.png')) if p.get('name') else []
    res['comparison'] = (d / 'comparison.png').is_file()
    return jsonify(res)


@bp.route('/api/evaluations/<name>/<path:rel>')
def api_file(name, rel):
    try:
        d = _dir(name)
    except FileNotFoundError:
        return jsonify({'error': 'no such evaluation'}), 404
    f = (d / rel).resolve()
    if d not in f.parents or f.suffix not in ('.png', '.md', '.json') or not f.is_file():
        return jsonify({'error': 'no such file'}), 404
    return send_file(f)
