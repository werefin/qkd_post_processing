'''
Agent service: this node's link to the network controller and to its neighbors; schedules continuous two-party QKD,
watches it, raises alerts and serves the key management and monitor pages
'''

from ..core import settings, web
from . import alerts, controller, kms_api, listener, live_status, monitor, twoparty

BLUEPRINTS = [kms_api.bp, twoparty.bp, monitor.bp, alerts.bp, controller.bp]


def start():
    for part in (twoparty, alerts, listener, live_status, controller):
        part.start_threads()


web.readiness('agent_data', web.writable(settings.CONFIG_DIR))
