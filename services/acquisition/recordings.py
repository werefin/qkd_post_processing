'''
Stored recordings: upload, list and delete
'''

from flask import Blueprint, jsonify, request
from werkzeug.utils import secure_filename

from ..core import recordings as rec
from ..core import settings

bp = Blueprint('acquisition_recordings', __name__)


@bp.route('/api/upload', methods=['POST'])
def api_upload():
    saved, skipped = [], []
    for f in request.files.getlist('files'):
        name = secure_filename(f.filename or '')
        if not name or not name.endswith(rec.SUFFIXES):
            skipped.append(f.filename or '(unnamed)')
            continue
        f.save(settings.RECORDINGS_DIR / name)
        saved.append(name)
    if saved:
        rec.enforce_cap()
    return jsonify({'saved': saved, 'skipped': skipped, 'recordings': rec.names()})


@bp.route('/api/recordings')
def api_recordings():
    names = rec.names()
    items = [{'base': rec.base(n), 'name': n, 'bytes': sum(p.stat().st_size for p in rec.group_paths(rec.base(n)))} for n in names]
    return jsonify({'files': names, 'items': items, 'pairs': rec.pairs(),
                    'total_bytes': rec.total_bytes(), 'max_gb': settings.RECORDINGS_MAX_GB})


@bp.route('/api/recordings', methods=['DELETE'])
def api_delete_all():
    freed = sum(rec.delete(rec.base(n)) for n in rec.names())
    return jsonify({'freed_bytes': freed, 'recordings': rec.names()})


@bp.route('/api/recordings/<base>', methods=['DELETE'])
def api_delete_one(base):
    base = secure_filename(base)
    if not (settings.RECORDINGS_DIR / f'{base}_timestamp_sequence.npy').is_file():
        return jsonify({'error': f'no such recording: {base}'}), 404
    return jsonify({'deleted': base, 'freed_bytes': rec.delete(base), 'recordings': rec.names()})
