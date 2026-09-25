'''
Dual TimeTagger recording, records from two TimeTaggers at once into per-TT npy files
Configure via config.json (see that file and README.md), run --help for every override
'''

import os
import json
import time
import argparse
import threading
from dataclasses import dataclass, field
from datetime import datetime
from typing import Optional

import numpy as np

from tt_stream_external import TimeTaggerStream


DEFAULT_CONFIG_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), 'config.json')

# a site entry's connection-vs-settings split; see config.json
_CONNECTION_KEYS = ('tt_ip', 'tt_port', 'auth_key_hex')
_SETTINGS_KEYS = ('tt_serial', 'channels', 'delays_ps', 'deadtimes_ps', 'triggers_volt', 'loop_period_s')


def load_config(config_path: str) -> dict:
    '''Loads and validates config.json: every known site, every known link,
    and the recording-mode defaults, raising SystemExit on anything wrong'''
    try:
        with open(config_path) as f:
            cfg = json.load(f)
    except (OSError, ValueError) as ex:
        raise SystemExit(f'{config_path}: {ex}')

    sites = cfg.get('sites')
    links = cfg.get('links')
    rec = cfg.get('record_dual')
    if not isinstance(sites, dict) or not sites:
        raise SystemExit(f'{config_path}: missing or empty "sites"')
    if not isinstance(links, dict) or not links:
        raise SystemExit(f'{config_path}: missing or empty "links"')
    if not isinstance(rec, dict):
        raise SystemExit(f'{config_path}: missing "record_dual" section')

    for link_name, link in links.items():
        for key in ('site_a', 'site_b'):
            name = link.get(key)
            if name not in sites:
                raise SystemExit(f'{config_path}: links.{link_name}.{key} = {name!r} not found in "sites"')

    if rec.get('link') not in links:
        raise SystemExit(f'{config_path}: record_dual.link = {rec.get("link")!r} not found in "links"')
    if rec.get('stop_mode') not in ('duration', 'detection_count'):
        raise SystemExit(f'{config_path}: record_dual.stop_mode must be "duration" or "detection_count"')

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
    links = cfg['links']

    if site_a or site_b:
        if not (site_a and site_b):
            raise SystemExit('--site-a and --site-b must be given together')
        for flag, name in (('--site-a', site_a), ('--site-b', site_b)):
            if name not in sites:
                raise SystemExit(f'{flag} {name!r} not found in "sites" ({config_path}); '
                                  f'known sites: {", ".join(sorted(sites))}')
        return site_a, site_b, f'{site_a}+{site_b} (ad hoc pair)'

    name = link_name or cfg['record_dual']['link']
    if name not in links:
        raise SystemExit(f'--link {name!r} not found in "links" ({config_path}); '
                          f'known links: {", ".join(sorted(links))}')
    link = links[name]
    return link['site_a'], link['site_b'], name


def parse_args(argv: Optional[list] = None) -> argparse.Namespace:
    '''CLI overrides for config.json's record_dual section, so any link or
    ad hoc site pair can be recorded without editing the file'''
    parser = argparse.ArgumentParser(description='Record from two TimeTaggers simultaneously.')
    parser.add_argument('config', nargs='?', default=DEFAULT_CONFIG_PATH,
                         help='path to config.json (default: %(default)s)')
    parser.add_argument('--link', metavar='NAME',
                         help='override record_dual.link; any name under "links" '
                         '(e.g. bratislava_nitra, bratislava_trnava)')
    parser.add_argument('--site-a', metavar='NAME',
                         help='record an ad hoc pair instead of a named link: any name under "sites". '
                         'Requires --site-b too; takes precedence over --link.')
    parser.add_argument('--site-b', metavar='NAME',
                         help='see --site-a; any name under "sites"')
    parser.add_argument('--stop-mode', choices=('duration', 'detection_count'),
                         help='override record_dual.stop_mode')
    parser.add_argument('--duration', type=float, metavar='SECONDS',
                         help='override record_dual.record_duration_s')
    parser.add_argument('--target-count', type=int, metavar='N',
                         help='override record_dual.target_detection_count')
    parser.add_argument('--output-dir', metavar='DIR', help='override record_dual.output_dir')
    return parser.parse_args(argv)


