'''
Continuous two-party QKD, one block at a time: each node records its own TimeTagger in segments that start on
the same UTC seconds at both sites; a block is the segments of one stretch of time, joined into one recording.
Alice announces her block's public timing in parts small enough for the KMS mailbox, Bob aligns his own block to
it from the previous block's clock model, and both run their half of importtt with the neighbor
'''

import base64
import json
import shutil
from datetime import datetime, timezone
from pathlib import Path

import numpy as np

import coincidence_peak as cp

PART_BYTES = 48 << 20  # raw bytes per mailbox message: base64 and JSON stay well under its 128 MB
PRUNE_PS = 50_000      # Bob's clicks kept around a predicted partner, as in continuous_qkd.py
HEAD_S = 30.0          # seconds searched in full when no clock model holds yet
TS, CH, META = '_timestamp_sequence.npy', '_channel_sequence.npy', '_meta.json'


def segment_start(name: str) -> float:
    '''A segment directory's UTC start, from its name'''
    return datetime.strptime(name, '%Y%m%d_%H%M%S').replace(tzinfo=timezone.utc).timestamp()


def segments_in(seg_dir: Path, start: float, end: float) -> list:
    '''Complete segment directories starting in [start, end), oldest first'''
    out = []
    for d in Path(seg_dir).iterdir():
        if d.is_dir() and not d.name.startswith('.'):
            try:
                t = segment_start(d.name)
            except ValueError:
                continue
            if start <= t < end:
                out.append((t, d))
    return [d for _, d in sorted(out)]


def join_segments(segments: list, out_dir: Path) -> Path:
    '''The segments' clicks as one recording in out_dir, named after the first; returns its timestamp file'''
    out_dir.mkdir(parents=True, exist_ok=True)
    ts_parts, ch_parts, base = [], [], None
    for d in segments:
        files = sorted(d.glob('*' + TS))
        if len(files) != 1:
            raise ValueError(f'{d.name}: expected one recording, found {len(files)}')
        if base is None:
            base = files[0].name[:-len(TS)]
        ts_parts.append(np.load(files[0]))
        ch_parts.append(np.load(files[0].with_name(files[0].name[:-len(TS)] + CH)))
    if base is None:
        raise ValueError('no segments to join')
    ts = np.concatenate(ts_parts)
    if ts.size > 1 and np.any(np.diff(ts) < 0):
        raise ValueError('the segments are not in time order')
    np.save(out_dir / (base + TS), ts)
    np.save(out_dir / (base + CH), np.concatenate(ch_parts))
    (out_dir / (base + META)).write_text(json.dumps({'segments': [d.name for d in segments], 'n_detections': int(ts.size)}))
    return out_dir / (base + TS)


def announcement_parts(files: dict, block: float) -> list:
    '''Alice's public files (name: bytes) as mailbox messages of at most PART_BYTES each'''
    pieces = []
    for name, data in sorted(files.items()):
        for off in range(0, max(len(data), 1), PART_BYTES):
            pieces.append((name, off, data[off:off + PART_BYTES], len(data)))
    return [{'block': block, 'part': i, 'parts': len(pieces), 'name': name, 'offset': off, 'size': size,
             'data': base64.b64encode(chunk).decode()} for i, (name, off, chunk, size) in enumerate(pieces)]


def join_announcement(parts: list) -> dict:
    '''The files back from every part; refuses gaps, overlaps and unexpected names'''
    parts = sorted(parts, key=lambda p: p['part'])
    if not parts or [p['part'] for p in parts] != list(range(parts[0]['parts'])):
        raise ValueError('announcement parts missing or repeated')
    files: dict = {}
    for p in parts:
        name = p['name']
        if Path(name).name != name or not name.endswith(('_public_timestamp_sequence.npy', '_public_flags.npy')):
            raise ValueError(f'unexpected announcement file {name}')
        buf = files.setdefault(name, bytearray())
        if p['offset'] != len(buf):
            raise ValueError(f'{name}: part at {p["offset"]} after {len(buf)} bytes')
        buf += base64.b64decode(p['data'])
        if len(buf) > p['size']:
            raise ValueError(f'{name}: more bytes than announced')
    for p in parts:
        if len(files[p['name']]) != p['size']:
            raise ValueError(f'{p["name"]}: {len(files[p["name"]])} of {p["size"]} bytes')
    return {k: bytes(v) for k, v in files.items()}


