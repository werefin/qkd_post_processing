'''
Two-party extraction by hand: each node runs its half on its own recordings only; this node announces its timing,
one way, the neighbor matches its own recording of the same start, and both store the key
'''

import base64
import json
import secrets
import shutil
import tempfile
import time
from pathlib import Path

import coincidence_peak

from ..core import flags, kms, procs, settings
from ..core import recordings as rec
from . import jobs


def _check_alice(params):
    if params.get('peer') not in kms.neighbors():
        raise ValueError('choose a QKD link of this node')
    if not params.get('recordings') or any(n not in rec.names() for n in params['recordings']):
        raise ValueError("tick this node's own recordings")


def _importtt(job, role, dirs, peer, session, params):
    args = [settings.IMPORTTT_BIN, '-role', role, '-dir', ','.join(dirs), '-peer', peer, '-session', session, '-json',
            '-keystore', str(settings.IMPORT_DIR / peer)] + flags.two_party_args(params)
    result = procs.last_json(procs.run_logged(job['log'], args))
    result['link'] = peer
    return result


@jobs.kind('twoparty-alice', check=_check_alice)
def alice(job):
    p = job['params']
    peer = p['peer']
    names = p.get('recordings') or []
    if not names or any(n not in rec.names() for n in names):
        raise ValueError("choose this node's own recordings")
    session = f'{settings.NODE_ID}-{int(time.time())}-{secrets.token_hex(3)}'
    p['session'] = session
    work = Path(tempfile.mkdtemp(prefix='alice_'))
    try:
        dirs = []
        for i, name in enumerate(names):
            d = work / f'r{i}'
            d.mkdir()
            rec.link_into(d, name)
            out = work / f'pub{i}'
            procs.run_logged(job['log'], [settings.IMPORTTT_BIN, '-role', 'alice', '-dir', str(d), '-export-public', str(out), '-json'])
            files = {f.name: base64.b64encode(f.read_bytes()).decode() for f in out.iterdir()}
            job['log'].append(f'announcing {name} to {peer}: timing and bases, no bits')
            kms.pp_send(peer, session, 'offer', {'index': i, 'total': len(names), 'base': rec.base(name),
                                                 'files': files, 'params': {k: p.get(k) for k in flags.TWO_PARTY_PARAMS}})
            dirs.append(str(d))
        job['log'].append(f'waiting for {peer} to align its own recordings')
        kind, body = kms.pp_recv(peer, session)
        if kind != 'accept':
            raise RuntimeError(f'{peer} declined: {body.get("reason", kind)}')
        job['log'].append(f"{peer} accepted; running Alice's half")
        return _importtt(job, 'alice', dirs, peer, session, p)
    finally:
        shutil.rmtree(work, ignore_errors=True)


def _answer(work, i, offer, peer, job):
    '''This node's own recording of the offer's start time, beside the announcement, aligned to it'''
    started = offer['base'][:15]
    mine = [n for n in rec.names() if n.startswith(started) and rec.base(n) != offer['base']]
    if len(mine) != 1:
        raise ValueError(f'no single own recording started at {started} to answer {offer["base"]}')
    d = work / f'r{i}'
    d.mkdir()
    rec.link_into(d, mine[0])
    for name, data in offer['files'].items():
        if Path(name).name != name or not name.endswith(('_public_timestamp_sequence.npy', '_public_flags.npy')):
            raise ValueError(f'unexpected announcement file {name}')
        (d / name).write_bytes(base64.b64decode(data))
    pub_ts = next(d.glob('*_public_timestamp_sequence.npy'))
    job['log'].append(f"aligning {mine[0]} with {peer}'s announcement {offer['base']}")
    report, _ = coincidence_peak.analyze_arrays(rec.load_timestamps(pub_ts), rec.load_timestamps(settings.RECORDINGS_DIR / mine[0]), strict=False)
    if not report['detected']:
        raise ValueError(f'no coincidence peak between {offer["base"]} and {mine[0]}')
    report['input_a'], report['input_b'] = pub_ts.name, mine[0]
    (d / 'pair_coincidence.json').write_text(json.dumps(report))
    return str(d)


@jobs.kind('twoparty-bob')
def bob(job):
    '''Answers a neighbor's announcement; queued by the agent when one arrives'''
    p = job['params']
    peer, session = p['peer'], p['session']
    work = Path(tempfile.mkdtemp(prefix='bob_'))
    try:
        offers = []
        while True:
            kind, body = kms.pp_recv(peer, session, timeout=120)
            if kind != 'offer':
                raise RuntimeError(f'expected an announcement from {peer}, got {kind}')
            offers.append(body)
            if body['index'] + 1 >= body['total']:
                break
        params = flags.agreed(offers[-1].get('params'))
        p.update(params, recordings=[o['base'] for o in offers])
        try:
            dirs = [_answer(work, i, offer, peer, job) for i, offer in enumerate(offers)]
        except Exception as e:
            kms.pp_send(peer, session, 'reject', {'reason': str(e)[:300]})
            raise
        kms.pp_send(peer, session, 'accept', {})
        job['log'].append("accepted; running Bob's half")
        return _importtt(job, 'bob', dirs, peer, session, params)
    finally:
        shutil.rmtree(work, ignore_errors=True)
