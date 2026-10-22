'''
Bob's side of one block: a neighbor's announcement answered with this node's own segments of the same time,
aligned to it from the previous block's clock model
'''

import shutil
import tempfile
import time
from pathlib import Path

import coincidence_peak
import two_party_blocks as tpb

from ...core import config, flags, kms, peers, settings
from ...core import recordings as rec
from . import importtt, store


def _recording():
    try:
        return peers.call_json('acquisition', 'GET', '/v1/recorder?brief=1', timeout=10).get('recording', False)
    except peers.PeerError:
        return False


def _announcement(peer, session):
    parts = []
    while True:
        kind, body = kms.pp_recv(peer, session, timeout=300)
        if kind != 'cont-offer':
            raise RuntimeError(f'expected a block announcement from {peer}, got {kind}')
        parts.append(body)
        if len(parts) >= body['parts']:
            return parts


def _align(entry, peer, parts, work):
    '''Joins this node's segments of the block beside the announcement and aligns them; the folder importtt reads'''
    start, block_s = float(parts[0]['block']), float(parts[0]['block_s'])
    files = tpb.join_announcement(parts)
    # this node's segments of the block: wait until the one after it exists, or the recorder stopped
    deadline = time.time() + 120
    while time.time() < deadline and _recording() and not tpb.segments_in(settings.TP_SEGMENTS, start + block_s, start + 2 * block_s + 3600):
        time.sleep(2)
    segs = tpb.segments_in(settings.TP_SEGMENTS, start, start + block_s)
    if not segs:
        raise ValueError('this node recorded nothing in that block')
    entry['segments'] = len(segs)
    pair = work / 'pair'
    own = tpb.join_segments(segs, pair)
    entry['singles_local'], entry['seconds'] = rec.singles_per_s(own)
    entry['singles_remote'] = parts[0].get('singles')
    entry['announced_bytes'] = sum(len(v) for v in files.values())
    for name, data in files.items():
        (pair / name).write_bytes(data)
    pub = next(pair.glob('*_public_timestamp_sequence.npy'))
    models = store.models()
    report, models[peer], entry['aligned_by'] = tpb.align_block(pub, own, models.get(peer),
                                                                coincidence_peak.kwargs_from_config(config.load()))
    entry['skew_ppb'] = report['clock_skew']['skew_ppb']
    store.save_models(models)
    return pair


def run(peer, session):
    entry = {'role': 'bob', 'peer': peer, 'started': time.time()}
    work = Path(tempfile.mkdtemp(prefix='bob_', dir=settings.TP_WORK))
    try:
        parts = _announcement(peer, session)
        entry['block'] = float(parts[0]['block'])
        try:
            pair = _align(entry, peer, parts, work)
        except Exception as e:
            kms.pp_send(peer, session, 'reject', {'reason': str(e)[:300]})
            raise
        kms.pp_send(peer, session, 'accept', {'singles': entry['singles_local'], 'window_ps': store.last_window(peer),
                                              'aligned_by': entry.get('aligned_by'), 'skew_ppb': entry.get('skew_ppb')})
        importtt.run(entry, 'bob', pair, peer, session, flags.agreed(parts[0].get('params')))
        entry['ok'] = True
    except Exception as e:
        entry.update(ok=False, error=str(e)[:300])
    finally:
        shutil.rmtree(work, ignore_errors=True)
    store.record(entry)
