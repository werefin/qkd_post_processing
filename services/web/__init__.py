'''
Web service: the dashboard page and the proxy that hands each /api call to its service; Caddy in front adds HTTPS
and the login
'''

from . import pages, proxy

BLUEPRINTS = [pages.bp, proxy.bp]


def start():
    pass
