'''
Where a log goes: the CSV with its metadata header, and one status line per update for the dashboard
'''

import json
import os
import sys
from pathlib import Path

STATUS_PREFIX = 'CLOG '  # stdout lines starting with this hold one JSON status for the dashboard


class CsvLog:
    '''"# " lines with the metadata as JSON, the column names, then one row per window, on disk as soon as written;
    read with pandas.read_csv(path, comment="#")'''

    def __init__(self, path: Path):
        self.path, self.f = Path(path), None

    def start(self, plan, meta):
        self.columns = plan.columns
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.f = open(self.path, 'x', encoding='utf-8', newline='')
        for line in json.dumps(meta, indent=1).splitlines():
            self.f.write('# ' + line + '\n')
        self.f.write(','.join(self.columns) + '\n')
        self._sync()

    def _sync(self):
        self.f.flush()
        os.fsync(self.f.fileno())

    def row(self, row):
        cells = [('' if row.get(c) is None else (f'{row[c]:.6g}' if isinstance(row[c], float) else str(row[c]))) for c in self.columns]
        self.f.write(','.join(cells) + '\n')
        self._sync()

    def status(self, st):
        pass

    def close(self):
        if self.f:
            self.f.close()


class StatusLines:
    '''One "CLOG {json}" line per update on stdout'''

    def start(self, plan, meta):
        pass

    def row(self, row):
        pass

    def status(self, st):
        sys.stdout.write(STATUS_PREFIX + json.dumps(st, separators=(',', ':')) + '\n')
        sys.stdout.flush()

    def close(self):
        pass


def _number(v):
    for cast in (int, float):
        try:
            return cast(v)
        except ValueError:
            pass
    return None if v == '' else v


def read_log(path) -> tuple:
    '''The metadata, the columns and the rows (numbers as numbers) of a log, with the standard library only'''
    meta, rows, columns = [], [], None
    with open(path, encoding='utf-8') as f:
        for line in f:
            if line.startswith('#'):
                meta.append(line[2:])
            elif columns is None:
                columns = line.rstrip('\n').split(',')
            elif line.strip():
                rows.append({c: _number(v) for c, v in zip(columns, line.rstrip('\n').split(','))})
    return json.loads(''.join(meta)), columns, rows
