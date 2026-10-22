'''
Alice's side of one block, streamed: each segment is announced as soon as it closes and its clicks dropped, keeping
only her bit and flags per click; at the block's end both run their half of importtt, Bob on what he kept, and the
keys go to the link's import folder, the neighbor's to its own
'''

import hashlib
import secrets
import shutil
import tempfile
import time
from pathlib import Path

import numpy as np
import two_party_blocks as tpb
import two_party_stream as stream

from ...core import kms, settings
from . import importtt, store

SEGMENT_WAIT_S = 60  # after a segment should have closed, how long to wait for the recorder to write it
PACK_CHUNK = 1 << 26  # clicks packed at a time at the block's end, a multiple of 8


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
        kms.pp_send(peer, session, 'cont-seg', {**msg, **part})
    # Bob's receipt carries nothing but the index: it only keeps the mailbox from filling up
    kind, body = kms.pp_recv(peer, session)
    if kind == 'reject':
        raise RuntimeError(f'{peer} declined: {body.get("reason", kind)}')
    if kind != 'seg-ack' or body.get('index') != msg['index']:
        raise RuntimeError(f'expected {peer}\'s receipt of segment {msg["index"]}, got {kind}')


class Kept:
    '''Alice's bit and announced flags of every click, appended to disk segment by segment and packed at the
    block's end: one bit and two bits a click, all importtt needs once the timing is announced'''

    def __init__(self, folder: Path):
        folder.mkdir(parents=True)
        self.folder, self.n = folder, 0
        self.bits, self.flags = open(folder / 'bits.u8', 'wb'), open(folder / 'flags.u8', 'wb')
        self.digest = hashlib.sha256()
        self.counts, self.seconds = np.zeros(4), 0.0

    def add(self, ch, flags, blob):
        valid = ch[(ch >= 1) & (ch <= 4)]
        self.bits.write(((valid - 1) & 1).astype(np.uint8).tobytes())
        self.flags.write(flags.astype(np.uint8).tobytes())
        self.digest.update(blob)
        self.n += valid.size
        self.counts += np.bincount(valid.astype(np.int64), minlength=5)[1:5]

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
    entry = {'role': 'alice', 'peer': peer, 'block': start, 'started': time.time(), 'segments': 0, 'announced_bytes': 0}
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
            pub = work / f'pub{k}'
            importtt.export_public(seg, pub)
            pub_ts = np.load(next(pub.glob('*_public_timestamp_sequence.npy')))
            flags = np.load(next(pub.glob('*_public_flags.npy')))
            shutil.rmtree(pub, ignore_errors=True)
            blob = stream.encode(pub_ts, flags)
            _announce(peer, session, {**msg, 'segment': seg.name}, blob)
            ts_file = next(seg.glob('*' + tpb.TS))
            ch = np.load(ts_file.with_name(ts_file.name[:-len(tpb.TS)] + tpb.CH))
            kept.add(ch, flags, blob)
            kept.seconds += max((int(pub_ts[-1]) - int(pub_ts[0])) / 1e12, 0.0)
            entry['segments'] += 1
            entry['announced_bytes'] += len(blob)
            del ch, pub_ts, flags, blob
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
