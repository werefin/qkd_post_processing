'''
Acquisition config: sites (TimeTaggers and their settings), links between them, and the recorder's options;
uploaded once, kept on the data volume, readable only by this node
'''

import json
import os

from flask import Blueprint, jsonify, request

from ..core import settings
from ..core.config import load, loaded

bp = Blueprint('acquisition_config', __name__)


def validate(cfg):
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


def summary(cfg):
    return {
        'sites': sorted(cfg.get('sites', {})),
        'links': [{'name': n, 'site_a': l['site_a'], 'site_b': l['site_b']} for n, l in sorted(cfg.get('links', {}).items())],
        'default_link': (cfg.get('record_dual') or {}).get('link'),
    }


@bp.route('/api/config', methods=['GET', 'POST'])
def api_config():
    if request.method == 'GET':
        return jsonify({'loaded': True, **summary(load())} if loaded() else {'loaded': False})
    raw = request.files['file'].read() if request.files.get('file') else request.data
    if not raw:
        return jsonify({'error': 'no config file given'}), 400
    try:
        cfg = json.loads(raw)
    except ValueError as e:
        return jsonify({'error': f'invalid JSON: {e}'}), 400
    err = validate(cfg)
    if err:
        return jsonify({'error': err}), 400
    settings.UPLOADED_CONFIG_PATH.write_text(json.dumps(cfg, indent=2))
    os.chmod(settings.UPLOADED_CONFIG_PATH, 0o600)  # holds the TimeTaggers' auth keys
    return jsonify({'loaded': True, **summary(cfg)})
