'''
Dual TimeTagger recording, records from two TimeTaggers at once into per-TT npy files
Configure via config.json (see that file and README.md), run --help for every override
'''

import os
import json
import time
import argparse
import queue
import threading
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from typing import Optional

import numpy as np

import site_links
from tt_stream_external import TimeTaggerStream


DEFAULT_CONFIG_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), 'config.json')

# a site entry's connection-vs-settings split; see config.json
_CONNECTION_KEYS = ('tt_ip', 'tt_port', 'auth_key_hex')
_SETTINGS_KEYS = ('tt_serial', 'channels', 'delays_ps', 'deadtimes_ps', 'triggers_volt', 'loop_period_s')


def load_config(config_path: str) -> dict:
    '''Loads and validates config.json: every known site, the links (every pair of sites is one),
    and the recording-mode defaults, raising SystemExit on anything wrong'''
    try:
        with open(config_path) as f:
            cfg = json.load(f)
    except (OSError, ValueError) as ex:
        raise SystemExit(f'{config_path}: {ex}')

    rec = cfg.get('record_dual')
    if not isinstance(rec, dict):
        raise SystemExit(f'{config_path}: missing "record_dual" section')
    err = site_links.check(cfg)
    if err:
        raise SystemExit(f'{config_path}: {err}')
    if rec.get('stop_mode') not in ('duration', 'detection_count', 'continuous'):
        raise SystemExit(f'{config_path}: record_dual.stop_mode must be "duration", "detection_count" or "continuous"')

    return cfg


def site_connection_and_settings(site: dict) -> tuple:
    '''Splits one config.json sites entry into the connection_params and
    tt_settings dicts TimeTaggerStream expects'''
    connection = {k: site[k] for k in _CONNECTION_KEYS}
    settings = {k: site[k] for k in _SETTINGS_KEYS}
    return connection, settings


def resolve_link(cfg: dict, config_path: str, link_name: Optional[str],
                  site_a: Optional[str], site_b: Optional[str]) -> tuple:
    '''Picks which two sites to record from, --site-a/--site-b takes
    precedence over --link, which takes precedence over config.json'''
    sites = cfg['sites']

    if site_a or site_b:
        if not (site_a and site_b):
            raise SystemExit('--site-a and --site-b must be given together')
        for flag, name in (('--site-a', site_a), ('--site-b', site_b)):
            if name not in sites:
                raise SystemExit(f'{flag} {name!r} not found in "sites" ({config_path}); '
                                  f'known sites: {", ".join(sorted(sites))}')
        return site_a, site_b, f'{site_a}+{site_b} (ad hoc pair)'

    name = link_name or cfg['record_dual'].get('link')
    if not name:
        raise SystemExit(f'no link to record: give --link, e.g. A_D, or set record_dual.link ({config_path})')
    try:
        a, b = site_links.link_sites(cfg, name)
    except ValueError as e:
        raise SystemExit(f'--link: {e} ({config_path})')
    return a, b, name


def parse_args(argv: Optional[list] = None) -> argparse.Namespace:
    '''CLI overrides for config.json's record_dual section, so any link or
    ad hoc site pair can be recorded without editing the file'''
    parser = argparse.ArgumentParser(description='Record from two TimeTaggers simultaneously.')
    parser.add_argument('config', nargs='?', default=DEFAULT_CONFIG_PATH,
                         help='path to config.json (default: %(default)s)')
    parser.add_argument('--link', metavar='NAME',
                         help='override record_dual.link; any two sites as X_Y (e.g. A_D, C_B) or a name under "links"')
    parser.add_argument('--site-a', metavar='NAME',
                         help='record an ad hoc pair instead of a named link: any name under "sites". '
                         'Requires --site-b too; takes precedence over --link.')
    parser.add_argument('--site-b', metavar='NAME',
                         help='see --site-a; any name under "sites"')
    parser.add_argument('--stop-mode', choices=('duration', 'detection_count', 'continuous'),
                         help='override record_dual.stop_mode; continuous records until stopped, in segments')
    parser.add_argument('--segment-s', type=float, metavar='SECONDS',
                         help='continuous mode: length of each segment written, override record_dual.segment_s (default 10)')
    parser.add_argument('--site', metavar='NAME',
                         help='record this one site\'s TimeTagger only, as each node of a two-party link does: '
                         'with --stop-mode continuous, segments fall on UTC multiples of --segment-s and are named by their UTC start')
    parser.add_argument('--replay', nargs='+', metavar='NPY',
                         help='play recorded *_timestamp_sequence.npy files back at stream speed, looping, instead of '
                         'connecting to TimeTaggers: two for a link, one with --site; for testing and demonstrations')
    parser.add_argument('--replay-speed', type=float, default=1.0, metavar='X', help='replay speed, default 1')
    parser.add_argument('--duration', type=float, metavar='SECONDS',
                         help='override record_dual.record_duration_s')
    parser.add_argument('--target-count', type=int, metavar='N',
                         help='override record_dual.target_detection_count')
    parser.add_argument('--output-dir', metavar='DIR', help='override record_dual.output_dir')
    live_group = parser.add_mutually_exclusive_group()
    live_group.add_argument('--live', dest='live', action='store_true', default=None,
                             help='override live_coincidence.enabled: lock onto and track the coincidence peak while recording')
    live_group.add_argument('--no-live', dest='live', action='store_false',
                             help='override live_coincidence.enabled: disable live tracking')
    parser.add_argument('--live-interval-s', type=float, metavar='SECONDS',
                         help='override live_coincidence.interval_s')
    parser.add_argument('--live-window-s', type=float, metavar='SECONDS',
                         help='override live_coincidence.window_s')
    return parser.parse_args(argv)


