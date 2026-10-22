'''
Command-line flags of the pipeline binaries, built from a request's settings: error correction and key length for
the simulator and cmd/importtt, and the options both halves of a two-party extraction must agree on
'''

from . import settings

# Default CRC chunk per method: polar and LDPC fix or fail whole blocks, so a coarse chunk keeps
# the 16-bit-per-chunk disclosure small; Winnow leaves scattered errors and needs fine chunks
DEFAULT_CHUNK = {'ldpc5g': 1024, 'polar': 1024, 'ldpc': 1024}

TWO_PARTY_PARAMS = ('method', 'cascade_sample', 'expected_qber', 'polar_margin', 'polar_list', 'soft_bins',
                    'asymptotic', 'filter', 'chunk', 'delays')


def add(body, args, flag, key, cast):
    v = body.get(key)
    if v not in (None, ''):
        args += [flag, str(cast(v))]


def method_args(body, args, real):
    '''Shared by the simulator and real extraction; real adds the coincidence-filter and timing options only
    cmd/importtt has'''
    method = body.get('method', 'cascade')
    if method == 'cascade':
        args.append('-cascade')
    elif method == 'ldpc5g':
        args.append('-ldpc5g')
    elif method == 'polar':
        args.append('-polar')
        add(body, args, '-polar-margin', 'polar_margin', float)
        add(body, args, '-polar-list', 'polar_list', int)
    elif method == 'ldpc':
        args.append('-ldpc')
        for flag, key in (('-ldpc-block', 'ldpc_block'), ('-ldpc-wc', 'ldpc_wc'), ('-ldpc-wr', 'ldpc_wr'), ('-ldpc-iterations', 'ldpc_iterations')):
            add(body, args, flag, key, int)
    elif method == 'winnow':
        args.append('-winnow')
        add(body, args, '-winnow-blocks', 'winnow_blocks', str)
    elif body.get('search_matrix'):
        args.append('-search-matrix')
    if real and method in ('ldpc5g', 'polar', 'ldpc'):
        add(body, args, '-soft-bins', 'soft_bins', int)
    if body.get('chunk') not in (None, ''):
        add(body, args, '-chunk', 'chunk', int)
    elif method in DEFAULT_CHUNK:
        args += ['-chunk', str(DEFAULT_CHUNK[method])]
    if body.get('asymptotic'):
        args.append('-asymptotic')
    if real:
        if body.get('filter') is False:
            args.append('-filter=false')
        add(body, args, '-filter-e0', 'filter_e0', float)
        add(body, args, '-window', 'window_ps', int)


def two_party_args(p):
    '''Error-correction and key options both halves must agree on'''
    method = p.get('method') or 'cascade'
    if method not in ('cascade', 'ldpc5g', 'polar'):
        raise ValueError('two-party extraction runs optimized Cascade, 5G NR LDPC or polar codes')
    args = ['-' + method, '-chunk', str(int(p.get('chunk') or 1024))]
    if method == 'cascade':
        if p.get('cascade_sample'):
            args.append('-cascade-sample')
        add(p, args, '-expected-qber', 'expected_qber', float)
    add(p, args, '-polar-margin', 'polar_margin', float)
    add(p, args, '-polar-list', 'polar_list', int)
    add(p, args, '-soft-bins', 'soft_bins', int)
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
