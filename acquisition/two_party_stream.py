'''
Continuous two-party QKD streamed per segment: Alice announces each segment as soon as it closes, in a compact
form, and keeps only her bit and flags per click; Bob aligns it to his own segment and keeps, for himself, the
announced clicks that can pair and their indices. Neither discloses anything the whole-block protocol does not:
at the block's end the usual protocol runs on what they kept, and Bob's pair list names the same clicks
'''

import base64
import struct

import numpy as np

MAGIC = b'QKDA2'
PART_BYTES = 48 << 20  # raw bytes per mailbox message: base64 and JSON stay well under its 128 MB
# Bob keeps an announced click with one of his within ALICE_RADIUS_PS: the coincidence filter looks +-3 ns, the
# rest is margin for the clock model; and his own within BOB_RADIUS_PS of a kept one, so his squashing (clicks
# within 2 ns of a first one) sees every neighbor of a click that can pair
ALICE_RADIUS_PS = 4_000
BOB_RADIUS_PS = 6_000


def _zigzag(d):
    return ((d << 1) ^ (d >> 63)).view(np.uint64)


def _unzigzag(z):
    return (z >> np.uint64(1)).view(np.int64) ^ -(z & np.uint64(1)).view(np.int64)


def _width(z):
    '''Bytes per gap, 2 to 4, that make the segment smallest: a gap that does not fit escapes to 64 bits'''
    return min((2, 3, 4), key=lambda w: z.size * w + 8 * int(np.count_nonzero(z >= np.uint64((1 << 8 * w) - 1))))


def encode(ts, flags) -> bytes:
    '''Alice's announcement of one segment: the first timestamp, every gap to the next in as few bytes as the
    segment needs (a signed gap folded to unsigned; one that does not fit escapes to 64 bits), and the flags at
    2 bits each; plain arrays, no compression, so a slow node keeps up'''
    ts = np.asarray(ts, dtype=np.int64)
    flags = np.asarray(flags, dtype=np.uint8)
    if ts.size != flags.size or np.any(flags > 3):
        raise ValueError('one flag of 0-3 per timestamp')
    if ts.size == 0:
        return MAGIC + struct.pack('<QqQB', 0, 0, 0, 4)
    z = _zigzag(np.diff(ts))
    w = _width(z)
    escape = np.uint64((1 << 8 * w) - 1)
    wide = z >= escape
    gaps = np.where(wide, escape, z).astype('<u4').view(np.uint8).reshape(-1, 4)[:, :w]
    packed = pack_flags(flags)
    return (MAGIC + struct.pack('<QqQB', ts.size, int(ts[0]), int(wide.sum()), w) + gaps.tobytes()
            + z[wide].astype('<u8').tobytes() + packed.astype(np.uint8).tobytes())


def decode(blob: bytes):
    '''(timestamps int64, flags int8) back from encode'''
    if blob[:len(MAGIC)] != MAGIC:
        raise ValueError('not a segment announcement')
    n, t0, n_wide, w = struct.unpack_from('<QqQB', blob, len(MAGIC))
    if n == 0:
        return np.empty(0, np.int64), np.empty(0, np.int8)
    off = len(MAGIC) + 25
    n_gaps = n - 1
    if w not in (2, 3, 4) or len(blob) != off + w * n_gaps + 8 * n_wide + (n + 3) // 4:
        raise ValueError('announcement of the wrong size')
    raw = np.zeros((n_gaps, 4), dtype=np.uint8)
    raw[:, :w] = np.frombuffer(blob, dtype=np.uint8, count=w * n_gaps, offset=off).reshape(-1, w)
    gaps = raw.view('<u4').reshape(-1).astype(np.uint64)
    at = np.flatnonzero(gaps == np.uint64((1 << 8 * w) - 1))
    if at.size != n_wide:
        raise ValueError('announcement escapes do not match')
    gaps[at] = np.frombuffer(blob, dtype='<u8', count=n_wide, offset=off + w * n_gaps)
    ts = np.empty(n, np.int64)
    ts[0] = t0
    np.cumsum(_unzigzag(gaps), out=ts[1:])
    ts[1:] += t0
    packed = np.frombuffer(blob, dtype=np.uint8, offset=off + w * n_gaps + 8 * n_wide)
    flags = np.stack([(packed >> s) & 3 for s in (0, 2, 4, 6)], axis=1).reshape(-1)[:n].astype(np.int8)
    return ts, flags


