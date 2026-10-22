'''
Alice's side of one block: this node's segments joined, their public timing announced in parts, then her half;
the keys go to the link's import folder, the neighbor's to its own
'''

import secrets
import shutil
import tempfile
import time
from pathlib import Path

import two_party_blocks as tpb

from ...core import kms, settings
from ...core import recordings as rec
from . import importtt, store


def run(peer, start, block_s, expected_qber):
    entry = {'role': 'alice', 'peer': peer, 'block': start, 'started': time.time()}
    work = Path(tempfile.mkdtemp(prefix='alice_', dir=settings.TP_WORK))
    try:
        segs = tpb.segments_in(settings.TP_SEGMENTS, start, start + block_s)
        if not segs:
            raise ValueError('no segments recorded in this block')
        entry['segments'] = len(segs)
        own_ts = tpb.join_segments(segs, work / 'own')
        own = own_ts.parent
        entry['singles_local'], entry['seconds'] = rec.singles_per_s(own_ts)
        pub = work / 'pub'
        importtt.export_public(own, pub)
        files = {f.name: f.read_bytes() for f in pub.iterdir()}
        session = f'{settings.NODE_ID or "node"}-tp-{int(start)}-{secrets.token_hex(3)}'
        agreed = importtt.params(expected_qber)
        t_send = time.time()
        for part in tpb.announcement_parts(files, start):
            kms.pp_send(peer, session, 'cont-offer', {**part, 'block_s': block_s, 'params': agreed,
                                                      'singles': entry['singles_local'], 'seconds': entry['seconds']})
        entry['announced_bytes'], entry['announce_s'] = sum(len(v) for v in files.values()), time.time() - t_send
        kind, body = kms.pp_recv(peer, session)
        if kind != 'accept':
            raise RuntimeError(f'{peer} declined: {body.get("reason", kind)}')
        entry['singles_remote'], entry['window_ps_peer'] = body.get('singles'), body.get('window_ps')
        entry['aligned_by_peer'], entry['skew_ppb'] = body.get('aligned_by'), body.get('skew_ppb')
        importtt.run(entry, 'alice', own, peer, session, agreed)
        entry['ok'] = True
    except Exception as e:
        entry.update(ok=False, error=str(e)[:300])
    finally:
        shutil.rmtree(work, ignore_errors=True)
    store.record(entry)
