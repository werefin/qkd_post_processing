'''
One block's half of importtt, and what of its result the block log keeps
'''

import json
import subprocess

from ...core import flags, settings


def params(expected_qber):
    '''Settings both halves of a continuous block agree on'''
    return {'method': 'cascade', 'expected_qber': expected_qber, 'chunk': 1024}


def run(entry, role, folder, peer, session, agreed):
    args = [settings.IMPORTTT_BIN, '-role', role, '-dir', str(folder), '-peer', peer, '-session', session,
            '-json', '-keystore', str(settings.IMPORT_DIR / peer)] + flags.two_party_args(agreed)
    proc = subprocess.run(args, capture_output=True, text=True, timeout=3600)
    lines = proc.stdout.strip().splitlines()
    try:
        out = json.loads(lines[-1])
    except (IndexError, ValueError):
        raise RuntimeError((proc.stderr.strip().splitlines() or [f'importtt exit status {proc.returncode}'])[-1][:300])
    if out.get('error'):
        raise RuntimeError(out['error'][:300])
    fk = out.get('finite_key') or {}
    entry.update(sifted=out.get('sifted_bits'), qber=out.get('qber'), key_bits=out.get('final_key_bits', 0),
                 keys_saved=out.get('keys_saved', 0), ec_leak=out.get('ec_leaked_bits'), rounds=out.get('cascade_rounds'),
                 phase_error_bound=fk.get('phase_error_bound'), verified=fk.get('verified'), surviving=fk.get('surviving_bits'),
                 charged=fk.get('charged_bits'), window_ps=out.get('filter_window_ps'), compression=out.get('compression_rate'),
                 basis_bits=out.get('basis_bits'), basis_checked=out.get('basis_checked'), basis_errors=out.get('basis_errors'),
                 pair_counts=out.get('pair_counts'))


def export_public(own, pub):
    '''Alice's announcement of a block: timing, bases and multi-photon flags, no bits'''
    subprocess.run([settings.IMPORTTT_BIN, '-role', 'alice', '-dir', str(own), '-export-public', str(pub), '-json'],
                   capture_output=True, text=True, timeout=1800, check=True)