_PRINT_LOCK = threading.Lock()


def safe_print(msg: str) -> None:
    '''Thread-safe stdout print, so per-TT log lines don't interleave'''
    with _PRINT_LOCK:
        print(msg, flush=True)


def log(level: str, tag: Optional[str], msg: str) -> None:
    '''arnika-style log line: "[LEVEL] [tag] message", one severity tag per
    line and the reason spelled out in the message itself, not a bare symbol'''
    prefix = f'[{tag}] ' if tag else ''
    safe_print(f'[{level}] {prefix}{msg}')


@dataclass
class TTContext:
    '''Per-TimeTagger runtime state shared between main and its reader thread'''
    cfg: dict
    stream: TimeTaggerStream
    channels: list = field(default_factory=list)     # list[np.ndarray], recent tail only
    timestamps: list = field(default_factory=list)    # list[np.ndarray], recent tail only
    frames_meta: list = field(default_factory=list)   # list[dict], kept in full (small: one dict per frame, not per detection)
    lock: threading.Lock = field(default_factory=threading.Lock)  # guards channels/timestamps appends, for the live monitor's snapshots
    stream_started: bool = False
    error: Optional[str] = None
    wall_start_s: Optional[float] = None   # time.monotonic() right after start_stream()
    wall_end_s: Optional[float] = None     # time.monotonic() right after the loop exits
    scratch_ch_path: Optional[str] = None  # set on first flush, see _flush_overflow/write_outputs
    scratch_ts_path: Optional[str] = None
    live_queue: Optional[queue.Queue] = None  # every new frame's timestamps, for the live tracker
    rates: bool = False  # one RATES line per read with its counts per channel, for a dashboard's live singles
    segmenting: bool = False  # continuous mode: the segment writer drains the buffers, no overflow spill

    @property
    def tag(self) -> str:
        '''Short identifier for log lines and thread names, e.g. 192.0.2.10:40404'''
        return f'{self.cfg["tt_ip"]}:{self.cfg["tt_port"]}'


def build_stream(cfg: dict, settings: dict) -> TimeTaggerStream:
    '''Constructs a TimeTaggerStream and pushes the connection and settings dicts'''
    tts = TimeTaggerStream()
    tts.update_tt_connection_params({
        'tt_ip': cfg['tt_ip'],
        'tt_port': cfg['tt_port'],
        'auth_key_hex': cfg['auth_key_hex'],
    })
    tts.update_tt_settings(settings)
    return tts


def make_file_basename(ip: str, port: int, started_at: datetime) -> str:
    '''e.g. 20260605_153012_192_0_2_10_p40404, dots in the IP become underscores'''
    ip_safe = ip.replace('.', '_')
    ts = started_at.strftime('%Y%m%d_%H%M%S')
    return f'{ts}_{ip_safe}_p{port}'


# Without this, ctx.channels/ctx.timestamps buffer the WHOLE recording in RAM (never
# freed until the final write), filling memory in minutes on a high-count-rate detector
_FLUSH_KEEP_EVENTS = 2_000_000


def _scratch_paths(ctx: 'TTContext', base: str, outdir: str) -> tuple:
    if ctx.scratch_ch_path is None:
        tag_safe = ctx.tag.replace(':', '_').replace('.', '_')
        ctx.scratch_ch_path = os.path.join(outdir, f'.{base}_{tag_safe}.ch.scratch')
        ctx.scratch_ts_path = os.path.join(outdir, f'.{base}_{tag_safe}.ts.scratch')
    return ctx.scratch_ch_path, ctx.scratch_ts_path


