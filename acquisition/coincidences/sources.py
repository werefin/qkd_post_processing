'''
Where clicks come from: TimeTaggers streaming live (or recordings replayed as if they were), or stored
recordings read offline as fast as the disk allows; each read gives every TimeTagger's (channels, timestamps,
capped reads) since the last
'''

import re
import threading
from datetime import datetime
from pathlib import Path

import numpy as np

PS_PER_S = 1_000_000_000_000
_EMPTY = (np.empty(0, np.int8), np.empty(0, np.int64), 0)


class TaggerSource:
    '''sites: (name, site config) per TimeTagger; replay: recordings played back at stream speed instead'''
    live = True
    anchor = None

    def __init__(self, sites: list, replay: list = None):
        import tt_record_dual as rec
        self._rec = rec
        self.names = [n for n, _ in sites]
        if replay:
            from tt_replay import ReplayStream, pair_skew
            period = max((int(t[-1]) - int(t[0])) / 1e12 for t in (np.load(p, mmap_mode='r') for p in replay)) + 1.0
            rates = (1.0, 1.0 + pair_skew(*replay)) if len(replay) == 2 else (1.0,)
            self.ctxs = [rec.TTContext(cfg={'tt_ip': f'replay{i}', 'tt_port': 0}, stream=ReplayStream(p, period, rate=r))
                         for i, (p, r) in enumerate(zip(replay, rates))]
        else:
            self.ctxs = []
            for _, site in sites:
                conn, settings = rec.site_connection_and_settings(site)
                self.ctxs.append(rec.TTContext(cfg=conn, stream=rec.build_stream(conn, settings)))
        self.seen = [0] * len(self.ctxs)
        self.readers = []

    def start(self, stop: threading.Event):
        for name, ctx in zip(self.names, self.ctxs):
            if not ctx.stream.connect():
                raise RuntimeError(f'could not connect to {name}')
            ctx.segmenting = True  # the log takes the buffers itself, nothing is spilled to disk
        barrier = threading.Barrier(len(self.ctxs))
        self.readers = [threading.Thread(target=self._rec.reader_thread, args=(ctx, barrier, stop, 'log', '.', None), daemon=True)
                        for ctx in self.ctxs]
        for t in self.readers:
            t.start()

    def read(self):
        out = []
        for i, ctx in enumerate(self.ctxs):
            with ctx.lock:
                chs, tss = ctx.channels, ctx.timestamps
                ctx.channels, ctx.timestamps = [], []
                frames = ctx.frames_meta[self.seen[i]:]
                self.seen[i] = len(ctx.frames_meta)
            out.append((np.concatenate(chs) if chs else _EMPTY[0], np.concatenate(tss) if tss else _EMPTY[1],
                        sum(1 for m in frames if m.get('capped'))))
        return out

    def finish(self):
        '''After the stop: the readers' last reads'''
        for t in self.readers:
            t.join(timeout=35.0)
        return self.read()

    @property
    def failed(self):
        return next((f'{n}: {c.error}' for n, c in zip(self.names, self.ctxs) if c.error), None)


class FileSource:
    '''Stored *_timestamp_sequence.npy recordings, one or a pair, read a second of data at a time'''
    live = False
    failed = None

    def __init__(self, paths: list):
        self.ts, self.ch = [], []
        for p in map(Path, paths):
            ch_path = p.with_name(p.name.replace('_timestamp_sequence.npy', '_channel_sequence.npy'))
            if not ch_path.is_file():
                raise ValueError(f'{p.name}: no channel file next to it')
            self.ts.append(np.load(p, mmap_mode='r'))
            self.ch.append(np.load(ch_path, mmap_mode='r'))
        self.first = [int(t[0]) for t in self.ts]
        self.last = [int(t[-1]) for t in self.ts]
        self.k = 0
        m = re.match(r'^(\d{8}_\d{6})_', Path(paths[0]).name)
        # rows are timed from the recording's start, which its file name holds
        self.anchor = (datetime.strptime(m[1], '%Y%m%d_%H%M%S').timestamp() if m else 0.0, self.first[0])
        self.names = ['A', 'B'][:len(paths)]  # by place, the first and the second recording
        self.description = f'stored recording{"s" if len(paths) == 2 else ""} started {m[1] if m else "at an unknown time"}, offline'

    def start(self, stop):
        pass

    def read(self):
        if all(f + self.k * PS_PER_S > l for f, l in zip(self.first, self.last)):
            return None
        out = []
        for f, ts, ch in zip(self.first, self.ts, self.ch):
            i, j = int(np.searchsorted(ts, f + self.k * PS_PER_S)), int(np.searchsorted(ts, f + (self.k + 1) * PS_PER_S))
            out.append((np.asarray(ch[i:j]), np.asarray(ts[i:j], dtype=np.int64), 0))
        self.k += 1
        return out

    def finish(self):
        return [_EMPTY] * len(self.ts)
