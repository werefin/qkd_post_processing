'''
Builds one service's Flask app: the shared base plus that service's blueprints and background threads; "all" puts
every service in one app, with the page but without the proxy, since every route is then local
'''

import importlib

from .core.web import base_app

SERVICES = ('agent', 'acquisition', 'processing', 'web')


def create_app(name='all', start=True):
    if name not in SERVICES + ('all',):
        raise ValueError(f'unknown service {name}; one of {", ".join(SERVICES)} or all')
    app = base_app(name)
    parts = [importlib.import_module(f'services.{n}') for n in (SERVICES[:3] if name == 'all' else (name,))]
    for part in parts:
        for bp in part.BLUEPRINTS:
            app.register_blueprint(bp)
    if name == 'all':
        from .web import pages
        app.register_blueprint(pages.bp)
    if start:
        for part in parts:
            part.start()
    return app