def _hinted(a, b, model, kwargs):
    '''The search from a clock model: Bob's clicks pruned to where Alice's predict a partner, the predicted skew
    taken out of them and only the residual searched; None if the model no longer holds. Returns the report on
    Alice's full announcement and Bob's pruned clicks, and which of Bob's clicks to keep'''
    _, keep_b = cp.prune_to_model(a, b, model['delay_ref'], model['slope'], model['t_ref'], PRUNE_PS)
    pb = b[keep_b]
    if pb.size < 100:
        return None
    a0, b0 = int(a[0]), int(pb[0])
    hint = int(round(model['delay_ref'] + model['slope'] * (a0 - model['t_ref']) / cp.PS_PER_S)) - (b0 - a0)
    s_p = model['slope'] / cp.PS_PER_S
    pb_corr = pb - np.round((pb - b0) * s_p).astype(np.int64)
    report, _ = cp.analyze_arrays(a, pb_corr, strict=False, lag_hint_ps=hint, lag_hint_slope_ps_per_s=0.0,
                                  **dict(kwargs, refine_radius_ps=PRUNE_PS // 2))
    if not report['detected'] or report['coarse_search']['n_fft'] != 0:
        return None
    # back on B's own clock: the total skew is predicted plus residual, as continuous_qkd.py does it
    sk = report['clock_skew']
    add = s_p * (sk['reference_elapsed_ps'] + sk['lag_at_reference_ps'])
    sk['lag_at_reference_ps'] += add
    sk['delay_b_minus_a_at_reference_ps'] += int(round(add))
    sk['shift_to_add_to_b_at_reference_ps'] = -sk['delay_b_minus_a_at_reference_ps']
    sk['slope_ps_per_s'] += model['slope']
    sk['skew_fraction'] = sk['slope_ps_per_s'] / cp.PS_PER_S
    sk['skew_ppb'] = sk['slope_ps_per_s'] / 1_000.0
    return report, keep_b


def model_of(report: dict, a0: int) -> dict:
    sk = report['clock_skew']
    return {'delay_ref': int(sk['delay_b_minus_a_at_reference_ps']), 't_ref': a0 + int(sk['reference_elapsed_ps']),
            'slope': float(sk['slope_ps_per_s'])}


def align_block(pub_ts: Path, own_ts: Path, model, kwargs: dict):
    '''Aligns Bob's joined block (own_ts) to Alice's announcement (pub_ts) and writes the pair's report next to
    them; Bob's files are rewritten to the clicks that can pair. Returns (report, new model, how)'''
    a = np.load(pub_ts)
    b = np.load(own_ts)
    how = 'previous block\'s clock model'
    found = _hinted(a, b, model, kwargs) if model else None
    if found is None:
        # no model, or it no longer holds: a full search on the head of the block gives one
        how = 'full search'
        head_a = a[a < a[0] + int(HEAD_S * cp.PS_PER_S)]
        head_b = b[b < b[0] + int(HEAD_S * cp.PS_PER_S)]
        head, _ = cp.analyze_arrays(head_a, head_b, strict=False, **kwargs)
        if not head['detected']:
            raise ValueError('no coincidence peak between the announcement and this node\'s block')
        found = _hinted(a, b, model_of(head, int(head_a[0])), kwargs)
        if found is None:
            raise ValueError('the clock model from the head of the block does not hold over the block')
    report, keep_b = found
    ch_path = own_ts.with_name(own_ts.name[:-len(TS)] + CH)
    np.save(ch_path, np.load(ch_path)[keep_b])
    np.save(own_ts, b[keep_b])
    report.update(input_a=pub_ts.name, input_b=own_ts.name, channel_delays=None)
    (own_ts.parent / 'pair_coincidence.json').write_text(json.dumps(report, default=float))
    return report, model_of(report, int(a[0])), how


def drop_old_segments(seg_dir: Path, older_than: float) -> int:
    '''Deletes segment directories that started before older_than (UTC seconds); returns how many'''
    n = 0
    for d in Path(seg_dir).iterdir():
        if d.is_dir() and not d.name.startswith('.'):
            try:
                t = segment_start(d.name)
            except ValueError:
                continue
            if t < older_than:
                shutil.rmtree(d, ignore_errors=True)
                n += 1
    return n
