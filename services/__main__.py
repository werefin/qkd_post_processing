'''
python -m services [NAME]: runs one service under gunicorn on its port (NAME defaults to $SERVICE, else all);
python -m services check probes the running service's /healthz, for the container health check
'''

import os
import sys
import urllib.request


def _check():
    from .core import settings
    try:
        urllib.request.urlopen(f'http://127.0.0.1:{settings.PORT}/healthz', timeout=4).close()
    except OSError as e:
        print(f'unhealthy: {e}', file=sys.stderr)
        return 1
    return 0


def main(argv):
    name = argv[1] if len(argv) > 1 else os.environ.get('SERVICE', 'all')
    if name == 'check':
        return _check()
    os.environ['SERVICE'] = name
    from .core import settings
    from .serve import create_app
    if os.environ.get('FLASK_DEBUG') == '1':
        # Werkzeug's debugger runs arbitrary code for whoever reaches it: localhost only
        create_app(name).run(host='127.0.0.1', port=settings.PORT, debug=True, use_reloader=False)
        return 0
    from gunicorn.app.base import BaseApplication

    class Service(BaseApplication):
        def load_config(self):
            # one worker: jobs, caches and the TimeTagger's processes live in this process; threads serve requests
            for key, value in {'bind': f'0.0.0.0:{settings.PORT}', 'workers': 1, 'threads': 16 if name == 'web' else 8,
                               'worker_class': 'gthread', 'timeout': 3600, 'graceful_timeout': 10, 'worker_tmp_dir': '/tmp',
                               'accesslog': None, 'proc_name': f'qkd-{name}'}.items():
                self.cfg.set(key, value)

        def load(self):
            return create_app(name)

    Service().run()
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv))
