'''
Command-line flags of the pipeline binaries, built from a request's settings: key length and filter options for
the simulator and cmd/importtt, and the options both halves of a two-party extraction must agree on; error
correction is always optimized Cascade, the codes stay as legacy in the Go library and its CLIs
'''

from . import settings

TWO_PARTY_PARAMS = ('method', 'expected_qber', 'asymptotic', 'filter', 'chunk', 'delays')


def add(body, args, flag, key, cast):
    v = body.get(key)
    if v not in (None, ''):
        args += [flag, str(cast(v))]


def method_args(body, args, real):
    '''Shared by the simulator and real extraction; real adds the coincidence-filter and timing options only
    cmd/importtt has'''
    args.append('-cascade')
    add(body, args, '-chunk', 'chunk', int)
    if body.get('asymptotic'):
        args.append('-asymptotic')
    if real:
        if body.get('filter') is False:
            args.append('-filter=false')
        add(body, args, '-filter-e0', 'filter_e0', float)
        add(body, args, '-window', 'window_ps', int)


def two_party_args(p):
    '''Options both halves must agree on'''
    if (p.get('method') or 'cascade') != 'cascade':
        raise ValueError('two-party extraction runs optimized Cascade')
    args = ['-cascade', '-chunk', str(int(p.get('chunk') or 1024))]
    add(p, args, '-expected-qber', 'expected_qber', float)
    if p.get('asymptotic'):
        args.append('-asymptotic')
    if p.get('filter') is False:
        args.append('-filter=false')
    if p.get('delays') and settings.DELAYS_PATH.is_file():
        args += ['-detector-delays', str(settings.DELAYS_PATH)]
    return args


def agreed(params):
    '''Only the agreed options of a neighbor's announcement'''
    return {k: v for k, v in (params or {}).items() if k in TWO_PARTY_PARAMS}
