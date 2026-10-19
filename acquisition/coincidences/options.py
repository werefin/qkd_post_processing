'''
Options of a coincidence log, from the config's "coincidence_log" section, the dashboard and the command line,
merged and checked
'''

import copy

DEFAULTS = {
    'sites': None,              # one site, or two whose clocks the live tracker aligns; None takes record_dual's link
    'integration_s': 30.0,      # seconds per row
    'window_ps': 1000,          # full width of the signal ROI and of the noise ROI
    'roi_center_ps': 0,         # signal ROI center, relative to the first channel's click once delays are applied
    'noise_offset_ps': 100_000,  # the noise ROI, this far from the signal ROI; whole periods for a pulsed source
    'delays_ps': {},            # software delay per channel, e.g. {"B2": 120}
    'twofold': 'all',           # "all" pairs, or a list of groups such as ["A1", "B1"]
    'higher_order': [],         # groups of three or more channels
    'derived': {},              # name: arithmetic on the columns, evaluated on every row
    'qber': True,               # two sites: QBER_HV, QBER_DA and QBER from the coincidences, as importtt wires channels
    'metadata': {},             # kept in the file header, e.g. {"experiment": "...", "notes": "..."}
    'output_dir': 'coincidence_logs',
}


def merge(*layers) -> dict:
    '''Defaults, then each layer in turn; None leaves an option as it was'''
    opts = copy.deepcopy(DEFAULTS)
    for layer in layers:
        for key, value in (layer or {}).items():
            if value is not None:
                opts[key] = copy.deepcopy(value)
    unknown = set(opts) - set(DEFAULTS)
    if unknown:
        raise ValueError(f'unknown options {sorted(unknown)}; known: {", ".join(DEFAULTS)}')
    if not float(opts['integration_s']) > 0 or not int(opts['window_ps']) > 0:
        raise ValueError('integration_s and window_ps must be positive')
    for key in ('delays_ps', 'derived', 'metadata'):
        if not isinstance(opts[key], dict):
            raise ValueError(f'{key} must be a mapping')
    if opts['sites'] is not None and (isinstance(opts['sites'], str) or not 1 <= len(opts['sites']) <= 2):
        raise ValueError('sites is a list of one or two sites')
    return opts
