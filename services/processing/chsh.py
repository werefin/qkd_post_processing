'''
CHSH diagnostic: coincidences per block of a pair in a signal ROI and a noise ROI, and their CSV
'''

import csv
import io

import chsh_diagnostic
from flask import Blueprint, current_app, jsonify, request

from ..core import recordings as rec
from .alignment import prepared_dirs

bp = Blueprint('processing_chsh', __name__)

# a pulsed source needs ROI2 a whole number of repetition periods away; far beyond the peak is safe
ROI2_OFFSET_WINDOW_MULTIPLE = 200
ROI2_OFFSET_FLOOR_NS = 20.0
_last = {'columns': None, 'rows': None, 'filename': None}


def _num(body, key, cast):
    v = body.get(key)
    return None if v in (None, '') else cast(v)


@bp.route('/api/chsh/run', methods=['POST'])
def api_run():
    body = request.get_json(silent=True) or {}
    try:
        pair = rec.resolve_pair(body.get('a'), body.get('b'))
        with prepared_dirs([pair]) as (dirs, reports):
            roi2_offset_ns = _num(body, 'roi2_offset_ns', float)
            auto = roi2_offset_ns is None
            if auto:
                window_ns = reports[0]['clock_skew']['corrected_window']['width_ps'] / 1000
                roi2_offset_ns = max(window_ns * ROI2_OFFSET_WINDOW_MULTIPLE, ROI2_OFFSET_FLOOR_NS)
            metadata, columns, rows = chsh_diagnostic.build_blocks(
                dirs[0], block_s=_num(body, 'block_s', float) or 1.0,
                roi1_width_ns=_num(body, 'roi1_width_ns', float), roi1_center_ns=_num(body, 'roi1_center_ns', float),
                roi2_offset_ns=roi2_offset_ns, roi2_width_ns=_num(body, 'roi2_width_ns', float))
    except Exception as e:
        return jsonify({'error': str(e)[:300]}), 400
    scale = metadata['roi1']['width_ps'] / metadata['roi2']['width_ps'] if metadata['roi2']['width_ps'] else 1.0
    pairs = []
    for name in metadata['pair_order']:
        i1, i2 = columns.index(f'roi1_{name}'), columns.index(f'roi2_{name}')
        r1, r2 = sum(r[i1] for r in rows), sum(r[i2] for r in rows)
        pairs.append({'pair': name, 'roi1_total': r1, 'roi2_total': r2, 'subtracted': round(r1 - r2 * scale, 2)})
    _last.update(columns=columns, rows=rows, filename=f'{rec.base(pair[0].name)}_chsh_blocks.csv')
    return jsonify({'metadata': metadata, 'pairs': pairs, 'a': pair[0].name, 'b': pair[1].name, 'roi2_offset_auto': auto})


@bp.route('/api/chsh/download')
def api_download():
    if _last['columns'] is None:
        return jsonify({'error': 'no CHSH diagnostic run yet'}), 404
    buf = io.StringIO()
    w = csv.writer(buf)
    w.writerow(_last['columns'])
    w.writerows(_last['rows'])
    resp = current_app.response_class(buf.getvalue(), mimetype='text/csv')
    resp.headers['Content-Disposition'] = f'attachment; filename="{_last["filename"]}"'
    return resp
