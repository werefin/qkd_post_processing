'''
Acquisition service: everything that drives this node's TimeTagger, one mode at a time
'''

from ..core import metrics, settings, web
from . import acquire, clog, config, continuous, recorder, recordings, tagger

BLUEPRINTS = [config.bp, recordings.bp, acquire.bp, continuous.bp, clog.bp, recorder.bp]


def start():
    recorder.start_threads()


metrics.gauge('config_loaded', lambda: int(config.loaded()))
metrics.gauge('timetagger_busy', lambda: int(tagger.busy() is not None))
web.readiness('acquisition_data', web.writable(settings.CONFIG_DIR, settings.RECORDINGS_DIR, settings.TP_DIR))