_PRINT_LOCK = threading.Lock()


def safe_print(msg: str) -> None:
    '''Thread-safe stdout print, so per-TT log lines don't interleave'''
    with _PRINT_LOCK:
        print(msg, flush=True)


@dataclass
class TTContext:
    '''Per-TimeTagger runtime state shared between main and its reader thread'''
    cfg: dict
    stream: TimeTaggerStream
    channels: list = field(default_factory=list)     # list[np.ndarray]
    timestamps: list = field(default_factory=list)    # list[np.ndarray]
    frames_meta: list = field(default_factory=list)   # list[dict]
    stream_started: bool = False
    error: Optional[str] = None
    wall_start_s: Optional[float] = None   # time.monotonic() right after start_stream()
    wall_end_s: Optional[float] = None     # time.monotonic() right after the loop exits

    @property
    def tag(self) -> str:
        '''Short identifier for log lines and thread names, e.g. 192.168.101.3:49404'''
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
    '''e.g. 20260605_153012_192_168_101_3_p49404, dots in the IP become underscores'''
    ip_safe = ip.replace('.', '_')
    ts = started_at.strftime('%Y%m%d_%H%M%S')
    return f'{ts}_{ip_safe}_p{port}'


def reader_thread(ctx: TTContext,
                   start_barrier: threading.Barrier,
                   stop_event: threading.Event,
                   target_detections: Optional[int] = None,
                   per_call_cap: int = 4_000_000) -> None:
    '''Waits at the barrier so streams across TimeTaggers start in lockstep,
    then loops read_stream() until stopped, disconnected, or target reached'''
    try:
        start_barrier.wait(timeout=10.0)
    except threading.BrokenBarrierError:
        ctx.error = 'barrier_broken'
        safe_print(f'[{ctx.tag}] barrier broken before start_stream')
        return

    if not ctx.stream.start_stream():
        ctx.error = 'start_stream_failed'
        safe_print(f'[{ctx.tag}] start_stream FAILED')
        return

    ctx.stream_started = True
    ctx.wall_start_s = time.monotonic()
    safe_print(f'[{ctx.tag}] stream started')

    total_detections = 0
    reached_target = False

    while not stop_event.is_set() and ctx.stream.is_connected():
        frame = ctx.stream.read_stream()
        if frame is None:
            ctx.error = 'read_stream_failed'
            safe_print(f'[{ctx.tag}] read_stream returned None, stopping')
            break

        # int8 covers physical channels 1...4 (and any negative falling-edge tags);
        # int64 holds picosecond timestamps, which can exceed 2^32 for long runs
        ch = np.asarray(frame['channel_sequence'], dtype=np.int8)
        ts = np.asarray(frame['timestamp_sequence'], dtype=np.int64)

        # detection_count mode: trim this frame if it would push us past the target
        trimmed = False
        if target_detections is not None and total_detections + ch.size >= target_detections:
            keep = target_detections - total_detections
            ch = ch[:keep]
            ts = ts[:keep]
            trimmed = True

        ctx.channels.append(ch)
        ctx.timestamps.append(ts)
        total_detections += ch.size

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

        cap_warn = '  !! CAPPED, likely lost detections' if meta['capped'] else ''
        trim_note = '  (trimmed to target)' if meta['trimmed'] else ''
        safe_print(
            f'[{ctx.tag}] '
            f'up={meta["stream_uptime_s"]:7.2f}s '
            f'detections={meta["n_detections"]:>7d} '
            f'total={total_detections:>9d} '
            f't0={meta["frame_start_ps"]} '
            f't1={meta["frame_end_ps"]} '
            f'unix={meta["unixtime_readout"]:.3f}'
            f'{cap_warn}{trim_note}'
        )

        if target_detections is not None and total_detections >= target_detections:
            reached_target = True
            safe_print(f'[{ctx.tag}] target {target_detections} detections reached')
            break

    # No stop request and no target reached leaves only a dropped connection,
    # flagged here so main discards the whole recording instead of saving a short file
    if ctx.error is None and not reached_target and not stop_event.is_set():
        ctx.error = 'disconnected'
        safe_print(f'[{ctx.tag}] stream disconnected before stop')

    try:
        ctx.stream.disconnect()
    except Exception:
        pass

    ctx.wall_end_s = time.monotonic()


