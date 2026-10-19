'''
Continuous QKD: aligns and sifts each segment of a continuous recording as it lands, and distils every block_s
seconds of data into keys for the KMS; usage: continuous_qkd.py <segments dir> [--block-s 300] [--keystore dir]
'''

import argparse
import json
import math
import os
import shutil
import signal
import subprocess
import sys
import threading
import time
from collections import deque
from pathlib import Path

import numpy as np

import coincidence_peak as cp

STATUS_PREFIX = 'CONT '  # stdout lines starting with this hold one JSON status for the dashboard
# clicks kept around the predicted lag: far wider than its error (well under a ns), the drift filter's 3 ns
# search and importtt's 20 ns prune, so everything downstream sees all it would have seen
PRUNE_PS = 50_000
SEGMENT_DONE = '.recording_done'
STATE_FILE = 'continuous_state.json'
BLOCKS_FILE = 'blocks.jsonl'
HISTORY = 720  # segments kept in the status history, two hours of 10 s segments
METHOD_FLAGS = {'cascade': ['-cascade'], 'ldpc5g': ['-ldpc5g'], 'polar': ['-polar']}
LEAK_FIELD = {'cascade': 'cascade_parities', 'ldpc5g': 'ldpc5g_leaked_bits', 'polar': 'polar_leaked_bits'}


def log(level, msg):
    print(f'[{level}] [continuous] {msg}', flush=True)


def h2(p):
    return 0.0 if p <= 0 or p >= 1 else -p * math.log2(p) - (1 - p) * math.log2(1 - p)


