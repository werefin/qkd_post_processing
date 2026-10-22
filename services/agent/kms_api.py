'''
Key management pages: this node and its links, the audit trail, and ETSI 014 calls as the dashboard's SAE,
which show key IDs and fingerprints, never keys
'''

from flask import Blueprint, jsonify, request

from ..core import kms, settings

bp = Blueprint('agent_kms', __name__)


@bp.route('/api/node')
def api_node():
    node = kms.call_json('admin', 'GET', '/kme/v1/node?probe=1')
    node['network'] = kms.call_json('admin', 'GET', '/kme/v1/network')['network']
    node['sae_id'] = settings.SAE_ID
    return jsonify(node)


@bp.route('/api/kms/audit')
def api_audit():
    n = int(request.args.get('n') or 200)
    return jsonify(kms.call_json('admin', 'GET', f'/kme/v1/audit?n={n}'))


@bp.route('/api/kms/status')
def api_status():
    return jsonify(kms.call_json('sae', 'GET', f'/api/v1/keys/{kms.quote(request.args.get("slave", ""))}/status'))


@bp.route('/api/kms/enc', methods=['POST'])
def api_enc():
    '''ETSI Get Key as this dashboard's SAE'''
    body = request.get_json(silent=True) or {}
    slave = str(body.get('slave', ''))
    req = {'number': int(body.get('number') or 1), 'size': int(body.get('size') or 256)}
    data = kms.call_json('sae', 'POST', f'/api/v1/keys/{kms.quote(slave)}/enc_keys', req)
    return jsonify({'master': settings.SAE_ID, 'slave': slave,
                    'keys': [{'key_ID': k['key_ID'], 'fingerprint': kms.fingerprint(k['key'])} for k in data['keys']]})


@bp.route('/api/kms/dec', methods=['POST'])
def api_dec():
    '''ETSI Get Key with Key IDs as this dashboard's SAE'''
    body = request.get_json(silent=True) or {}
    master = str(body.get('master', ''))
    ids = [str(i).strip() for i in body.get('key_ids') or [] if str(i).strip()]
    data = kms.call_json('sae', 'POST', f'/api/v1/keys/{kms.quote(master)}/dec_keys', {'key_IDs': [{'key_ID': i} for i in ids]})
    return jsonify({'master': master, 'slave': settings.SAE_ID,
                    'keys': [{'key_ID': k['key_ID'], 'fingerprint': kms.fingerprint(k['key'])} for k in data['keys']]})
