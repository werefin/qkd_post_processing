'''
Notices a neighbor starting a two-party session in the KMS mailbox and hands this node's half to the processing
service: a manual announcement or a continuous block
'''

import threading
import time

from ..core import kms, peers, settings

_KINDS = ('offer', 'cont-offer')


def _listen():
    seen = set()
    while True:
        time.sleep(3)
        if not (settings.TWO_PARTY_AUTO_ACCEPT and settings.KMS_URL):
            continue
        try:
            sessions = kms.call_json('admin', 'GET', '/kme/v1/pp/sessions').get('sessions') or []
        except Exception:
            continue
        for s in sessions:
            key = (s['peer'], s['session'])
            if key in seen or s.get('first_kind') not in _KINDS:
                continue
            try:
                peers.call_json('processing', 'POST', '/v1/sessions', {'peer': s['peer'], 'session': s['session'], 'kind': s['first_kind']})
                seen.add(key)
            except peers.PeerError:
                pass  # handed over on a later pass


def start_threads():
    threading.Thread(target=_listen, daemon=True).start()