def _flush_overflow(ctx: 'TTContext', base: str, outdir: str) -> None:
    '''Spills oldest frames to a per-TT scratch file once more than _FLUSH_KEEP_EVENTS
    detections are buffered, always leaving the newest frame in RAM. Caller must hold ctx.lock'''
    total = sum(a.size for a in ctx.timestamps)
    if total <= _FLUSH_KEEP_EVENTS:
        return
    flush_ch, flush_ts = [], []
    while len(ctx.timestamps) > 1 and total - ctx.timestamps[0].size >= _FLUSH_KEEP_EVENTS:
        flush_ch.append(ctx.channels.pop(0))
        popped = ctx.timestamps.pop(0)
        total -= popped.size
        flush_ts.append(popped)
    if not flush_ts:
        return
    ch_path, ts_path = _scratch_paths(ctx, base, outdir)
    with open(ch_path, 'ab') as f:
        for a in flush_ch:
            f.write(a.tobytes())
    with open(ts_path, 'ab') as f:
        for a in flush_ts:
            f.write(a.tobytes())


def _drain(q: queue.Queue) -> list:
    parts = []
    while True:
        try:
            parts.append(q.get_nowait())
        except queue.Empty:
            return parts


_LIVE_MODEL: dict = {}  # the live tracker's latest clock model, written into each continuous segment


def live_coincidence_thread(ctx_a: TTContext, ctx_b: TTContext, stop_event: threading.Event,
                             interval_s: float, tracker_kwargs: dict) -> None:
    '''Feeds every new frame to live_peak.LiveTracker, which locks onto the peak and then follows its lag and
    skew; one LIVE status line per update for the dashboard, a readable line every interval_s'''
    from live_peak import LiveTracker, emit, summary

    tracker = LiveTracker(**tracker_kwargs)
    seen, last_log = 0, 0.0
    while not stop_event.wait(0.5):
        tracker.feed(_drain(ctx_a.live_queue), _drain(ctx_b.live_queue))
        try:
            tracker.step()
        except Exception as exc:
            log('WARN', 'live', f'live tracker update failed, will retry with the next data: {exc}')
            continue
        status = tracker.status()
        _LIVE_MODEL.clear()
        if status.get('model'):
            _LIVE_MODEL.update(status['model'])
        emit(status)
        for event in tracker.events[seen:]:
            log('INFO', 'live', event)
        seen = len(tracker.events)
        if time.monotonic() - last_log >= interval_s:
            last_log = time.monotonic()
            log('INFO', 'live', summary(status))


