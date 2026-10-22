'''
Neighbors' two-party sessions the agent noticed in the KMS mailbox: a manual announcement becomes a Bob job, a
continuous block goes to the block queue
'''

from flask import Blueprint, jsonify, request

from . import blocks, jobs

bp = Blueprint('processing_sessions', __name__)


@bp.route('/v1/sessions', methods=['POST'])
def api_session():
    body = request.get_json(silent=True) or {}
    peer, session, kind = body.get('peer'), body.get('session'), body.get('kind')
    if not peer or not session:
        return jsonify({'error': 'peer and session are required'}), 400
    if kind == 'offer':
        return jsonify({'job': jobs.queue_job('twoparty-bob', {'peer': peer, 'session': session})['id']})
    if kind == 'cont-offer':
        blocks.queue_bob(peer, session)
        return jsonify({'queued': True})
    return jsonify({'error': f'no work starts with a {kind} message'}), 400
