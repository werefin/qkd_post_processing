'''
Plan of a log: its channels, coincidence groups, columns, derived values and metadata, fixed for one file
'''

import ast
import math
import re
import subprocess
from datetime import datetime
from itertools import combinations, product
from pathlib import Path

FORMAT = 'qkd_post_processing coincidence log, version 2'
HIST_HALF_PS, HIST_BIN_PS = 20_000, 100  # delay histograms on the dashboard, for placing the ROI

_BINOPS = {ast.Add: lambda a, b: a + b, ast.Sub: lambda a, b: a - b, ast.Mult: lambda a, b: a * b,
           ast.Div: lambda a, b: a / b, ast.Pow: lambda a, b: a ** b}
_FUNCS = {'sqrt': math.sqrt, 'abs': abs, 'min': min, 'max': max}


def compile_expr(text: str, names: set):
    '''Arithmetic on column names, numbers and a few functions, nothing else; None where it is undefined'''
    try:
        tree = ast.parse(text, mode='eval')
    except SyntaxError as e:
        raise ValueError(f'derived "{text}": {e.msg}')
    for node in ast.walk(tree):
        ok = isinstance(node, (ast.Expression, ast.BinOp, ast.UnaryOp, ast.USub, ast.UAdd, ast.Load, *_BINOPS))
        ok = ok or (isinstance(node, ast.Constant) and isinstance(node.value, (int, float)))
        ok = ok or (isinstance(node, ast.Name) and (node.id in names or node.id in _FUNCS))
        ok = ok or (isinstance(node, ast.Call) and isinstance(node.func, ast.Name) and node.func.id in _FUNCS and not node.keywords)
        if not ok:
            bad = node.id if isinstance(node, ast.Name) else type(node).__name__
            raise ValueError(f'derived "{text}": {bad} is not a column, a number or one of {", ".join(_FUNCS)}')

    def ev(node, row):
        if isinstance(node, ast.Constant):
            return float(node.value)
        if isinstance(node, ast.Name):
            return float(row[node.id])
        if isinstance(node, ast.UnaryOp):
            v = ev(node.operand, row)
            return -v if isinstance(node.op, ast.USub) else v
        if isinstance(node, ast.Call):
            return float(_FUNCS[node.func.id](*(ev(a, row) for a in node.args)))
        return _BINOPS[type(node.op)](ev(node.left, row), ev(node.right, row))

    def run(row):
        try:
            v = ev(tree.body, row)
        except (ZeroDivisionError, ValueError, OverflowError, TypeError):
            return None
        return v if math.isfinite(v) else None
    return run


def site_prefixes(names: list) -> list:
    '''Two sites' channels are labelled by site name, as C1 or D3: letters and digits only; a name that cannot
    prefix a label (empty, ending in a digit, or the same as the other) falls back to X and Y'''
    p = [re.sub(r'[^A-Za-z0-9]', '', str(n)) for n in names]
    if len(set(p)) != len(p) or any(not x or x[-1].isdigit() for x in p):
        return ['X', 'Y'][:len(names)]
    return p