def reader_thread(ctx: TTContext,
                   start_barrier: threading.Barrier,
                   stop_event: threading.Event,
                   base: str,
                   outdir: str,
                   target_detections: Optional[int] = None,
                   per_call_cap: int = 4_000_000) -> None:
    '''Waits at the barrier so streams across TimeTaggers start in lockstep, then loops
    read_stream(); base/outdir let a long run spill old frames to scratch, see _flush_overflow'''
    try:
        start_barrier.wait(timeout=10.0)
    except threading.BrokenBarrierError:
        ctx.error = 'barrier_broken'
        log('ERROR', ctx.tag, '[STOP] the other TimeTagger never became ready within 10s, so this one never started; nothing was recorded for this run')
        return

    if not ctx.stream.start_stream():
        ctx.error = 'start_stream_failed'
        log('ERROR', ctx.tag, '[STOP] the TimeTagger rejected the start-measurement request; check it is reachable and not already exclusively held by another client')
        return

    ctx.stream_started = True
    ctx.wall_start_s = time.monotonic()
    log('INFO', ctx.tag, '[OK] measurement started, now streaming detections')

    total_detections = 0
    reached_target = False

    def ingest(frame: dict, total: int) -> int:
        # int8 covers physical channels 1...4 (and any negative falling-edge tags);
        # int64 holds picosecond timestamps, which can exceed 2^32 for long runs
        ch = np.asarray(frame['channel_sequence'], dtype=np.int8)
        ts = np.asarray(frame['timestamp_sequence'], dtype=np.int64)

        # detection_count mode: trim this frame if it would push us past the target
        trimmed = False
        if target_detections is not None and total + ch.size >= target_detections:
            keep = target_detections - total
            ch = ch[:keep]
            ts = ts[:keep]
            trimmed = True

        with ctx.lock:
            ctx.channels.append(ch)
            ctx.timestamps.append(ts)
            if not ctx.segmenting:
                _flush_overflow(ctx, base, outdir)
        if ctx.live_queue is not None and ts.size:
            ctx.live_queue.put(ts)
        total += ch.size

        meta = {
            'unixtime_readout': frame['unixtime_readout'],
            'stream_uptime_s': frame['stream_uptime_s'],
            'frame_start_ps': frame['frame_start_ps'],
            # frame_end_ps from the TT is when the read returned, not when the last detection landed;
            # if we trimmed, the last kept detection's own timestamp is the tighter, correct bound
            'frame_end_ps': int(ts[-1]) if (trimmed and ts.size > 0) else frame['frame_end_ps'],
            'n_detections': int(ch.size),
            'capped': bool(ch.size >= per_call_cap),
            'trimmed': trimmed,
        }
        ctx.frames_meta.append(meta)
        if ctx.rates:
            prev = ctx.frames_meta[-2]['stream_uptime_s'] if len(ctx.frames_meta) > 1 else 0.0
            per = np.bincount(np.clip(ch.astype(np.int64), 0, 8), minlength=9)[1:5]
            safe_print('RATES ' + json.dumps({'t': meta['unixtime_readout'], 'dt_s': round(meta['stream_uptime_s'] - prev, 4),
                                              'counts': [int(x) for x in per]}))

        log('INFO', ctx.tag,
            f'read {meta["n_detections"]} detections this cycle (running total {total}), '
            f'device uptime {meta["stream_uptime_s"]:.2f}s')
        if meta['capped']:
            # the server never returns more than per_call_cap in one read; excess beyond that is
            # silently dropped, and hitting the cap exactly is the only signal we get of it
            log('WARN', ctx.tag,
                f'this read came back at the server\'s maximum batch size ({per_call_cap} detections); '
                f'detections may be arriving faster than single reads can drain them, so some from this '
                f'interval could be missing; read more often (shorter loop_period_s) or expect this at high count rates')
        if meta['trimmed']:
            log('INFO', ctx.tag, 'stopped partway through this batch, having reached the requested detection-count target exactly')
        return total

    while not stop_event.is_set() and ctx.stream.is_connected():
        frame = ctx.stream.read_stream()
        if frame is None:
            ctx.error = 'read_stream_failed'
            log('ERROR', ctx.tag, '[STOP] the TimeTagger stopped responding to reads; this recording will be discarded')
            break

        total_detections = ingest(frame, total_detections)

        if target_detections is not None and total_detections >= target_detections:
            reached_target = True
            log('INFO', ctx.tag, f'[OK] reached the requested target of {target_detections} detections, stopping')
            break

    # stop_event is checked BEFORE each read, so one more frame can be buffered server-side;
    # one drain picks it up. NOT a loop: a high-rate TT streams continuously, so it never ends
    if ctx.error is None and not reached_target and stop_event.is_set() and ctx.stream.is_connected():
        frame = ctx.stream.read_stream()
        if frame is not None:
            total_detections = ingest(frame, total_detections)
        log('INFO', ctx.tag, f'[OK] final read after stop, recording complete: {total_detections} detections total')

    # No stop request and no target reached leaves only a dropped connection,
    # flagged here so main discards the whole recording instead of saving a short file
    if ctx.error is None and not reached_target and not stop_event.is_set():
        ctx.error = 'disconnected'
        log('ERROR', ctx.tag, '[STOP] connection dropped unexpectedly, before the requested stop was ever sent; this recording will be discarded')

    try:
        ctx.stream.disconnect()
    except Exception:
        pass

    ctx.wall_end_s = time.monotonic()


SEGMENT_DONE = '.recording_done'  # continuous mode: written last, so a processor knows no segment follows


