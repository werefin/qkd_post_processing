'''
Processing service: everything that turns recordings into analysis and keys, the simulator included
'''

from ..core import settings, web
from . import alignment, blocks, chsh, evaluations, extraction, jobs, sessions, simulator, status, twoparty_jobs  # noqa: F401  job kinds register on import

BLUEPRINTS = [status.bp, simulator.bp, alignment.bp, jobs.bp, evaluations.bp, chsh.bp, blocks.bp, sessions.bp]


def start():
    jobs.start_threads()
    blocks.start_threads()


web.readiness('processing_data', web.writable(settings.CONFIG_DIR, settings.IMPORT_DIR, settings.TP_DIR))
