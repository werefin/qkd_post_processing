'''
Bob's side of one block, streamed: every segment Alice announces is aligned to this node's own segment of the
same time, from the previous clock model, and Bob keeps for himself the announced clicks that can pair, their
indices and his own clicks around them; he answers each segment with a bare receipt, and at the block's end both
run their half of importtt, his pair list naming the same clicks the whole announcement would
'''

import hashlib
import json
import shutil
import tempfile
import time
from pathlib import Path

import clock_tracker as ct
import coincidence_peak
import numpy as np
import two_party_blocks as tpb
import two_party_stream as stream

from ...core import config, flags, kms, settings
from . import importtt, store
from .alice import wait_segment

ANNOUNCE_WAIT_S = 900  # between two of Alice's segments: one segment's length, her export, and the link


class Kept:
    '''Bob's own selection, never sent: the announced clicks that can pair with their index in the whole
    announcement, and his clicks around them, already on Alice's clock so the block needs no alignment of its own'''

    def __init__(self):
        self.pub_ts, self.pub_flags, self.index, self.ts, self.ch = [], [], [], [], []
        self.counts, self.seconds, self.own_base, self.n = np.zeros(4), 0.0, None, 0
        self.digest = hashlib.sha256()

    def write(self, folder: Path):
        folder.mkdir(parents=True)
        pub_ts, ts = np.concatenate(self.pub_ts), np.concatenate(self.ts)
        if pub_ts.size == 0 or ts.size == 0:
            raise ValueError('no clicks that can pair in this block')
        pub_name, own_name = 'block_public_timestamp_sequence.npy', self.own_base + tpb.TS
        np.save(folder / pub_name, pub_ts)
        np.save(folder / 'block_public_flags.npy', np.concatenate(self.pub_flags))
        np.save(folder / 'block_public_index.npy', np.concatenate(self.index))
        np.save(folder / own_name, ts)
        np.save(folder / (self.own_base + tpb.CH), np.concatenate(self.ch))
        report = stream.identity_report(pub_name, own_name, int(pub_ts[0]), int(ts[0]))
        (folder / 'pair_coincidence.json').write_text(json.dumps(report))
        return folder


def _segment(peer, session, first):
    '''One announced segment: its header and the announcement's bytes, from as many parts as it took'''
    parts = [first]
    while len(parts) < first['parts']:
        kind, body = kms.pp_recv(peer, session, timeout=300)
        if kind != 'cont-seg' or body.get('index') != first['index']:
            raise RuntimeError(f'expected the rest of segment {first["index"]} from {peer}, got {kind}')
        parts.append(body)
    return first, b'' if first.get('empty') else stream.join_parts(parts)


def _keep(kept, entry, models, peer, head, a, a_flags, kwargs):
    '''Aligns this node's segment of the same time to the announced one and keeps what can pair'''
    seg = wait_segment(float(head['block']) + head['index'] * float(head['segment_s']), float(head['segment_s']))
    if seg is None:
        return
    ts_file = next(seg.glob('*' + tpb.TS))
    b, ch = np.load(ts_file), np.load(ts_file.with_name(ts_file.name[:-len(tpb.TS)] + tpb.CH))
    kept.own_base = kept.own_base or ts_file.name[:-len(tpb.TS)]
    valid = (ch >= 1) & (ch <= 4)
    kept.counts += np.bincount(ch[valid].astype(np.int64), minlength=5)[1:5]
    kept.seconds += max((int(b[-1]) - int(b[0])) / 1e12, 0.0)
    # the tracker predicts this segment's delay and skew from every earlier slice; lost or absent, a full search
    tracker = ct.ClockTracker.from_dict(models[peer]) if 'kf' in models.get(peer, {}) else None
    hint = tracker.model_at(int(a[0])) if tracker and not tracker.lost else None
    try:
        report, keep, model, how = tpb.align_arrays(a, b, hint, kwargs)
    except ValueError:
        # too few clicks to align, as a segment cut short by a start: nothing of it is kept, the rest still pairs
        entry['skipped'] = entry.get('skipped', 0) + 1
        return
    if hint is None or how == 'full search':
        tracker = ct.ClockTracker.from_model(model)
    entry.setdefault('aligned_by', how)
    pb = b[keep]
    b_on_a = stream.to_alice_clock(pb, int(a[0]), int(pb[0]), report)
    centres, width = ct.slice_centres(a, b_on_a, pb)
    sync = tracker.feed(centres)
    sync.update(t=int(a[0]), at=time.time(), how=how, peak_sigma_ps=None if width is None else round(width, 1))
    store.add_sync(peer, sync)
    models[peer] = tracker.to_dict()
    entry['skew_ppb'], entry['sync'] = sync['skew_ppb'], sync
    keep_a, keep_b = stream.masks(a, b_on_a)
    kept.pub_ts.append(a[keep_a])
    kept.pub_flags.append(a_flags[keep_a])
    kept.index.append(kept.n + np.flatnonzero(keep_a))
    kept.ts.append(b_on_a[keep_b])
    kept.ch.append(ch[keep][keep_b])
    entry['segments'] += 1


def run(peer, session):
    entry = {'role': 'bob', 'peer': peer, 'started': time.time(), 'segments': 0, 'announced_bytes': 0}
    work = Path(tempfile.mkdtemp(prefix='bob_', dir=settings.TP_WORK))
    kept, models, agreed = Kept(), store.models(), None
    kwargs = coincidence_peak.kwargs_from_config(config.load())
    try:
        try:
            while True:
                kind, body = kms.pp_recv(peer, session, timeout=ANNOUNCE_WAIT_S)
                if kind == 'cont-block':
                    break
                if kind != 'cont-seg':
                    raise RuntimeError(f'expected a segment from {peer}, got {kind}')
                head, blob = _segment(peer, session, body)
                if head['index'] == 0:
                    agreed = flags.agreed(head.get('params'))
                    entry['block'] = float(head['block'])
                if blob:
                    a, a_flags = stream.decode(blob)
                    kept.digest.update(blob)
                    entry['announced_bytes'] += len(blob)
                    _keep(kept, entry, models, peer, head, a, a_flags, kwargs)
                    kept.n += a.size
                    del a, a_flags, blob
                kms.pp_send(peer, session, 'seg-ack', {'index': head['index']})
            store.save_models(models)
            if agreed is None or kept.own_base is None:
                raise ValueError('this node recorded nothing in that block')
            if body.get('clicks') != kept.n or body.get('digest') != kept.digest.hexdigest():
                raise ValueError('the announcement received differs from the one announced')
            entry['singles_remote'] = body.get('singles')
            entry['singles_local'] = [round(float(c) / kept.seconds, 1) if kept.seconds else 0.0 for c in kept.counts]
            entry['seconds'] = round(kept.seconds, 3)
            pair = kept.write(work / 'pair')
            entry['kept_clicks'] = int(sum(t.size for t in kept.pub_ts))
        except Exception as e:
            kms.pp_send(peer, session, 'reject', {'reason': str(e)[:300]})
            raise
        kms.pp_send(peer, session, 'accept', {'singles': entry['singles_local'], 'window_ps': store.last_window(peer),
                                              'aligned_by': entry.get('aligned_by'), 'skew_ppb': entry.get('skew_ppb')})
        importtt.run(entry, 'bob', pair, peer, session, agreed, streamed=(kept.digest.hexdigest(), kept.n))
        entry['ok'] = True
    except Exception as e:
        entry.update(ok=False, error=str(e)[:300])
    finally:
        shutil.rmtree(work, ignore_errors=True)
    store.record(entry)