def _cut_segment(ctxs: list, seg_start: datetime, index: int, segment_s: float, outdir: str, frames_done: dict) -> Optional[str]:
    '''Takes what every TimeTagger buffered since the last cut and writes it as one recording pair in its own
    directory, named by the segment's start, built under a hidden name and renamed so readers see it whole'''
    taken = []
    for ctx in ctxs:
        with ctx.lock:
            chs, tss = ctx.channels, ctx.timestamps
            ctx.channels, ctx.timestamps = [], []
            frames = ctx.frames_meta[frames_done.get(ctx.tag, 0):]
            frames_done[ctx.tag] = len(ctx.frames_meta)
        taken.append((ctx, chs, tss, frames))
    if not any(sum(t.size for t in tss) for _, _, tss, _ in taken):
        return None
    name = seg_start.strftime('%Y%m%d_%H%M%S')
    tmp, final = os.path.join(outdir, f'.{name}.tmp'), os.path.join(outdir, name)
    os.makedirs(tmp, exist_ok=True)
    for ctx, chs, tss, frames in taken:
        base = make_file_basename(ctx.cfg['tt_ip'], ctx.cfg['tt_port'], seg_start)
        ch = np.concatenate(chs) if chs else np.empty(0, np.int8)
        ts = np.concatenate(tss) if tss else np.empty(0, np.int64)
        np.save(os.path.join(tmp, f'{base}_channel_sequence.npy'), ch)
        np.save(os.path.join(tmp, f'{base}_timestamp_sequence.npy'), ts)
        with open(os.path.join(tmp, f'{base}_meta.json'), 'w') as f:
            json.dump({'tt_ip': ctx.cfg['tt_ip'], 'tt_port': ctx.cfg['tt_port'], 'segment': index, 'segment_s': segment_s,
                       'n_frames': len(frames), 'n_detections': int(ch.size),
                       'n_capped_frames': sum(1 for m in frames if m.get('capped')), 'frames': frames,
                       'segment_start_unix': seg_start.timestamp(), 'live_model': dict(_LIVE_MODEL) or None}, f)
    os.rename(tmp, final)
    return name


