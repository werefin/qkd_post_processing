'''
Coincidence logger: singles, two-fold and higher-order coincidences in a signal ROI and a noise ROI, and values
computed from them, one row per integration window into a CSV whose header holds every setting; live from one
or two TimeTaggers, or offline from stored recordings
'''

import argparse
import json
import signal
import sys
import threading
from datetime import datetime
from pathlib import Path

import site_links
from coincidences import CsvLog, FileSource, LiveModel, OneClock, Plan, Run, StatusLines, TaggerSource, merge

DEFAULT_CONFIG = Path(__file__).with_name('config.json')


def log(level, msg):
    print(f'[{level}] [log] {msg}', flush=True)


def _pairs(items, cast=str):
    out = {}
    for item in items or []:
        k, sep, v = item.partition('=')
        if not sep:
            raise ValueError(f'"{item}" is not KEY=VALUE')
        out[k.strip()] = cast(v.strip())
    return out


def parse_args(argv=None):
    ap = argparse.ArgumentParser(description='Singles and coincidences per integration window into a CSV')
    ap.add_argument('config', nargs='?', default=str(DEFAULT_CONFIG), help='config.json; its "coincidence_log" section gives the defaults')
    ap.add_argument('--sites', nargs='+', metavar='SITE', help='one site, or two')
    ap.add_argument('--recordings', nargs='+', metavar='NPY', help='one or two stored recordings, processed offline')
    ap.add_argument('--replay', nargs='+', metavar='NPY', help='one or two recordings played back as if live, for testing')
    ap.add_argument('--integration-s', dest='integration_s', type=float, help='seconds per row, e.g. 30, 300, 1200, 18000')
    ap.add_argument('--windows', type=int, help='stop after this many rows')
    ap.add_argument('--duration-s', type=float, help='stop after this many seconds')
    ap.add_argument('--window-ps', dest='window_ps', type=int, help='ROI width')
    ap.add_argument('--roi-center-ps', dest='roi_center_ps', type=int, help='signal ROI center')
    ap.add_argument('--noise-offset-ps', dest='noise_offset_ps', type=int, help='noise ROI distance from the signal ROI')
    ap.add_argument('--delay', action='append', metavar='CHANNEL=PS', help='software delay, repeatable')
    ap.add_argument('--group', action='append', metavar='C1+C2+C3', help='a higher-order group, repeatable')
    ap.add_argument('--derived', action='append', metavar='NAME=EXPR', help='a value computed on every row, repeatable')
    ap.add_argument('--metadata', action='append', metavar='KEY=VALUE', help='kept in the header, e.g. experiment=..., repeatable')
    ap.add_argument('--options', help='options as JSON, over the config\'s section and under the flags; the dashboard uses it')
    ap.add_argument('--output-dir', dest='output_dir')
    ap.add_argument('--output', type=Path, help='the log file itself')
    ap.add_argument('--json', action='store_true', help='"CLOG {json}" status lines for the dashboard')
    return ap.parse_args(argv)


def main(argv=None) -> int:
    args = parse_args(argv)
    try:
        cfg = json.loads(Path(args.config).read_text()) if Path(args.config).is_file() else {}
        flags = {k: getattr(args, k) for k in ('sites', 'integration_s', 'window_ps', 'roi_center_ps', 'noise_offset_ps', 'output_dir')}
        if args.delay:
            flags['delays_ps'] = _pairs(args.delay, int)
        if args.group:
            flags['higher_order'] = [g.replace(' ', '').split('+') for g in args.group]
        if args.derived:
            flags['derived'] = _pairs(args.derived)
        if args.metadata:
            flags['metadata'] = _pairs(args.metadata)
        opts = merge(cfg.get('coincidence_log', {}), json.loads(args.options) if args.options else {}, flags)
        files = args.recordings or args.replay
        if files:
            if len(files) not in (1, 2):
                raise ValueError('one or two recordings')
            source = FileSource(files) if args.recordings else TaggerSource([(n, None) for n in ['A', 'B'][:len(files)]], replay=sorted(files))
            sites = [(n, None) for n in source.names]
            description = getattr(source, 'description', f'replay of {len(files)} recording(s)')
        else:
            names = opts['sites']
            if not names:
                default = (cfg.get('record_dual') or {}).get('link')
                if not default:
                    raise ValueError('choose the sites: no "sites" option and no record_dual.link')
                names = list(site_links.link_sites(cfg, default))
            missing = [n for n in names if n not in cfg.get('sites', {})]
            if missing:
                raise ValueError(f'sites {missing} are not in the config')
            sites = [(n, cfg['sites'][n]) for n in names]
            source = TaggerSource(sites)
            description = 'TimeTaggers ' + ' + '.join(names)
        plan = Plan(opts, [(n, (s or {}).get('channels') or [1, 2, 3, 4]) for n, s in sites])
        if len(sites) == 1:
            aligner = OneClock()
        else:
            from live_peak import tracker_kwargs
            aligner = LiveModel(tracker_kwargs(cfg))
    except (OSError, ValueError, KeyError) as e:
        log('ERROR', str(e))
        return 2

    path = args.output
    if path is None:
        # two runs started within one second get _2, _3 and so on
        stem, n = f'{datetime.now():%Y%m%d_%H%M%S}', 1
        path = Path(opts['output_dir']) / f'{stem}_coincidences.csv'
        while path.exists():
            n += 1
            path = Path(opts['output_dir']) / f'{stem}_{n}_coincidences.csv'
    outputs = [CsvLog(path)] + ([StatusLines()] if args.json else [])
    stop = threading.Event()
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda *_: stop.set())
    if args.duration_s:
        threading.Timer(args.duration_s, stop.set).start()
    log('INFO', f'{description}: {len(plan.singles)} channels, {len(plan.groups)} coincidence groups, '
                f'{float(opts["integration_s"]):g} s per row')
    run = Run(plan, source, aligner, outputs, stop, plan.metadata(sites, description, aligner.description), Path(path).name, args.windows)
    try:
        run.loop()
    except RuntimeError as e:
        log('ERROR', str(e))
        return 1
    finally:
        for out in outputs:
            out.close()
    log('INFO', f'finished: {run.written} rows in {path}')
    return 1 if source.failed else 0


if __name__ == '__main__':
    sys.exit(main())
