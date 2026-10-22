'''
Simulated pipeline (cmd/qkd_post_processing -json); its keys are demo keys, replace the previous run's, and never
reach the KMS
'''

import json
import subprocess

from flask import Blueprint, jsonify, request

from ..core import flags, settings

bp = Blueprint('processing_simulator', __name__)

_last_keys = set()


@bp.route('/api/run', methods=['POST'])
def api_run():
    global _last_keys
    body = request.get_json(silent=True) or {}
    args = [settings.QKD_BIN, '-json', '-keystore', str(settings.KEYSTORE_DIR)]
    for flag, key, cast in (('-n', 'n', int), ('-err', 'err', float), ('-multiphoton', 'multiphoton', float),
                            ('-window', 'window', int), ('-jitter', 'jitter', int), ('-seed', 'seed', int)):
        flags.add(body, args, flag, key, cast)
    flags.method_args(body, args, real=False)
    for p in _last_keys:
        p.unlink(missing_ok=True)
    before = set(settings.KEYSTORE_DIR.glob('*.json'))
    try:
        proc = subprocess.run(args, capture_output=True, text=True, timeout=300)
    except subprocess.TimeoutExpired:
        return jsonify({'error': 'simulation timed out'}), 504
    except FileNotFoundError:
        return jsonify({'error': f'{settings.QKD_BIN} not found'}), 500
    if proc.returncode != 0:
        return jsonify({'error': (proc.stderr or 'simulation failed').strip()[:400]}), 500
    try:
        out = json.loads(proc.stdout.strip().splitlines()[-1])
    except (ValueError, IndexError):
        return jsonify({'error': 'could not parse simulator output'}), 500
    _last_keys = set(settings.KEYSTORE_DIR.glob('*.json')) - before
    out['keys_in_store'] = len(_last_keys)
    return jsonify(out)
