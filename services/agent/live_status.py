'''
Every two seconds, this node's singles of the last reads to every neighbor, and theirs taken in, as the TimeTagger
vendor's monitor shows the remote count rates beside the local ones
'''

import collections
import threading
import time

from ..core import kms, peers, settings
from . import state

SESSION = 'live-status'
_remote = {}  # neighbor: deque of {t, singles, recording}


def remote(peer, n=150):
    return list(_remote.get(peer, []))[-n:]


def _mine():
    try:
        rec = peers.call_json('acquisition', 'GET', '/v1/recorder?brief=1', timeout=5)
    except peers.PeerError:
        return None
    return {'t': time.time(), 'recording': rec['recording'], 'singles': rec.get('singles_now')}


def _exchange():
    neighbors, neighbors_at = [], 0.0
    while True:
        time.sleep(2)
        if not settings.KMS_URL:
            continue
        if time.time() - neighbors_at > 60:
            neighbors, neighbors_at = kms.neighbors(), time.time()
        mine = _mine() if state.load()['active'] else None
        for peer in neighbors:
            if mine:
                try:
                    kms.pp_send(peer, SESSION, 'status', mine)
                except Exception:
                    pass
            for body in kms.pp_drain(peer, SESSION):  # whatever arrived since the last pass
                if body.get('singles'):
                    _remote.setdefault(peer, collections.deque(maxlen=600)).append(
                        {'t': body.get('t'), 'singles': body['singles'], 'recording': body.get('recording')})


def start_threads():
    threading.Thread(target=_exchange, daemon=True).start()
