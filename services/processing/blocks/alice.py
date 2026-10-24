'''
Alice's side of one block, streamed: each segment is announced as soon as it closes and its clicks dropped, keeping
only her bit and flags per click, and a node announcing to several neighbors makes each segment's announcement once; at the block's end both run their half of importtt, Bob on what he kept, and the
keys go to the link's import folder, the neighbor's to its own
'''

import collections
import hashlib
import secrets
import shutil
import tempfile
import threading
import time
from pathlib import Path

import numpy as np
import two_party_blocks as tpb
import two_party_stream as stream

from ...core import kms, settings
from . import importtt, store

SEGMENT_WAIT_S = 60  # after a segment should have closed, how long to wait for the recorder to write it
PACK_CHUNK = 1 << 26  # clicks packed at a time at the block's end, a multiple of 8
SQUASHED = '_squashed.npy'  # importtt's squashed channel per click, beside the segment, made with the announcement
CACHE_SEGMENTS = 3  # segments kept announced for the other links, which reach them moments later


def wait_segment(start, segment_s):
    '''This node's segment starting at start, once written; None if it never was'''
    deadline = start + segment_s + SEGMENT_WAIT_S
    while True:
        found = tpb.segments_in(settings.TP_SEGMENTS, start, start + 0.5)
        if found or time.time() > deadline:
            return found[0] if found else None
        time.sleep(1)


def _announce(peer, session, msg, blob):
    for part in stream.parts_of(blob):
        data = part.pop('blob')
        kms.pp_send(peer, session, 'cont-seg', {**msg, **part}, data=data)
    # Bob's receipt carries nothing but the index: it only keeps the mailbox from filling up
    kind, body = kms.pp_recv(peer, session)
    if kind == 'reject':
        raise RuntimeError(f'{peer} declined: {body.get("reason", kind)}')
    if kind != 'seg-ack' or body.get('index') != msg['index']:
        raise RuntimeError(f'expected {peer}\'s receipt of segment {msg["index"]}, got {kind}')


class Announcement:
    '''One segment as this node announces it, the same to every neighbor: its timing and bases encoded, and for
    this node alone each announced detection's bit and flag and the singles of the raw clicks'''

    def __init__(self, ch, squashed, pub_ts, flags):
        announced = squashed[squashed > 0]
        if announced.size != flags.size:
            raise ValueError(f'{announced.size} squashed detections for {flags.size} announced')
        self.blob = stream.encode(pub_ts, flags)
        self.bits = ((announced - 1) & 1).astype(np.uint8)
        self.flags = flags.astype(np.uint8)
        self.counts = np.bincount(ch[(ch >= 1) & (ch <= 4)].astype(np.int64), minlength=5)[1:5]
        self.seconds = max((int(pub_ts[-1]) - int(pub_ts[0])) / 1e12, 0.0) if pub_ts.size else 0.0

    @classmethod
    def of_segment(cls, seg):
        '''importtt squashes and exports the segment; the squashed channels are read and removed with it'''
        pub = Path(tempfile.mkdtemp(prefix='pub_', dir=settings.TP_WORK))
        try:
            importtt.export_public(seg, pub)
            pub_ts = np.load(next(pub.glob('*_public_timestamp_sequence.npy')))
            flags = np.load(next(pub.glob('*_public_flags.npy')))
        finally:
            shutil.rmtree(pub, ignore_errors=True)
        base = next(seg.glob('*' + tpb.TS)).name[:-len(tpb.TS)]
        ch = np.load(seg / (base + tpb.CH))
        squashed_file = seg / (base + SQUASHED)
        squashed = np.load(squashed_file)
        squashed_file.unlink()
        return cls(ch, squashed, pub_ts, flags)


_cache_lock = threading.Lock()
_cache = collections.OrderedDict()  # segment: {'lock', 'ann'}, the newest last


def announcement(seg):
    '''Segment's announcement, made once for every link this node announces to: the first link to reach it
    makes it, the others wait for it and take the same one'''
    with _cache_lock:
        slot = _cache.get(str(seg))
        if slot is None:
            slot = _cache[str(seg)] = {'lock': threading.Lock(), 'ann': None}
            while len(_cache) > CACHE_SEGMENTS:
                _cache.popitem(last=False)
        else:
            _cache.move_to_end(str(seg))
    with slot['lock']:
        if slot['ann'] is None:
            slot['ann'] = Announcement.of_segment(seg)
        return slot['ann']