class Processor:
    def __init__(self, seg_dir: Path, importtt: str, cfg: dict, args):
        self.dir, self.importtt, self.args = seg_dir, importtt, args
        self.peak_kwargs = cp.kwargs_from_config(cfg)
        # runs beside the recorder for hours: the first segment's full search must not take the machine's memory
        self.peak_kwargs['max_memory_gib'] = min(self.peak_kwargs['max_memory_gib'], args.max_memory_gib)
        self.state = {'done': [], 'skipped': [], 'block': [], 'blocks': [], 'history': [], 'key_bits': 0,
                      'keys_saved': 0, 'data_s': 0.0, 'started': time.time()}
        state_path = self.dir / STATE_FILE
        if state_path.is_file():  # a restarted processor continues where it stopped
            try:
                self.state.update(json.loads(state_path.read_text()))
            except ValueError:
                pass
        self.state['history'] = deque(self.state['history'], maxlen=HISTORY)
        self.block_thread = None
        self.block_running = None
        self.stop = threading.Event()
        self.lock = threading.Lock()

    # One segment

    def importtt_json(self, dirs, *flags):
        r = subprocess.run([self.importtt, '-dir', ','.join(str(d) for d in dirs), '-json', '-events-cache', *flags],
                           capture_output=True, text=True)
        try:
            out = json.loads(r.stdout)
        except ValueError:
            raise RuntimeError((r.stderr.strip().splitlines() or [f'importtt exit status {r.returncode}'])[-1][:300])
        if out.get('error'):
            raise RuntimeError(out['error'][:300])
        return out

    def fast_align(self, seg, ts, a, b):
        '''The predicted clock model keeps only clicks that can pair, a percent or two on a busy link; aligned on
        those, the segment is rewritten pruned. None if the prediction does not hold, for the full search'''
        align = self.state.get('align')
        if not align:
            # the first segment: the recorder's live tracker may have locked already
            for meta in sorted(seg.glob('*_meta.json')):
                m = json.loads(meta.read_text()).get('live_model')
                if m:
                    align = {'delay_ref': m['delay_b_minus_a_ps'], 't_ref': m['t_ref_ps'], 'slope': m['slope_ps_per_s']}
                    break
        if not align:
            return None
        keep_a, keep_b = cp.prune_to_model(a, b, align['delay_ref'], align['slope'], align['t_ref'], PRUNE_PS)
        pa, pb = a[keep_a], b[keep_b]
        if pa.size < 100 or pb.size < 100:
            return None
        a0, b0 = int(pa[0]), int(pb[0])
        hint = int(round(align['delay_ref'] + align['slope'] * (a0 - align['t_ref']) / cp.PS_PER_S)) - (b0 - a0)
        # the predicted skew moves the peak by hundreds of ns over a segment, more than the kept margin, so it
        # is taken out of B first and only the residual is searched, with sidebands inside the margin
        s_p = align['slope'] / cp.PS_PER_S
        pb_corr = pb - np.round((pb - b0) * s_p).astype(np.int64)
        kw = dict(self.peak_kwargs, refine_radius_ps=PRUNE_PS // 2)
        report, _ = cp.analyze_arrays(pa, pb_corr, strict=False, lag_hint_ps=hint, lag_hint_slope_ps_per_s=0.0, **kw)
        if not report['detected'] or report['coarse_search']['n_fft'] != 0:
            return None
        # back on B's own clock: total skew is predicted plus residual, and the lag at the reference grows by
        # the predicted skew times the time from B's first click
        sk = report['clock_skew']
        add = s_p * (sk['reference_elapsed_ps'] + sk['lag_at_reference_ps'])
        sk['lag_at_reference_ps'] += add
        sk['delay_b_minus_a_at_reference_ps'] += int(round(add))
        sk['shift_to_add_to_b_at_reference_ps'] = -sk['delay_b_minus_a_at_reference_ps']
        sk['slope_ps_per_s'] += align['slope']
        sk['skew_fraction'] = sk['slope_ps_per_s'] / cp.PS_PER_S
        sk['skew_ppb'] = sk['slope_ps_per_s'] / 1_000.0
        if self.args.keep_raw:
            (seg / 'raw').mkdir(exist_ok=True)
        for path, keep, arr in ((ts[0], keep_a, pa), (ts[1], keep_b, pb)):
            ch_path = Path(str(path).replace('timestamp_sequence', 'channel_sequence'))
            ch = np.load(ch_path)
            if self.args.keep_raw:
                for f in (path, ch_path):
                    shutil.copy2(f, seg / 'raw' / f.name)
            np.save(ch_path, ch[keep])
            np.save(path, arr)
        report.update(input_a=str(ts[0]), input_b=str(ts[1]), channel_delays=None,
                      pruned={'radius_ps': PRUNE_PS, 'kept_a': int(pa.size), 'of_a': int(a.size), 'kept_b': int(pb.size), 'of_b': int(b.size)})
        return report

    def process_segment(self, seg: Path):
        t0 = time.time()
        ts = sorted(seg.glob('*_timestamp_sequence.npy'))
        entry = {'name': seg.name}
        if len(ts) != 2:
            raise RuntimeError(f'{seg.name}: expected two recordings, found {len(ts)}')
        a, b = np.load(ts[0]), np.load(ts[1])
        a_end = int(a[-1])
        entry['seconds'] = (a_end - int(a[0])) / cp.PS_PER_S
        report = self.fast_align(seg, ts, a, b)
        entry['fast'] = report is not None
        if report is None:
            # no model yet, or it did not hold: the full search on the raw recording
            hint, align = None, self.state.get('align')
            if align:
                a0, b0 = int(a[0]), int(b[0])
                hint = int(round(align['delay_ref'] + align['slope'] * (a0 - align['t_ref']) / cp.PS_PER_S)) - (b0 - a0)
            del a, b
            report, _ = cp.analyze_files(ts[0], ts[1], strict=False, lag_hint_ps=hint,
                                         lag_hint_slope_ps_per_s=align['slope'] if align else 0.0, **self.peak_kwargs)
        else:
            del a, b
        a0 = int(np.load(ts[0], mmap_mode='r')[0])  # the first click of the files importtt reads, pruned or not
        (seg / (ts[0].name[:-len('_timestamp_sequence.npy')] + '_coincidence.json')).write_text(json.dumps(report, default=float))
        sk, cs = report['clock_skew'], report['constant_shift']
        entry.update(detected=bool(report['detected']), hinted=report['coarse_search']['n_fft'] == 0,
                     lag_ms=cs['relative_lag_ps'] / 1e9, skew_ppb=sk['skew_ppb'], score=sk['corrected_window']['local_score'])
        if report['detected']:
            self.state['align'] = {'delay_ref': int(sk['delay_b_minus_a_at_reference_ps']),
                                   't_ref': a0 + int(sk['reference_elapsed_ps']), 'slope': float(sk['slope_ps_per_s'])}
            # block discard is the fastest method; only the sifting statistics and the events cache are wanted here
            out = self.importtt_json([seg])
            entry.update(sifted=out['sifted_bits'], qber_vis=out['qber_vis'], window_ps=out['window_ps'],
                         sigma_ps=out.get('filter_sigma_ps'), accidentals=out.get('filter_predicted_accidental_fraction'))
        if not self.args.keep_raw:
            for f in seg.glob('*_sequence.npy'):
                f.unlink()
        entry['processing_s'] = time.time() - t0
        return entry

    # Blocks

    def block_ready(self):
        block = self.state['block']
        return block and sum(s['seconds'] for s in block) >= self.args.block_s

    def run_block(self, segments, index):
        dirs = [self.dir / s['name'] for s in segments]
        t0 = time.time()
        flags = list(METHOD_FLAGS[self.args.method])
        if self.args.keystore:
            flags += ['-keystore', self.args.keystore, '-keystore-max', str(self.args.keystore_max)]
        seconds = sum(s['seconds'] for s in segments)
        res = {'index': index, 'segments': len(segments), 'seconds': seconds, 'first': segments[0]['name'],
               'last': segments[-1]['name'], 'method': self.args.method}
        try:
            out = self.importtt_json(dirs, *flags)
            fk = out.get('finite_key') or {}
            n = fk.get('surviving_bits', 0)
            leak = out.get(LEAK_FIELD[self.args.method])
            shannon = n * h2(out['qber'])
            res.update(sifted=out['sifted_bits'], qber=out['qber'], qber_vis=out['qber_vis'], ec_leak=leak,
                       f=(leak / shannon) if leak and shannon else None, phase_error_bound=fk.get('phase_error_bound'),
                       key_bits=out['final_key_bits'], keys_saved=out.get('keys_saved', 0),
                       ok=out['chunks_total'] > 0 and out['chunks_kept'] == out['chunks_total'])
        except RuntimeError as e:
            res.update(error=str(e), key_bits=0, keys_saved=0, ok=False)
        res['processing_s'] = time.time() - t0
        res['finished'] = time.time()
        with self.lock:
            self.state['blocks'].append(res)
            self.state['key_bits'] += res['key_bits']
            self.state['keys_saved'] += res['keys_saved']
            self.block_running = None
            with open(self.dir / BLOCKS_FILE, 'a') as f:
                f.write(json.dumps(res) + '\n')
        if not self.args.keep_segments:
            for d in dirs:
                shutil.rmtree(d, ignore_errors=True)
        log('INFO', f'block {index}: {seconds:.0f} s, {res.get("sifted", 0)} sifted bits, QBER {100 * res.get("qber", 0):.2f}%, '
                    f'{res["key_bits"]} key bits' + (f', failed: {res["error"]}' if res.get('error') else ''))
        self.save()
        self.emit()

    def start_block(self, final=False):
        '''Hands the block to a background thread, so segments keep being processed while it distills'''
        if self.block_thread and self.block_thread.is_alive():
            if not final:
                return
            self.block_thread.join()
        with self.lock:
            segments, self.state['block'] = self.state['block'], []
            index = len(self.state['blocks']) + 1
            self.block_running = {'index': index, 'segments': len(segments), 'seconds': sum(s['seconds'] for s in segments)}
        self.block_thread = threading.Thread(target=self.run_block, args=(segments, index), daemon=True)
        self.block_thread.start()

    # State and status

    def save(self):
        with self.lock:
            snap = dict(self.state, history=list(self.state['history']))
        tmp = self.dir / (STATE_FILE + '.tmp')
        tmp.write_text(json.dumps(snap, default=float))
        os.replace(tmp, self.dir / STATE_FILE)

    def pending(self):
        known = set(self.state['done']) | set(self.state['skipped'])
        return sorted(p for p in self.dir.iterdir() if p.is_dir() and not p.name.startswith('.') and p.name not in known)

    def status(self, state):
        with self.lock:
            block = self.state['block']
            seconds = sum(s['seconds'] for s in block)
            sifted = sum(s.get('sifted', 0) for s in block)
            errors = sum(s.get('sifted', 0) * s.get('qber_vis', 0) for s in block)
            blocks = self.state['blocks'][-50:]
            data_s = self.state['data_s']
            hist = list(self.state['history'])
            return {
                'state': state, 'uptime_s': time.time() - self.state['started'], 'segments_done': len(self.state['done']),
                'segments_skipped': len(self.state['skipped']), 'backlog': len(self.pending()), 'data_s': data_s,
                'block_s': self.args.block_s, 'method': self.args.method, 'keystore': bool(self.args.keystore),
                'block': {'index': len(self.state['blocks']) + 1 + (1 if self.block_running else 0), 'seconds': seconds,
                          'segments': len(block), 'sifted': sifted, 'qber_vis': (errors / sifted) if sifted else None},
                'distilling': self.block_running, 'blocks': blocks,
                'key_bits': self.state['key_bits'], 'keys_saved': self.state['keys_saved'],
                'key_rate': self.state['key_bits'] / data_s if data_s else 0.0,
                'last': hist[-1] if hist else None,
                'history': {k: [h.get(k) for h in hist] for k in ('name', 'seconds', 'sifted', 'qber_vis', 'skew_ppb', 'lag_ms', 'window_ps', 'processing_s')},
            }

    def emit(self, state='running'):
        print(STATUS_PREFIX + json.dumps(self.status(state), separators=(',', ':'), default=float), flush=True)

    # Main loop

    def run(self):
        log('INFO', f'following {self.dir}: blocks of {self.args.block_s:g} s, {self.args.method}, '
                    + (f'keys to {self.args.keystore}' if self.args.keystore else 'keys counted, not stored'))
        idle_emit = 0.0
        while not self.stop.is_set():
            todo = self.pending()
            if not todo:
                if self.block_ready():
                    self.start_block()
                finished = (self.dir / SEGMENT_DONE).is_file()
                if finished or (self.args.once and not todo):
                    break
                if time.time() - idle_emit > 5:
                    self.emit('waiting')
                    idle_emit = time.time()
                self.stop.wait(0.5)
                continue
            seg = todo[0]
            try:
                entry = self.process_segment(seg)
            except Exception as e:  # one bad segment must not stop the stream
                log('WARN', f'{seg.name}: {type(e).__name__}: {e}')
                entry = {'name': seg.name, 'error': str(e)[:200], 'detected': False, 'seconds': 0.0}
            with self.lock:
                if entry.get('detected') and entry.get('sifted') is not None:
                    self.state['done'].append(seg.name)
                    self.state['block'].append(entry)
                    self.state['data_s'] += entry['seconds']
                else:
                    self.state['skipped'].append(seg.name)
                    if not self.args.keep_segments:
                        shutil.rmtree(seg, ignore_errors=True)
                self.state['history'].append(entry)
            log('INFO', f'{seg.name}: ' + (f'{entry.get("sifted")} sifted bits, QBER {100 * entry.get("qber_vis", 0):.2f}%, '
                                          f'window ±{entry.get("window_ps")} ps, skew {entry.get("skew_ppb", 0):.2f} ppb, '
                                          f'{entry["processing_s"]:.1f} s to process' if entry.get('sifted') is not None
                                          else 'no coincidence peak, skipped'))
            if self.block_ready():
                self.start_block()
            self.save()
            self.emit()
        # the recording ended: whatever remains forms a last, shorter block
        if self.state['block'] and not self.stop.is_set():
            self.start_block(final=True)
        if self.block_thread:
            self.block_thread.join()
        self.save()
        self.emit('finished')
        log('INFO', f'finished: {len(self.state["blocks"])} block(s), {self.state["key_bits"]} key bits from {self.state["data_s"]:.0f} s of data')


def main(argv=None):
    ap = argparse.ArgumentParser(description='Continuous QKD over the segments of a continuous recording')
    ap.add_argument('segments', type=Path, help='the output directory of tt_record_dual.py --stop-mode continuous')
    ap.add_argument('--importtt', default=os.environ.get('IMPORTTT_BIN', 'importtt'), help='importtt binary')
    ap.add_argument('--config', type=Path, default=Path(__file__).with_name('config.json'), help='coincidence_peak settings')
    ap.add_argument('--block-s', type=float, default=300.0, help='seconds of data per privacy-amplification block, default 300')
    ap.add_argument('--method', choices=sorted(METHOD_FLAGS), default='cascade', help='error correction, default cascade')
    ap.add_argument('--keystore', help='directory the keys are written to, the KMS import directory of the link')
    ap.add_argument('--keystore-max', type=int, default=30000)
    ap.add_argument('--max-memory-gib', type=float, default=1.5, help='cap on the peak search\'s working memory, default 1.5')
    ap.add_argument('--keep-raw', action='store_true', help='keep each segment\'s raw arrays after processing')
    ap.add_argument('--keep-segments', action='store_true', help='keep segment directories after their block is distilled')
    ap.add_argument('--once', action='store_true', help='process what is there, distil the rest as a last block, and exit')
    args = ap.parse_args(argv)
    cfg = json.loads(args.config.read_text()) if args.config.is_file() else {}
    args.segments.mkdir(parents=True, exist_ok=True)
    proc = Processor(args.segments, args.importtt, cfg, args)
    # a stop finishes the segment in hand and the block distilling, then saves the state
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda *_: proc.stop.set())
    proc.run()
    return 0


if __name__ == '__main__':
    sys.exit(main())
