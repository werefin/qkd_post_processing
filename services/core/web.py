'''
Flask base every service starts from: security headers, the service token between services, the KMS and
peer errors as JSON, and /healthz, /readyz and /metrics
'''

import hmac
import os

from flask import Flask, jsonify, request

from . import metrics, settings
from .kms import KMSError
from .peers import PeerError

_OPEN_PATHS = ('/healthz', '/readyz', '/metrics')
_ready_checks = []  # (name, function returning None when ready, else what is missing)


def readiness(name, fn):
    _ready_checks.append((name, fn))


def writable(*dirs):
    '''Readiness check that the service's data directories are mounted and writable'''
    return lambda: next((f'{d} is not writable' for d in dirs if not os.access(d, os.W_OK)), None)


def base_app(service):
    app = Flask(f'qkd-{service}', template_folder=str(settings.UI_DIR / 'templates'),
                static_folder=str(settings.UI_DIR / 'static'))
    app.config['MAX_CONTENT_LENGTH'] = (settings.UPLOAD_MAX_MB * 1024 * 1024) or None
    app.config['SERVICE'] = service
    # the web front end and the all-in-one process answer the browser; the others only the services
    guarded = service not in ('web', 'all') and bool(settings.SERVICE_TOKEN)

    @app.before_request
    def _token():
        if guarded and request.path not in _OPEN_PATHS:
            if not hmac.compare_digest(request.headers.get('X-Service-Token', ''), settings.SERVICE_TOKEN):
                return jsonify({'error': 'this service answers the node\'s other services only'}), 403

    @app.after_request
    def _headers(resp):
        metrics.count_request(resp.status_code)
        resp.headers['X-Content-Type-Options'] = 'nosniff'
        resp.headers['X-Frame-Options'] = 'DENY'
        resp.headers['Referrer-Policy'] = 'no-referrer'
        resp.headers['Permissions-Policy'] = 'geolocation=(), microphone=(), camera=()'
        resp.headers['Content-Security-Policy'] = (
            "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; "
            "font-src 'self' https://fonts.gstatic.com; img-src 'self' data:; connect-src 'self'; "
            "object-src 'none'; base-uri 'self'; frame-ancestors 'none'; form-action 'self'"
        )
        if request.path.startswith(('/api/', '/v1/')):
            resp.headers['Cache-Control'] = 'no-store'
        resp.headers.pop('Server', None)
        return resp

    @app.errorhandler(KMSError)
    def _kms_error(e):
        return jsonify({'error': str(e)}), 503

    @app.errorhandler(PeerError)
    def _peer_error(e):
        return jsonify({'error': str(e)}), e.status

    @app.route('/healthz')
    def healthz():
        return 'ok'

    @app.route('/readyz')
    def readyz():
        missing = {}
        for name, fn in _ready_checks:
            try:
                why = fn()
            except Exception as e:
                why = str(e)[:200]
            if why:
                missing[name] = why
        return jsonify({'ready': not missing, 'service': service, 'missing': missing}), 503 if missing else 200

    @app.route('/metrics')
    def metrics_page():
        return jsonify({'service': service, 'metrics': metrics.collect()})

    return app