class Plan:
    '''sites: (name, channels) per TimeTagger; one site's channels are labelled 1, 2, ..., two sites' by site name'''

    def __init__(self, opts: dict, sites: list):
        self.opts = opts
        self.n_sites = len(sites)
        self.labels, self.where = [], {}
        self.prefix = site_prefixes([n for n, _ in sites]) if self.n_sites == 2 else ['']
        for s, (_, chans) in enumerate(sites):
            for ch in chans:
                label = f'{self.prefix[s]}{ch}'
                self.labels.append(label)
                self.where[label] = (s, int(ch))
        self.delay = {l: 0 for l in self.labels}
        for k, v in opts['delays_ps'].items():
            self.delay[self._check(k)] = int(v)
        tw = opts['twofold']
        if tw == 'all':
            first = [l for l in self.labels if self.where[l][0] == 0]
            rest = [l for l in self.labels if self.where[l][0] == 1]
            tw = [list(p) for p in (product(first, rest) if self.n_sites == 2 else combinations(self.labels, 2))]
        self.groups = [self._group(g) for g in list(tw or []) + list(opts['higher_order'])]
        names = [g['name'] for g in self.groups]
        if len(set(names)) != len(names):
            raise ValueError('two coincidence groups have the same name')
        self.singles = [f'S_{l}' for l in self.labels]
        self.counts = self.singles + [c for g in self.groups for c in (g['name'], g['name'] + '_noise')]
        self.derived = {}
        for name, text in {**self._qber(), **opts['derived']}.items():
            if name in self.counts:
                raise ValueError(f'derived value {name} has the name of a column')
            self.derived[name] = (text, compile_expr(text, set(self.counts) | {'duration_s'}))
        self.columns = ['window', 'start', 'end', 'duration_s', 'complete', *self.counts, *self.derived, 'capped_reads']
        if self.n_sites == 2:
            self.columns.append('aligned')
        lo = int(opts['roi_center_ps']) - int(opts['window_ps']) // 2
        self.roi = (lo, lo + int(opts['window_ps']))
        self.noise = (lo + int(opts['noise_offset_ps']), lo + int(opts['noise_offset_ps']) + int(opts['window_ps']))
        # how far a counted click looks either way: both ROIs and the histograms
        self.reach = max(abs(v) for v in (*self.roi, *self.noise)) + HIST_HALF_PS + 1

    def _qber(self) -> dict:
        '''Channels 1, 2 are the bits of one basis and 3, 4 of the other, as importtt maps them: equal bits are
        correct; each count less its noise ROI, so accidentals do not count as errors'''
        if not self.opts['qber'] or self.n_sites != 2:
            return {}
        have = {g['name'] for g in self.groups}
        pa, pb = self.prefix
        net = lambda a, b: f'(C_{pa}{a}_{pb}{b} - C_{pa}{a}_{pb}{b}_noise)'
        out, parts = {}, []
        for basis, (x, y) in (('HV', (1, 2)), ('DA', (3, 4))):
            if not all(f'C_{pa}{a}_{pb}{b}' in have for a in (x, y) for b in (x, y)):
                continue
            wrong, right = f'{net(x, y)} + {net(y, x)}', f'{net(x, x)} + {net(y, y)}'
            out[f'QBER_{basis}'] = f'({wrong}) / ({wrong} + {right})'
            parts.append((wrong, right))
        if len(parts) == 2:
            wrong = ' + '.join(w for w, _ in parts)
            out['QBER'] = f'({wrong}) / ({wrong} + {" + ".join(r for _, r in parts)})'
        return out

    def _check(self, label):
        if str(label) not in self.where:
            raise ValueError(f'unknown channel {label}, the channels are {self.labels}')
        return str(label)

    def _group(self, g):
        g = {'channels': list(g)} if isinstance(g, (list, tuple)) else dict(g)
        chans = [self._check(c) for c in g.get('channels', [])]
        if len(chans) < 2 or len(set(chans)) != len(chans):
            raise ValueError(f'a coincidence group needs two or more distinct channels, got {chans}')
        return {'name': str(g.get('name') or 'C_' + '_'.join(chans)), 'channels': chans}

    def values(self, counts) -> dict:
        return {c: int(v) for c, v in zip(self.counts, counts)}

    def with_derived(self, row: dict) -> dict:
        for name, (_, fn) in self.derived.items():
            row[name] = fn(row)
        return row

    def metadata(self, sites: list, source: str, alignment: str) -> dict:
        '''Everything a dataset needs to make sense on its own; sites by name and settings, never addresses'''
        o = self.opts
        return {
            'format': FORMAT, 'software': _software_version(), 'created': datetime.now().astimezone().isoformat(timespec='seconds'),
            'experiment': o['metadata'], 'integration_s': float(o['integration_s']), 'source': source,
            'sites': [{'name': name, 'labels': [l for l in self.labels if self.where[l][0] == i],
                       **{k: (settings or {}).get(k) for k in ('channels', 'delays_ps', 'triggers_volt', 'deadtimes_ps')}}
                      for i, (name, settings) in enumerate(sites)],
            'clock_alignment': alignment, 'software_delays_ps': self.delay,
            'signal_roi_ps': list(self.roi), 'noise_roi_ps': list(self.noise),
            'groups': {g['name']: g['channels'] for g in self.groups},
            'derived': {k: t for k, (t, _) in self.derived.items()},
            'columns': {
                'window': 'index of the integration window, from 0',
                'start, end': 'time of the window, ISO 8601 with the UTC offset',
                'duration_s': 'integration time of the row; shorter only in the last row of a stopped run',
                'complete': '1 when the window ran its full integration time',
                'S_<channel>': 'singles of that channel',
                '<group>': 'clicks of the group\'s first channel with a click of every other channel in the signal ROI, '
                           'relative to the first channel\'s click once all delays are applied',
                '<group>_noise': 'the same count in the noise ROI: the accidentals expected in the signal ROI',
                'capped_reads': 'TimeTagger reads that hit the server\'s batch cap; above 0 some clicks may be missing',
                'aligned': 'two sites only: 1 when the clock model held through the whole window',
            },
        }


def _software_version():
    try:
        r = subprocess.run(['git', 'describe', '--always', '--dirty'], cwd=Path(__file__).parent,
                           capture_output=True, text=True, timeout=5)
        return r.stdout.strip() or None
    except (OSError, subprocess.SubprocessError):
        return None