class Kept:
    '''Alice's bit and announced flags of every click, appended to disk segment by segment and packed at the
    block's end: one bit and two bits a click, all importtt needs once the timing is announced'''

    def __init__(self, folder: Path):
        folder.mkdir(parents=True)
        self.folder, self.n = folder, 0
        self.bits, self.flags = open(folder / 'bits.u8', 'wb'), open(folder / 'flags.u8', 'wb')
        self.digest = hashlib.sha256()
        self.counts, self.seconds = np.zeros(4), 0.0

    def add(self, ann):
        self.bits.write(ann.bits.tobytes())
        self.flags.write(ann.flags.tobytes())
        self.digest.update(ann.blob)
        self.n += ann.bits.size
        self.counts += ann.counts
        self.seconds += ann.seconds

    def write(self):
        self.bits.close()
        self.flags.close()
        bits = np.memmap(self.folder / 'bits.u8', dtype=np.uint8, mode='r')
        flags = np.memmap(self.folder / 'flags.u8', dtype=np.uint8, mode='r')
        packed_bits = np.concatenate([np.packbits(bits[i:i + PACK_CHUNK]) for i in range(0, self.n, PACK_CHUNK)])
        packed_flags = np.concatenate([stream.pack_flags(flags[i:i + PACK_CHUNK]) for i in range(0, self.n, PACK_CHUNK)])
        del bits, flags
        own = self.folder / 'own'
        own.mkdir()
        np.save(own / 'block_alice_bits.npy', packed_bits.view(np.int8))
        np.save(own / 'block_alice_flags.npy', packed_flags.view(np.int8))
        (self.folder / 'bits.u8').unlink()
        (self.folder / 'flags.u8').unlink()
        return own

    def singles(self):
        return [round(float(c) / self.seconds, 1) if self.seconds else 0.0 for c in self.counts], round(self.seconds, 3)


def run(peer, start, block_s, segment_s, expected_qber):
    entry = {'role': 'alice', 'peer': peer, 'block': start, 'block_s': block_s, 'started': time.time(), 'segments': 0,
             'announced_bytes': 0}
    work = Path(tempfile.mkdtemp(prefix='alice_', dir=settings.TP_WORK))
    session = f'{settings.NODE_ID or "node"}-tp-{int(start)}-{secrets.token_hex(3)}'
    agreed = importtt.params(expected_qber)
    kept = Kept(work / 'kept')
    try:
        n_seg = int(round(block_s / segment_s))
        for k in range(n_seg):
            msg = {'block': start, 'block_s': block_s, 'segment_s': segment_s, 'index': k, 'segments': n_seg}
            if k == 0:
                msg['params'] = agreed
            seg = wait_segment(start + k * segment_s, segment_s)
            if seg is None:
                _announce(peer, session, {**msg, 'empty': True}, b'')
                continue
            ann = announcement(seg)
            _announce(peer, session, {**msg, 'segment': seg.name}, ann.blob)
            kept.add(ann)
            entry['segments'] += 1
            entry['announced_bytes'] += len(ann.blob)
            del ann
        if not kept.n:
            raise ValueError('no segments recorded in this block')
        entry['singles_local'], entry['seconds'] = kept.singles()
        own = kept.write()
        digest = kept.digest.hexdigest()
        kms.pp_send(peer, session, 'cont-block', {'singles': entry['singles_local'], 'seconds': entry['seconds'],
                                                  'clicks': kept.n, 'digest': digest})
        kind, body = kms.pp_recv(peer, session)
        if kind != 'accept':
            raise RuntimeError(f'{peer} declined: {body.get("reason", kind)}')
        entry['singles_remote'], entry['window_ps_peer'] = body.get('singles'), body.get('window_ps')
        entry['aligned_by_peer'], entry['skew_ppb'] = body.get('aligned_by'), body.get('skew_ppb')
        importtt.run(entry, 'alice', own, peer, session, agreed, streamed=(digest, kept.n))
        entry['ok'] = True
    except Exception as e:
        entry.update(ok=False, error=str(e)[:300])
        try:
            kms.pp_send(peer, session, 'reject', {'reason': str(e)[:300]})
        except Exception:
            pass
    finally:
        shutil.rmtree(work, ignore_errors=True)
    store.record(entry)
