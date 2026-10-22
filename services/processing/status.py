'''
Node summary for the dashboard's header: recordings on disk, simulator keys and the job in progress
'''

import time

from flask import Blueprint, jsonify

from ..core import recordings as rec
from ..core import settings
from . import jobs

bp = Blueprint('processing_status', __name__)


@bp.route('/api/status')
def api_status():
    return jsonify({
        'node_id': settings.NODE_ID,
        'kms': bool(settings.KMS_URL),
        'sim_keys': sum(1 for _ in settings.KEYSTORE_DIR.glob('*.json')),
        'recordings': rec.names(),
        'recordings_bytes': rec.total_bytes(),
        'job': jobs.current(),
        'timestamp': int(time.time()),
    })
