'''
Every /api call of the dashboard passed to the service that owns it, streamed both ways so that uploads and
downloads of recordings never sit in memory; ROUTES is the whole map
'''

import http.client
import urllib.parse

from flask import Blueprint, Response, jsonify, request

from ..core import settings

bp = Blueprint('web_proxy', __name__)

# path prefix: service; a prefix owns itself and everything below it, and no two overlap
ROUTES = {
    '/api/node': 'agent', '/api/kms': 'agent', '/api/twoparty': 'agent', '/api/monitor': 'agent',
    '/api/alerts': 'agent', '/api/controller': 'agent',
    '/api/config': 'acquisition', '/api/upload': 'acquisition', '/api/recordings': 'acquisition',
    '/api/acquire': 'acquisition', '/api/clog': 'acquisition',
    '/api/status': 'processing', '/api/run': 'processing', '/api/align': 'processing', '/api/jobs': 'processing',
    '/api/evaluations': 'processing', '/api/chsh': 'processing',
}
CHUNK = 1 << 16
TIMEOUT_S = 3600  # a job's start or a stop that waits for the recorder can take a while
_PASSED = ('Content-Type', 'Content-Disposition', 'Content-Length', 'Cache-Control', 'Last-Modified', 'ETag')


def owner(path):
    return next((service for prefix, service in ROUTES.items() if path == prefix or path.startswith(prefix + '/')), None)


@bp.route('/api/<path:rest>', methods=['GET', 'POST', 'PUT', 'DELETE'])
def forward(rest):
    service = owner(request.path)
    if service is None:
        return jsonify({'error': 'no such API'}), 404
    url = urllib.parse.urlsplit(settings.service_url(service))
    conn = http.client.HTTPConnection(url.hostname, url.port or 80, timeout=TIMEOUT_S)
    target = request.full_path if request.query_string else request.path
    headers = {'X-Service-Token': settings.SERVICE_TOKEN}
    for name in ('Content-Type', 'Content-Length', 'Accept'):
        if request.headers.get(name):
            headers[name] = request.headers[name]
    try:
        conn.putrequest(request.method, target, skip_accept_encoding=True)
        for k, v in headers.items():
            conn.putheader(k, v)
        conn.endheaders()
        if request.content_length:
            while (chunk := request.stream.read(CHUNK)):
                conn.send(chunk)
        upstream = conn.getresponse()
    except OSError as e:
        conn.close()
        return jsonify({'error': f'the {service} service does not answer: {e}'}), 502

    def body():
        try:
            while (chunk := upstream.read(CHUNK)):
                yield chunk
        finally:
            conn.close()
    return Response(body(), status=upstream.status, headers={k: v for k in _PASSED if (v := upstream.getheader(k))})
