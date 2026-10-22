'''
Continuous two-party QKD, the parts that work on whole segments: segments chosen by their UTC start (each node
records its own TimeTagger in segments that start on the same UTC seconds at both sites), Bob's clicks aligned to
Alice's announcement from the previous clock model or a full search, and old segments dropped; the streaming
itself is two_party_stream.py
'''

import shutil
from datetime import datetime, timezone
from pathlib import Path

import numpy as np

import coincidence_peak as cp

PRUNE_PS = 50_000      # Bob's clicks kept around a predicted partner, as in continuous_qkd.py
HEAD_S = 30.0          # seconds searched in full when no clock model holds yet
TS, CH, META = '_timestamp_sequence.npy', '_channel_sequence.npy', '_meta.json'


def segment_start(name: str) -> float:
    '''A segment directory's UTC start, from its name'''
    return datetime.strptime(name, '%Y%m%d_%H%M%S').replace(tzinfo=timezone.utc).timestamp()


def segments_in(seg_dir: Path, start: float, end: float) -> list:
    '''Complete segment directories starting in [start, end), oldest first'''
    out = []
    if not Path(seg_dir).is_dir():
        return out  # the recorder has not written its first segment yet
    for d in Path(seg_dir).iterdir():
        if d.is_dir() and not d.name.startswith('.'):
            try:
                t = segment_start(d.name)
            except ValueError:
                continue
            if start <= t < end:
                out.append((t, d))
    return [d for _, d in sorted(out)]


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


def align_arrays(a, b, model, kwargs: dict):
    '''Aligns Bob's clicks (b) to Alice's announced ones (a), from the clock model when it holds, else from a full
    search on the head; returns (report, which of b can pair, new model, how). The report maps b[keep][0] onto a[0]'''
    how = 'previous clock model'
    found = _hinted(a, b, model, kwargs) if model else None
    if found is None:
        how = 'full search'
        head_a = a[a < a[0] + int(HEAD_S * cp.PS_PER_S)]
        head_b = b[b < b[0] + int(HEAD_S * cp.PS_PER_S)]
        head, _ = cp.analyze_arrays(head_a, head_b, strict=False, **kwargs)
        if not head['detected']:
            raise ValueError('no coincidence peak between the announcement and this node\'s clicks')
        found = _hinted(a, b, model_of(head, int(head_a[0])), kwargs)
        if found is None:
            raise ValueError('the clock model from the full search does not hold over the segment')
    report, keep_b = found
    return report, keep_b, model_of(report, int(a[0])), how


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
