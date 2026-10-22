'''
Key extraction and detector-delay calibration over recording pairs this node holds both halves of
'''

from ..core import flags, kms, procs, settings
from ..core import recordings as rec
from . import jobs
from .alignment import prepared_dirs


def pairs_from(params):
    pairs = [rec.resolve_pair(p.get('a'), p.get('b')) for p in params.get('pairs') or []]
    if not pairs:
        raise ValueError('choose at least one recording pair')
    return pairs


def _check_extract(params):
    pairs_from(params)
    link = params.get('link') or ''
    if link and link not in kms.neighbors():
        raise ValueError(f'{link} is not a QKD link of this node')


@jobs.kind('extract', check=_check_extract)
def extract(job):
    p = job['params']
    pairs = pairs_from(p)
    link = p.get('link') or ''
    job['log'].append(f'analysing {len(pairs)} recording pair(s)')
    with prepared_dirs(pairs) as (dirs, _):
        args = [settings.IMPORTTT_BIN, '-json', '-dir', ','.join(str(d) for d in dirs)]
        flags.method_args(p, args, real=True)
        if p.get('delays'):
            if not settings.DELAYS_PATH.is_file():
                raise ValueError('no detector delay calibration yet')
            args += ['-detector-delays', str(settings.DELAYS_PATH)]
        if link:
            args += ['-keystore', str(settings.IMPORT_DIR / link), '-keystore-max', str(settings.KEYSTORE_MAX)]
        out = procs.run_logged(job['log'], args)
    result = procs.last_json(out)
    if result.get('error'):
        raise RuntimeError(result['error'])
    result['link'] = link
    if link and result.get('keys_saved'):
        job['log'].append(f'{result["keys_saved"]} key(s) handed to the KMS pool of link {settings.NODE_ID}-{link}')
    return result


@jobs.kind('calibrate', check=pairs_from)
def calibrate(job):
    pairs = pairs_from(job['params'])
    with prepared_dirs(pairs) as (dirs, _):
        out = procs.run_logged(job['log'], [settings.IMPORTTT_BIN, '-json', '-dir', ','.join(str(d) for d in dirs),
                                            '-calibrate-delays', str(settings.DELAYS_PATH)])
    try:
        return procs.last_json(out)
    except (ValueError, IndexError):
        return {'written': settings.DELAYS_PATH.is_file()}

