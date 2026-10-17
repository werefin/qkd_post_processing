'''
A stand-in for TimeTaggerStream that plays a recorded *_timestamp_sequence.npy back at stream speed, looping,
so the recorder, the live tracker and continuous QKD can run and be demonstrated without TimeTaggers
'''

import json
import re
import time
from pathlib import Path

import numpy as np

PS_PER_S = 1_000_000_000_000
_NAME_RE = re.compile(r'_(\d+)_(\d+)_(\d+)_(\d+)_p(\d+)_timestamp_sequence\.npy$')


def replay_identity(path) -> dict:
    '''TimeTagger a recording came from, read from its file name: tt_ip and tt_port'''
    m = _NAME_RE.search(Path(path).name)
    if not m:
        raise ValueError(f'{path}: not a <start>_<ip>_p<port>_timestamp_sequence.npy recording')
    return {'tt_ip': '.'.join(m.groups()[:4]), 'tt_port': int(m[5])}


def pair_skew(path_a, path_b) -> float:
    '''B's clock rate relative to A's (the skew fraction), from the pair's coincidence report if there is one,
    else from a search, so looping the pair can scale B's period and keep the lag continuous across loops'''
    for report in Path(path_a).parent.glob('*_coincidence.json'):
        try:
            r = json.loads(report.read_text())
        except ValueError:
            continue
        if Path(r.get('input_a', '')).name == Path(path_a).name and r.get('detected'):
            return float(r['clock_skew']['skew_fraction'])
    import coincidence_peak as cp
    r, _ = cp.analyze_arrays(np.load(path_a, mmap_mode='r'), np.load(path_b, mmap_mode='r'), strict=False)
    return float(r['clock_skew']['skew_fraction']) if r['detected'] else 0.0


class ReplayStream:
    '''Same calls as TimeTaggerStream; each loop continues the clock, one second after the previous ends.
    B's period is A's scaled by the clock skew, so the pair's lag runs on across loops as on a real link'''

    def __init__(self, timestamps_path, period_s: float, loop_period_s: float = 0.5, speed: float = 1.0, rate: float = 1.0):
        self.ts = np.load(timestamps_path)
        ch_path = Path(str(timestamps_path).replace('timestamp_sequence', 'channel_sequence'))
        self.ch = np.load(ch_path) if ch_path.is_file() else np.ones(len(self.ts), np.int8)
        self.t0_rec = int(self.ts[0])
        self.period_ps = int(period_s * PS_PER_S)
        self.clock_ps = int(round(period_s * rate * PS_PER_S))  # one loop on this stream's own clock
        self.read_every = loop_period_s
        self.speed = speed
        self.connected = False
        self.started = None
        self.emitted_ps = 0  # stream time already handed out

    def connect(self) -> bool:
        self.connected = True
        return True

    def disconnect(self):
        self.connected = False

    def is_connected(self):
        return self.connected

    def start_stream(self) -> bool:
        self.started = time.monotonic()
        return True

    def read_stream(self):
        time.sleep(self.read_every)
        if not self.connected:
            return None
        up_s = (time.monotonic() - self.started) * self.speed
        end_ps = int(up_s * PS_PER_S)
        parts_t, parts_c = [], []
        # every loop overlapping the stream interval (emitted, end], shifted to continue the clock
        for loop in range(self.emitted_ps // self.period_ps, end_ps // self.period_ps + 1):
            lo = max(self.emitted_ps - loop * self.period_ps, 0)
            hi = end_ps - loop * self.period_ps
            i = int(np.searchsorted(self.ts, self.t0_rec + lo, side='right' if lo > 0 else 'left'))
            j = int(np.searchsorted(self.ts, self.t0_rec + hi, side='right'))
            if j > i:
                parts_t.append(self.ts[i:j] + loop * self.clock_ps)
                parts_c.append(self.ch[i:j])
        self.emitted_ps = end_ps
        ts = np.concatenate(parts_t) if parts_t else np.empty(0, np.int64)
        ch = np.concatenate(parts_c) if parts_c else np.empty(0, np.int8)
        return {'timestamp_sequence': ts, 'channel_sequence': ch, 'unixtime_readout': time.time(),
                'stream_uptime_s': up_s, 'frame_start_ps': 0, 'frame_end_ps': end_ps}