def segment_thread(ctxs: list, stop_event: threading.Event, started_at: datetime, segment_s: float, outdir: str,
                   readers: list, aligned: bool = False) -> None:
    '''Continuous mode: cuts a segment every segment_s seconds of host time until stopped, then, once the
    readers have made their last read, the final one and the marker that no segment follows. Aligned, the cuts
    fall on UTC multiples of segment_s and segments are named by their UTC start, so two sites recording on
    NTP-synced clocks name the same stretch of time alike'''
    frames_done: dict = {}
    if aligned:
        index = int(time.time() // segment_s)  # the segment in progress
        start_of = lambda i: datetime.fromtimestamp(i * segment_s, timezone.utc)
        due = lambda i: (i + 1) * segment_s - time.time()
    else:
        index, t0 = 0, time.monotonic()
        start_of = lambda i: started_at + timedelta(seconds=i * segment_s)
        due = lambda i: t0 + (i + 1) * segment_s - time.monotonic()
    while not stop_event.wait(max(0.0, due(index))):
        name = _cut_segment(ctxs, start_of(index), index, segment_s, outdir, frames_done)
        if name:
            log('INFO', 'segment', f'[OK] wrote segment {name}')
        index += 1
        if any(c.error for c in ctxs):
            stop_event.set()
    for t in readers:
        t.join(timeout=35.0)
    name = _cut_segment(ctxs, start_of(index), index, segment_s, outdir, frames_done)
    if name:
        log('INFO', 'segment', f'[OK] wrote the last segment {name}')
    open(os.path.join(outdir, SEGMENT_DONE), 'w').close()


def ctx_failure_reason(ctx: TTContext) -> Optional[str]:
    '''Why this TimeTagger disqualifies the recording, or None if healthy'''
    if ctx.error is not None:
        return ctx.error
    if not ctx.stream_started:
        return 'stream_not_started'
    if not ctx.frames_meta:
        return 'no_frames'
    if ctx.wall_start_s is None or ctx.wall_end_s is None:
        return 'incomplete_wall_clock'
    return None


def _read_and_clear_scratch(path: Optional[str], dtype) -> np.ndarray:
    '''Reads back everything _flush_overflow spilled to disk, then removes the scratch
    file; its content now lives only in the final .npy'''
    if path is None or not os.path.exists(path):
        return np.array([], dtype=dtype)
    arr = np.fromfile(path, dtype=dtype)
    try:
        os.remove(path)
    except OSError:
        pass
    return arr


def write_outputs(ctx: TTContext, base: str, outdir: str) -> None:
    '''Concatenates the buffered arrays (scratch-file overflow first, then
    whatever's still in RAM, chronological order) and writes the .npy + .json sidecar'''
    if not ctx.frames_meta:
        log('WARN', ctx.tag, 'no detections were captured during this recording, skipping the write')
        return

    scratch_ch = _read_and_clear_scratch(ctx.scratch_ch_path, np.int8)
    scratch_ts = _read_and_clear_scratch(ctx.scratch_ts_path, np.int64)
    ch = np.concatenate([scratch_ch, *ctx.channels]) if (scratch_ch.size or ctx.channels) else np.array([], dtype=np.int8)
    ts = np.concatenate([scratch_ts, *ctx.timestamps]) if (scratch_ts.size or ctx.timestamps) else np.array([], dtype=np.int64)

    ch_path = os.path.join(outdir, f'{base}_channel_sequence.npy')
    ts_path = os.path.join(outdir, f'{base}_timestamp_sequence.npy')
    meta_path = os.path.join(outdir, f'{base}_meta.json')

    np.save(ch_path, ch)
    np.save(ts_path, ts)

    sidecar = {
        'tt_ip': ctx.cfg['tt_ip'],
        'tt_port': ctx.cfg['tt_port'],
        'n_frames': len(ctx.frames_meta),
        'n_detections': int(ch.size),
        'n_capped_frames': sum(1 for m in ctx.frames_meta if m.get('capped')),
        'wall_duration_s': (ctx.wall_end_s - ctx.wall_start_s)
                            if (ctx.wall_start_s is not None and ctx.wall_end_s is not None)
                            else None,
        'tt_last_uptime_s': ctx.frames_meta[-1]['stream_uptime_s'] if ctx.frames_meta else None,
        'frames': ctx.frames_meta,
    }
    with open(meta_path, 'w') as f:
        json.dump(sidecar, f, indent=2)

    log('INFO', ctx.tag, f'[OK] saved {ch.size} detections across {len(ctx.frames_meta)} reads to {base}_*')


def main(argv: Optional[list] = None) -> int:
    args = parse_args(argv)
    try:
        cfg = load_config(args.config)
        rec = cfg['record_dual']
        sites = cfg['sites']
        if args.site:
            if args.site not in sites:
                raise SystemExit(f'--site {args.site!r} not found in "sites" ({args.config})')
            site_a_name, site_b_name, link_label = args.site, None, f'{args.site} (one site)'
        else:
            site_a_name, site_b_name, link_label = resolve_link(
                cfg, args.config, args.link, args.site_a, args.site_b)
        if args.replay and len(args.replay) != (1 if args.site else 2):
            raise SystemExit('--replay takes two recordings, or one with --site')
    except SystemExit as ex:
        log('ERROR', None, str(ex))
        return 2

    stop_mode = args.stop_mode or rec['stop_mode']
    record_duration_s = rec['record_duration_s'] if args.duration is None else args.duration
    target_detection_count = rec['target_detection_count'] if args.target_count is None else args.target_count
    output_dir = args.output_dir or rec['output_dir']
    per_call_cap = rec.get('per_call_detection_cap', 4_000_000)

    segment_s = float(args.segment_s or rec.get('segment_s', 10.0))
    if stop_mode == 'continuous' and segment_s < 1:
        log('ERROR', None, 'segments must be at least 1 s long')
        return 2

    started_at = datetime.now()
    stop_event = threading.Event()

    if args.replay:
        # stand-ins named after the recordings, playing them back looped with one common period
        from tt_replay import ReplayStream, pair_skew, replay_identity
        files = sorted(args.replay)  # A first, as the recordings sort
        spans = [(lambda t: (int(t[-1]) - int(t[0])) / 1e12)(np.load(p, mmap_mode='r')) for p in files]
        period = max(spans) + 1.0
        skew = pair_skew(files[0], files[1]) if len(files) == 2 else 0.0
        contexts = [TTContext(cfg=replay_identity(p), stream=ReplayStream(p, period, speed=args.replay_speed, rate=rate))
                    for p, rate in zip(files, (1.0, 1.0 + skew))]
        log('INFO', None, f'[CONF] replaying {len(args.replay)} recordings, looping every {period:.1f}s, instead of TimeTaggers')
    elif site_b_name is None:
        log('INFO', None, f'[CONF] one site: {site_a_name}')
        conn, settings = site_connection_and_settings(sites[site_a_name])
        contexts = [TTContext(cfg=conn, stream=build_stream(conn, settings))]
    else:
        log('INFO', None, f'[CONF] link "{link_label}": {site_a_name} + {site_b_name}')
        tt_a_conn, tt_a_settings = site_connection_and_settings(sites[site_a_name])
        tt_b_conn, tt_b_settings = site_connection_and_settings(sites[site_b_name])
        contexts = [
            TTContext(cfg=tt_a_conn, stream=build_stream(tt_a_conn, tt_a_settings)),
            TTContext(cfg=tt_b_conn, stream=build_stream(tt_b_conn, tt_b_settings)),
        ]

    # connect each TimeTagger sequentially; collect the survivors
    live = []
    for ctx in contexts:
        log('INFO', ctx.tag, '[REQ] connecting...')
        if ctx.stream.connect():
            live.append(ctx)
            log('INFO', ctx.tag, '[OK] connected')
        else:
            ctx.error = 'connect_failed'
            log('ERROR', ctx.tag, '[STOP] could not connect, skipping this TimeTagger')

    if len(live) < len(contexts):
        failed = [ctx.tag for ctx in contexts if ctx.error == 'connect_failed']
        log('ERROR', None,
            f'[STOP] both TimeTaggers are required; only {len(live)}/{len(contexts)} reachable '
            f'({", ".join(failed)} failed), aborting with nothing recorded')
        for ctx in live:
            try:
                ctx.stream.disconnect()
            except Exception:
                pass
        return 1

    if stop_mode == 'continuous':
        target_detections = None
        os.makedirs(output_dir, exist_ok=True)
        for ctx in live:
            ctx.segmenting = True
            ctx.rates = args.site is not None
        log('INFO', None,
            f'[REQ] recording continuously in {segment_s:g}s segments into {output_dir} until stopped '
            f'({len(live)} TimeTagger(s): {[c.tag for c in live]})')
    elif stop_mode == 'duration':
        target_detections = None
        log('INFO', None,
            f'[REQ] recording for {record_duration_s:.1f}s on {site_a_name} + {site_b_name} '
            f'({len(live)} TimeTagger(s): {[c.tag for c in live]})')
    else:
        target_detections = target_detection_count
        log('INFO', None,
            f'[REQ] recording until {target_detections} detections per TimeTagger on {site_a_name} + {site_b_name} '
            f'({len(live)} TimeTagger(s): {[c.tag for c in live]})')

    # barrier sized to the survivors, so start_stream() fires across TimeTaggers in lockstep
    barrier = threading.Barrier(len(live))
    threads = []
    # base is computed here (not just at the final write) so reader_thread can
    # name its scratch files with the same stem write_outputs will use later
    for ctx in live:
        base = make_file_basename(ctx.cfg['tt_ip'], ctx.cfg['tt_port'], started_at)
        t = threading.Thread(
            target=reader_thread,
            args=(ctx, barrier, stop_event, base, output_dir, target_detections, per_call_cap),
            name=f'reader-{ctx.tag}',
            # daemon=True: a TimeTagger that stops responding mid-read leaves
            # this thread permanently blocked inside read_stream()
            daemon=True,
        )
        threads.append(t)
        t.start()

    live_cfg = cfg.get('live_coincidence', {})
    live_enabled = live_cfg.get('enabled', True) if args.live is None else args.live
    live_thread = None
    if live_enabled and len(live) == 2:
        # same ordering *_timestamp_sequence.npy files will sort into once written,
        # so the live A/B assignment (and lag sign) matches the final batch report
        ctx_a, ctx_b = sorted(live, key=lambda c: make_file_basename(c.cfg['tt_ip'], c.cfg['tt_port'], started_at))
        from live_peak import tracker_kwargs
        if args.live_interval_s is not None:
            live_cfg = {**live_cfg, 'interval_s': args.live_interval_s}
        if args.live_window_s is not None:
            live_cfg = {**live_cfg, 'window_s': args.live_window_s}
        interval_s, window_s = live_cfg.get('interval_s', 5.0), live_cfg.get('window_s', 30.0)
        for ctx in (ctx_a, ctx_b):
            ctx.live_queue = queue.Queue()
        live_thread = threading.Thread(
            target=live_coincidence_thread,
            args=(ctx_a, ctx_b, stop_event, interval_s, tracker_kwargs({**cfg, 'live_coincidence': live_cfg})),
            name='live-coincidence',
            daemon=True,
        )
        live_thread.start()
        log('INFO', None, f'[CONF] live coincidence tracking enabled: searching the trailing {window_s:.1f}s until the peak locks, '
                          f'then following it chunk by chunk; a summary every {interval_s:.1f}s')

    seg_thread = None
    if stop_mode == 'continuous':
        ordered = sorted(live, key=lambda c: make_file_basename(c.cfg['tt_ip'], c.cfg['tt_port'], started_at))
        seg_thread = threading.Thread(target=segment_thread, args=(ordered, stop_event, started_at, segment_s, output_dir, threads, len(ordered) == 1),
                                      name='segments', daemon=True)
        seg_thread.start()

    # duration mode: timeout on stop_event, then set it. detection_count mode: wait for
    # threads to finish on their own (they stop at the target); Ctrl+C still sets stop_event
    try:
        if stop_mode == 'continuous':
            while not stop_event.wait(1.0):
                if any(c.error for c in live):
                    log('ERROR', None, '[STOP] a TimeTagger failed, ending the continuous recording')
                    break
        elif stop_mode == 'duration':
            stopped_early = stop_event.wait(timeout=record_duration_s)
            if not stopped_early:
                log('INFO', None, 'requested duration reached, stopping both streams')
        else:
            for t in threads:
                while t.is_alive():
                    t.join(timeout=0.5)
    except KeyboardInterrupt:
        log('INFO', None, 'received Ctrl+C, stopping both streams')
    finally:
        stop_event.set()

    # the next read_stream() can take up to ~10s to return at high rates; give threads time
    for t, ctx in zip(threads, live):
        t.join(timeout=30.0)
        if t.is_alive():
            log('WARN', t.name, 'did not finish within 30s of being asked to stop, forcing its connection closed')
            # Best-effort unblock: closing the socket out from under a thread stuck in
            # read_stream() usually makes it raise/return instead of waiting to time out
            try:
                ctx.stream.disconnect()
            except Exception:
                pass

    if seg_thread is not None:
        # continuous mode: every segment is already written, each complete on its own
        seg_thread.join(timeout=60.0)
        failed = {}
    else:
        failed = None
    # strict quorum: both TimeTaggers must be healthy, or nothing is saved
    # checked before any write, so a failed run leaves zero files behind
    if failed is None:
        failed = {ctx.tag: ctx_failure_reason(ctx) for ctx in live}
    failed = {tag: reason for tag, reason in failed.items() if reason is not None}
    if failed:
        for tag, reason in failed.items():
            log('ERROR', tag, f'[STOP] this TimeTagger\'s recording is unusable ({reason})')
        log('ERROR', None, '[STOP] both TimeTaggers are required to be healthy; discarding all data from this run, nothing saved')
        # "zero files behind" above includes any scratch files _flush_overflow
        # wrote mid-recording, not just the final .npy this branch never gets to
        for ctx in live:
            for path in (ctx.scratch_ch_path, ctx.scratch_ts_path):
                if path and os.path.exists(path):
                    try:
                        os.remove(path)
                    except OSError:
                        pass
        rc = 1
    elif seg_thread is not None:
        rc = 0 if not any(c.error for c in live) else 1
    else:
        # basename uses the same wall-clock tag for both TimeTaggers
        for ctx in live:
            base = make_file_basename(ctx.cfg['tt_ip'], ctx.cfg['tt_port'], started_at)
            write_outputs(ctx, base, output_dir)
        rc = 0

    # Gap between host wall-clock time and the TT's own stream uptime signals data loss
    # (reads too slow). Only meaningful in duration mode; detection_count mode expects a gap
    safe_print('')
    log('INFO', None, '[CONF] ==== recording summary ====')
    for ctx in live:
        if not ctx.frames_meta or ctx.wall_start_s is None or ctx.wall_end_s is None:
            log('WARN', ctx.tag, 'no data was captured during this recording')
            continue
        wall_s = ctx.wall_end_s - ctx.wall_start_s
        tt_up_s = ctx.frames_meta[-1]['stream_uptime_s']
        gap_s = wall_s - tt_up_s
        n_detections = sum(m['n_detections'] for m in ctx.frames_meta)
        n_capped = sum(1 for m in ctx.frames_meta if m['capped'])
        rate_dps = n_detections / tt_up_s if tt_up_s > 0 else 0.0

        log('INFO', ctx.tag,
            f'{n_detections} detections across {len(ctx.frames_meta)} reads, '
            f'average rate {rate_dps * 1e-6:.2f} Mdet/s over {tt_up_s:.2f}s of device uptime')

        likely_loss = stop_mode == 'duration' and (gap_s > 0.5 or n_capped > 0)
        if not likely_loss:
            log('INFO', ctx.tag, '[OK] no sign of data loss: host recording time matches the device\'s own reported uptime')
            continue
        if n_capped > 0:
            log('WARN', ctx.tag,
                f'{n_capped} of {len(ctx.frames_meta)} reads came back at the server\'s maximum batch size; '
                f'this detector\'s rate may exceed what single reads can keep up with')
        if gap_s > 0.5:
            log('WARN', ctx.tag,
                f'the device\'s last reported uptime ({tt_up_s:.2f}s) is {gap_s:.2f}s behind how long we were '
                f'actually recording ({wall_s:.2f}s); detections from roughly that last {gap_s:.2f}s window may be missing')

    log('INFO', None, 'recording finished')
    return rc


if __name__ == '__main__':
    raise SystemExit(main())