def ctx_failure_reason(ctx: TTContext) -> Optional[str]:
    '''Why this TimeTagger disqualifies the recording, or None if healthy.'''
    if ctx.error is not None:
        return ctx.error
    if not ctx.stream_started:
        return 'stream_not_started'
    if not ctx.frames_meta:
        return 'no_frames'
    if ctx.wall_start_s is None or ctx.wall_end_s is None:
        return 'incomplete_wall_clock'
    return None


def write_outputs(ctx: TTContext, base: str, outdir: str) -> None:
    '''Concatenates the buffered arrays and writes the .npy + .json sidecar.'''
    if not ctx.frames_meta:
        safe_print(f'[{ctx.tag}] no data captured, skipping write')
        return

    ch = np.concatenate(ctx.channels) if ctx.channels else np.array([], dtype=np.int8)
    ts = np.concatenate(ctx.timestamps) if ctx.timestamps else np.array([], dtype=np.int64)

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

    safe_print(
        f'[{ctx.tag}] wrote {ch.size} detections across '
        f'{len(ctx.frames_meta)} frames -> {base}_*'
    )


def main(argv: Optional[list] = None) -> int:
    args = parse_args(argv)
    try:
        cfg = load_config(args.config)
        rec = cfg['record_dual']
        sites = cfg['sites']
        site_a_name, site_b_name, link_label = resolve_link(
            cfg, args.config, args.link, args.site_a, args.site_b)
    except SystemExit as ex:
        safe_print(str(ex))
        return 2

    stop_mode = args.stop_mode or rec['stop_mode']
    record_duration_s = rec['record_duration_s'] if args.duration is None else args.duration
    target_detection_count = rec['target_detection_count'] if args.target_count is None else args.target_count
    output_dir = args.output_dir or rec['output_dir']
    per_call_cap = rec.get('per_call_detection_cap', 4_000_000)

    safe_print(f'link: {link_label} ({site_a_name} + {site_b_name})')
    tt_a_conn, tt_a_settings = site_connection_and_settings(sites[site_a_name])
    tt_b_conn, tt_b_settings = site_connection_and_settings(sites[site_b_name])

    started_at = datetime.now()
    stop_event = threading.Event()

    contexts = [
        TTContext(cfg=tt_a_conn, stream=build_stream(tt_a_conn, tt_a_settings)),
        TTContext(cfg=tt_b_conn, stream=build_stream(tt_b_conn, tt_b_settings)),
    ]

    # connect each TimeTagger sequentially; collect the survivors
    live = []
    for ctx in contexts:
        safe_print(f'[{ctx.tag}] connecting...')
        if ctx.stream.connect():
            live.append(ctx)
        else:
            ctx.error = 'connect_failed'
            safe_print(f'[{ctx.tag}] connect FAILED, skipping')

    if len(live) < len(contexts):
        failed = [ctx.tag for ctx in contexts if ctx.error == 'connect_failed']
        safe_print(
            f'Both TimeTaggers required, only {len(live)}/{len(contexts)} reachable '
            f'({", ".join(failed)} failed) -- aborting, nothing recorded.'
        )
        for ctx in live:
            try:
                ctx.stream.disconnect()
            except Exception:
                pass
        return 1

    if stop_mode == 'duration':
        target_detections = None
        safe_print(f'mode=duration: recording for {record_duration_s:.1f}s on '
                   f'{site_a_name} + {site_b_name}, {len(live)} TimeTagger(s): {[c.tag for c in live]}')
    else:
        target_detections = target_detection_count
        safe_print(f'mode=detection_count: recording until {target_detections} detections per TT on '
                   f'{site_a_name} + {site_b_name}, {len(live)} TimeTagger(s): {[c.tag for c in live]}')

    # barrier sized to the survivors, so start_stream() fires across TimeTaggers in lockstep
    barrier = threading.Barrier(len(live))
    threads = []
    for ctx in live:
        t = threading.Thread(
            target=reader_thread,
            args=(ctx, barrier, stop_event, target_detections, per_call_cap),
            name=f'reader-{ctx.tag}',
            daemon=False,
        )
        threads.append(t)
        t.start()

    # main waits for the appropriate stop signal:
    #   duration mode: timeout on stop_event, then set it
    #   detection_count mode: just wait for threads to finish on their own
    #     (they stop themselves at the target); Ctrl+C still sets stop_event
    try:
        if stop_mode == 'duration':
            stopped_early = stop_event.wait(timeout=record_duration_s)
            if not stopped_early:
                safe_print('duration elapsed, stopping...')
        else:
            for t in threads:
                while t.is_alive():
                    t.join(timeout=0.5)
    except KeyboardInterrupt:
        safe_print('\nKeyboardInterrupt, stopping...')
    finally:
        stop_event.set()

    # the next read_stream() can take up to ~10s to return at high rates; give threads time
    for t in threads:
        t.join(timeout=30.0)
        if t.is_alive():
            safe_print(f'WARNING: thread {t.name} did not finish within 30s')

    # strict quorum: both TimeTaggers must be healthy, or nothing is saved -
    # checked before any write, so a failed run leaves zero files behind
    failed = {ctx.tag: ctx_failure_reason(ctx) for ctx in live}
    failed = {tag: reason for tag, reason in failed.items() if reason is not None}
    if failed:
        for tag, reason in failed.items():
            safe_print(f'[{tag}] FAILED: {reason}')
        safe_print('Both TimeTaggers required -- discarding all data, nothing saved.')
        rc = 1
    else:
        # basename uses the same wall-clock tag for both TimeTaggers
        for ctx in live:
            base = make_file_basename(ctx.cfg['tt_ip'], ctx.cfg['tt_port'], started_at)
            write_outputs(ctx, base, output_dir)
        rc = 0

    # per-TT data-loss summary: host wall-clock recording duration vs. the
    # TT's last reported stream_uptime_s. A meaningful deficit (more than a
    # frame's worth) means the TT's ring buffer overflowed and detections
    # were lost. Only meaningful in duration mode - in detection_count mode
    # we deliberately stop once the target is hit, so a deficit there is
    # expected and does NOT indicate loss inside the kept window.
    safe_print('')
    safe_print('=== summary ===')
    for ctx in live:
        if not ctx.frames_meta or ctx.wall_start_s is None or ctx.wall_end_s is None:
            safe_print(f'[{ctx.tag}] no frames captured')
            continue
        wall_s = ctx.wall_end_s - ctx.wall_start_s
        tt_up_s = ctx.frames_meta[-1]['stream_uptime_s']
        deficit_s = wall_s - tt_up_s
        n_detections = sum(m['n_detections'] for m in ctx.frames_meta)
        n_capped = sum(1 for m in ctx.frames_meta if m['capped'])
        rate_dps = n_detections / tt_up_s if tt_up_s > 0 else 0.0
        if stop_mode == 'duration':
            loss_flag = '  !! LIKELY LOST DATA' if (deficit_s > 0.5 or n_capped > 0) else ''
        else:
            loss_flag = ''   # expected behavior in detection_count mode
        safe_print(
            f'[{ctx.tag}] frames={len(ctx.frames_meta):>3d} '
            f'capped={n_capped:>3d} '
            f'detections={n_detections:>10d} '
            f'rate={rate_dps * 1e-6:6.2f} Mdet/s '
            f'wall={wall_s:6.2f}s '
            f'tt_uptime={tt_up_s:6.2f}s '
            f'deficit={deficit_s:+6.2f}s'
            f'{loss_flag}'
        )

    safe_print('done.')
    return rc


if __name__ == '__main__':
    raise SystemExit(main())