def near(x, ref, radius):
    '''Which of x have some ref within radius; ref need not be sorted'''
    ref = np.sort(ref) if ref.size > 1 and np.any(np.diff(ref) < 0) else ref
    if ref.size == 0:
        return np.zeros(x.size, bool)
    i = np.searchsorted(ref, x - radius, side='left')
    return (i < ref.size) & (ref[np.minimum(i, ref.size - 1)] <= x + radius)


def masks(alice_ts, bob_on_alice):
    '''Bob's own choice, never sent: the announced clicks that can pair, and his clicks to keep around them'''
    keep_a = near(alice_ts, bob_on_alice, ALICE_RADIUS_PS)
    keep_b = near(bob_on_alice, alice_ts[keep_a], BOB_RADIUS_PS)
    return keep_a, keep_b


def to_alice_clock(b, a0: int, b0: int, report: dict):
    '''Bob's timestamps on Alice's clock, exactly as importtt maps them with a coincidence report'''
    sk = report['clock_skew']
    ref, lag, skew = float(sk['reference_elapsed_ps']), float(sk['lag_at_reference_ps']), float(sk['skew_fraction'])
    rel = (np.asarray(b, np.int64) - b0).astype(np.float64)
    return a0 + np.round(ref + (rel - (ref + lag)) / (1 + skew)).astype(np.int64)


def identity_report(pub_name: str, own_name: str, a0: int, b0: int) -> dict:
    '''Report for clicks already on Alice's clock: importtt's mapping then leaves them where they are'''
    return {'input_a': pub_name, 'input_b': own_name, 'detected': True, 'channel_delays': None,
            'clock_skew': {'reference_elapsed_ps': 0.0, 'lag_at_reference_ps': float(a0 - b0), 'skew_fraction': 0.0,
                           'skew_ppb': 0.0, 'corrected_window': {'width_ps': 0}}}


def pack_flags(flags):
    '''2-bit flags four to a byte, lowest bits first, as importtt reads a streamed block'''
    flags = np.asarray(flags, dtype=np.uint8)
    padded = np.zeros((flags.size + 3) // 4 * 4, dtype=np.uint8)
    padded[:flags.size] = flags
    padded = padded.reshape(-1, 4)
    return (padded[:, 0] | padded[:, 1] << 2 | padded[:, 2] << 4 | padded[:, 3] << 6).astype(np.uint8)


def parts_of(blob: bytes) -> list:
    '''One segment's announcement as mailbox messages of at most PART_BYTES each'''
    chunks = [blob[i:i + PART_BYTES] for i in range(0, max(len(blob), 1), PART_BYTES)]
    return [{'part': i, 'parts': len(chunks), 'size': len(blob), 'data': base64.b64encode(c).decode()} for i, c in enumerate(chunks)]


def join_parts(parts: list) -> bytes:
    '''The announcement back from every part; refuses gaps, repeats and a wrong size'''
    parts = sorted(parts, key=lambda p: p['part'])
    if not parts or [p['part'] for p in parts] != list(range(parts[0]['parts'])):
        raise ValueError('announcement parts missing or repeated')
    blob = b''.join(base64.b64decode(p['data']) for p in parts)
    if len(blob) != parts[0]['size']:
        raise ValueError(f'announcement of {len(blob)} bytes, {parts[0]["size"]} announced')
    return blob
