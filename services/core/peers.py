'''
Calls to the node's other services, with the service token; JSON in and out
'''

import json
import urllib.error
import urllib.request

from . import settings


class PeerError(Exception):
    '''A service that failed or answered with an error; status is what to answer the caller with'''

    def __init__(self, message, status=502):
        super().__init__(message)
        self.status = status


def call(service, method, path, body=None, timeout=30):
    '''Returns (status, json); raises PeerError only when the service cannot be reached'''
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(settings.service_url(service) + path, data=data, method=method,
                                 headers={'Content-Type': 'application/json', 'X-Service-Token': settings.SERVICE_TOKEN})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read() or b'{}')
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read() or b'{}')
        except ValueError:
            return e.code, {'error': str(e.reason)}
    except (urllib.error.URLError, OSError) as e:
        raise PeerError(f'the {service} service does not answer: {getattr(e, "reason", e)}')


def call_json(service, method, path, body=None, timeout=30):
    '''The answer of a call that must succeed; an error answer is passed on with its own status'''
    status, data = call(service, method, path, body, timeout)
    if status >= 400:
        raise PeerError(data.get('error') or f'the {service} service answered {status}', status)
    return data
