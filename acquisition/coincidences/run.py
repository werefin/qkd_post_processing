'''
A log run: reads the source, puts the second TimeTagger on the first one's clock, counts, and hands each
finished window and each status to the outputs
'''

import time
from collections import deque
from datetime import datetime

import numpy as np

from .counting import PS_PER_S, WindowCounter, partition

TICK_S = 0.5           # how often a live source is read
HIST_EVERY_S = 2.0
STATUS_EVERY_S = 0.25  # an offline run steps far faster than anyone can watch
RECENT_PS = 10 * 1_000_000_000_000


def log(level, msg):
    print(f'[{level}] [log] {msg}', flush=True)


class Run:
    def __init__(self, plan, source, aligner, outputs: list, stop, meta, file_name, max_windows=None):
        self.plan, self.source, self.aligner, self.outputs, self.stop = plan, source, aligner, outputs, stop
        self.meta, self.file_name, self.max_windows = meta, file_name, max_windows
        self.counter = None
        self.cover = [None] * plan.n_sites  # last click of each TimeTagger, on the common clock
        self.capped: dict = {}
        self.unaligned: set = set()
        self.anchor = None  # (host time, common clock) of the first counted click
        self.written = 0
        self.last_row = None
        # per TimeTagger: its labels and the channel-to-slot table partition() splits a read with
        self.slots = []
        for i in range(plan.n_sites):
            labels = [l for l, (s, _) in plan.where.items() if s == i]
            slot_of = np.full(256, -1, np.int64)
            for k, l in enumerate(labels):
                slot_of[plan.where[l][1] + 128] = k
            self.slots.append((labels, slot_of))
        self.recent = deque()  # (counts, span) of the reads in the last ten seconds
        self.t_start = time.time()
        self.last_hist = self.last_status = 0.0

    def step(self, data, final=False):
        if self.plan.n_sites == 2:
            self.aligner.update(data)
            if not self.aligner.ready:
                self.status('waiting for the clock lock')
                return
            if not self.aligner.ok and self.counter is not None:
                self.unaligned.add(self.counter.current())
        for i, (ch, ts, capped) in enumerate(data):
            if not ts.size:
                continue
            if i == 1:
                ts = self.aligner.to_a(ts)
            if self.counter is None:
                # counting starts at the first click; the other side's earlier clicks are left out
                origin = int(ts[0]) + min(self.plan.delay.values())
                self.counter = WindowCounter(self.plan, origin)
                self.anchor = self.source.anchor or (time.time(), int(ts[-1]))
                log('INFO', f'counting, {self.plan.opts["integration_s"]:g} s per row into {self.file_name}')
            labels, slot_of = self.slots[i]
            grouped, offsets = partition(ch.astype(np.int8, copy=False), ts, slot_of, len(labels))
            for k, label in enumerate(labels):
                if offsets[k + 1] > offsets[k]:
                    self.counter.feed(label, grouped[offsets[k]:offsets[k + 1]] + self.plan.delay[label])
            self.cover[i] = int(ts[-1])
            k = self.counter.current()
            self.capped[k] = self.capped.get(k, 0) + capped
        if self.counter is None or any(c is None for c in self.cover):
            self.status('starting')
            return
        horizon = min(self.cover[s] + self.plan.delay[l] for l, (s, _) in self.plan.where.items()) - self.plan.reach
        done = self.counter.advance(horizon)
        counts, span = self.counter.chunk
        live = None
        if span > 0:
            # derived values, the QBER among them, over the last ten seconds: one read is too few counts
            self.recent.append((counts, span))
            while sum(s for _, s in self.recent) - self.recent[0][1] >= RECENT_PS:
                self.recent.popleft()
            total, total_span = sum(c for c, _ in self.recent), sum(s for _, s in self.recent)
            vals = self.plan.values(total)
            full = self.plan.with_derived({**vals, 'duration_s': total_span / PS_PER_S})
            live = {'t_s': (self.counter.done - self.counter.origin) / PS_PER_S, 'dt_s': span / PS_PER_S,
                    'rates': {c: round(v * PS_PER_S / span, 3) for c, v in self.plan.values(counts).items()},
                    'derived': {k: full[k] for k in self.plan.derived}, 'derived_span_s': total_span / PS_PER_S}
        if final:
            # nothing else is coming: the reach is waited for no longer
            done += self.counter.advance(horizon + self.plan.reach) + self.counter.close()
        for k, counts, span in done:
            self.write(k, counts, span)
        self.status('running', live)

    def write(self, k, counts, span):
        if self.max_windows is not None and self.written >= self.max_windows:
            return
        c = self.counter
        start = c.origin + k * c.period
        when = lambda t: datetime.fromtimestamp(self.anchor[0] + (t - self.anchor[1]) / PS_PER_S).astimezone().isoformat(timespec='milliseconds')
        row = {'window': k, 'start': when(start), 'end': when(start + span), 'duration_s': span / PS_PER_S,
               'complete': int(span == c.period), 'capped_reads': self.capped.pop(k, 0), **self.plan.values(counts)}
        if self.plan.n_sites == 2:
            row['aligned'] = int(k not in self.unaligned)
        self.plan.with_derived(row)
        for out in self.outputs:
            out.row(row)
        self.last_row = row
        self.written += 1
        log('INFO', f'window {k}: {span / PS_PER_S:g} s, singles {sum(row[s] for s in self.plan.singles)}, '
                    f'coincidences {sum(row[g["name"]] for g in self.plan.groups)}' + ('' if row['complete'] else ', incomplete'))
        if self.max_windows is not None and self.written >= self.max_windows:
            self.stop.set()

    def status(self, state, live=None):
        now = time.time()
        if state == 'running' and now - self.last_status < STATUS_EVERY_S:
            return
        self.last_status = now
        st = {'state': state, 'file': self.file_name, 'written': self.written, 'max_windows': self.max_windows,
              'integration_s': float(self.plan.opts['integration_s']), 'uptime_s': now - self.t_start,
              'columns': self.plan.columns, 'singles': self.plan.singles,
              'groups': [{'name': g['name'], 'channels': g['channels']} for g in self.plan.groups],
              'roi_ps': list(self.plan.roi), 'noise_roi_ps': list(self.plan.noise),
              'live': live, 'last_row': self.last_row, 'alignment': self.aligner.status()}
        if self.counter is not None:
            c = self.counter
            st['window'] = {'index': c.current(), 'progress_s': ((c.done - c.origin) % c.period) / PS_PER_S}
            if now - self.last_hist >= HIST_EVERY_S or state == 'finished':
                st['histograms'] = c.histograms()
                self.last_hist = now
        for out in self.outputs:
            out.status(st)

    def loop(self):
        self.source.start(self.stop)
        # outputs open once the source is up, so a failed connection leaves no empty log
        for out in self.outputs:
            out.start(self.plan, self.meta)
        while not self.stop.is_set():
            if self.source.live and self.stop.wait(TICK_S):
                break
            data = self.source.read()
            if data is None:
                break
            self.step(data)
            if self.source.failed:
                log('ERROR', f'{self.source.failed}, ending the log')
                break
        self.stop.set()
        self.step(self.source.finish(), final=True)
        self.last_status = 0.0
        self.status('finished')
