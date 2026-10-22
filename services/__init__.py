'''
One node's services, all from one image, each on its own port:

core/         settings, the shared web base, metrics, and clients for the KMS and the other services
agent/        9100: the node's link to the network controller, two-party scheduling, monitor and alerts
acquisition/  9101: the TimeTagger: config, recordings, live tracker, coincidence log, the two-party recorder
processing/   9102: simulator, alignment, jobs, evaluations, CHSH, and the two-party block work
web/          9823: the dashboard, passing each /api call to the service that owns it

python -m services NAME runs one of them; NAME "all" runs every service in one process, for a lab PC or tests
'''

from .core import settings  # noqa: F401  puts acquisition/ on the import path before any service module loads
