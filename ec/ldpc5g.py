'''
5G NR LDPC reconciliation for QKD on two libraries: py3gpp supplies the 3GPP TS 38.212 parity-check
matrices, ldpc (Roffe) decodes the error pattern from its syndrome by belief propagation
'''

import base64
import json
import math
import struct
import sys
from concurrent.futures import ProcessPoolExecutor
from dataclasses import dataclass, field

import numpy as np
import scipy.sparse as sp
from ldpc import BpDecoder
from ldpc.mod2 import rank as gf2_rank
from py3gpp.nrLDPCEncode import _lift_basegraph, _load_basegraph

KB = 22            # information columns of base graph 1
Z = 384            # largest 5G lifting size, the longest frames
TAG_BITS = 32      # per-frame hash: a satisfied syndrome is not proof while positions stay punctured
KNOWN = 1e-12      # prior error rate of a position both sides know


def h2(p):
    return 0.0 if p <= 0 or p >= 1 else -p * math.log2(p) - (1 - p) * math.log2(1 - p)


def lifting_set(z):
    '''Set index of lifting size z in TS 38.212 Table 5.3.2-1, z = a * 2^j'''
    for i, a in enumerate((2, 3, 5, 7, 9, 11, 13, 15)):
        v = a
        while v <= 384:
            if v == z:
                return i
            v *= 2
    raise ValueError(f'{z} is not a 5G lifting size')


def nr_matrix(m, z=Z):
    '''The first m rows and KB+m columns of base graph 1, lifted by z'''
    bm = _load_basegraph(lifting_set(z), 1)[:m, :KB + m]
    return sp.csr_matrix(_lift_basegraph(bm, z), dtype=np.uint8)


def min_rows(h):
    '''Below row 7 only the four core rows hold the core, which needs 4/24 >= 1.6 h2(QBER)'''
    return 4 if 4 / (KB + 2) >= 1.6 * h else 7


@dataclass
class Plan:
    '''Public frame layout for one QBER: code, punctured positions in reveal order, key positions'''
    m: int
    n: int
    checks: int
    pcm: sp.csr_matrix
    punctured: np.ndarray
    key: np.ndarray
    credit: bool
    step: int


_plans = {}


def plan(qber, f_low=1.0, f_high=2.0, steps=40, seed=3, z=Z):
    '''Smallest-m code whose disclosure spans f_low to f_high times the Shannon limit'''
    h = h2(max(qber, 1e-4))
    for m in range(min_rows(h), 47):
        n, checks = (KB + m) * z, m * z
        d = max(2 * z, int((checks - f_low * h * n) / (1 - f_low * h)))
        if d < checks and checks >= f_high * h * (n - d):
            break
    else:
        raise ValueError(f'no 5G code fits QBER {qber}')
    key = (m, d, z, seed, steps)
    if key not in _plans:
        pcm = nr_matrix(m, z)
        rest = 2 * z + np.random.default_rng(seed).permutation(n - 2 * z)
        # extra punctured positions are revealed first, the base graph's own two columns last
        punctured = np.concatenate([rest[:d - 2 * z], np.arange(2 * z)])
        keep = np.ones(n, bool)
        keep[punctured] = False
        credit = gf2_rank(pcm[:, punctured]) == d
        _plans[key] = Plan(m, n, checks, pcm, punctured, np.flatnonzero(keep), credit, max(1, -(-d // steps)))
    return _plans[key]


@dataclass
class Result:
    frames: int = 0
    failed: int = 0
    attempts: int = 0
    leaked: int = 0
    corrected: list = field(default_factory=list)


_worker = {}


def _init(p, bp_method, max_iter, step_iter):
    # min-sum needs its usual normalization; the library's default of 1.0 decodes far worse
    dec = BpDecoder(p.pcm, error_rate=0.05, bp_method=bp_method, schedule='serial', max_iter=max_iter, ms_scaling_factor=0.75)
    _worker.update(plan=p, max_iter=max_iter, step_iter=step_iter, dec=dec)


def _frame(args):
    '''One frame: Alice's syndrome, then a reveal of the next slice of punctured values per failure'''
    alice, bob, q, seed = args
    p, dec = _worker['plan'], _worker['dec']
    rng = np.random.default_rng(seed)
    x = np.zeros(p.n, np.uint8)
    x[p.punctured] = rng.integers(0, 2, len(p.punctured), dtype=np.uint8)
    k = len(alice)
    x[p.key[:k]] = alice
    s_alice = (p.pcm @ x) % 2
    y = np.zeros(p.n, np.uint8)
    y[p.key[:k]] = bob
    probs = np.full(p.n, KNOWN)
    probs[p.key[:k]] = np.clip(q, 1e-6, 0.5)
    probs[p.punctured] = 0.5
    revealed, attempts = 0, 0
    while True:
        attempts += 1
        # earlier attempts stop sooner: a frame that needs more checks will get them anyway
        dec.max_iter = _worker['max_iter'] if revealed == len(p.punctured) else _worker['step_iter']
        dec.update_channel_probs(probs)
        e = dec.decode((p.pcm @ y + s_alice) % 2)
        out = y ^ e.astype(np.uint8)
        # the frame tag: equal to comparing with Alice's frame, up to a 2^-32 collision
        ok = bool(dec.converge) and np.array_equal(out[p.key[:k]], alice)
        if ok or revealed == len(p.punctured):
            break
        nxt = p.punctured[revealed:revealed + p.step]
        y[nxt], probs[nxt] = x[nxt], KNOWN
        revealed = min(revealed + p.step, len(p.punctured))
    hidden = len(p.punctured) - revealed
    leak = p.checks + TAG_BITS + attempts - (hidden if p.credit else 0)
    return out[p.key[:k]], ok, attempts, leak


def reconcile(alice, bob, q, qber, bp_method='product_sum', max_iter=100, step_iter=25, seed=3, workers=None):
    '''Corrects bob onto alice frame by frame on every core; q holds each bit's error rate'''
    p = plan(qber, seed=seed)
    kl = len(p.key)
    jobs = [(alice[i:i + kl], bob[i:i + kl], q[i:i + kl], seed * 1_000_003 + j) for j, i in enumerate(range(0, len(alice), kl))]
    res = Result()
    with ProcessPoolExecutor(workers, initializer=_init, initargs=(p, bp_method, max_iter, step_iter)) as ex:
        for out, ok, attempts, leak in ex.map(_frame, jobs):
            res.frames += 1
            res.failed += not ok
            res.attempts += attempts
            res.leaked += leak
            res.corrected.append(out)
    return np.concatenate(res.corrected), res, p


# Service mode: Go pipeline sends matrix work here over stdin and stdout

class _Service:
    '''Caches one lifted matrix and one decoder per number of base rows and lifting size'''

    def __init__(self):
        self.pcm, self.dec = {}, {}

    def matrix(self, m, z):
        if (m, z) not in self.pcm:
            self.pcm[m, z] = nr_matrix(m, z)
        return self.pcm[m, z]

    def decoder(self, m, z):
        if (m, z) not in self.dec:
            # the parallel schedule runs about 6x faster per iteration than serial; with the Go side's longer attempts
            # it discloses no more (doc/ec_comparison.md)
            self.dec[m, z] = BpDecoder(self.matrix(m, z), error_rate=0.05, bp_method='product_sum', schedule='parallel', max_iter=250)
        return self.dec[m, z]

    def handle(self, req):
        op, m, z = req['op'], int(req.get('m', 0)), int(req.get('z', Z))
        if op == 'shape':
            pcm = self.matrix(m, z)
            return {'checks': pcm.shape[0], 'n': pcm.shape[1]}
        if op == 'syndrome':
            x = np.frombuffer(base64.b64decode(req['x']), np.uint8)
            return {'s': base64.b64encode(((self.matrix(m, z) @ x) % 2).astype(np.uint8).tobytes()).decode()}
        if op == 'rank':
            cols = np.asarray(req['cols'], np.int64)
            return {'rank': int(gf2_rank(self.matrix(m, z)[:, cols]))}
        if op == 'decode':
            # the error pattern e between y and Alice's string has syndrome H y + s
            pcm, dec = self.matrix(m, z), self.decoder(m, z)
            y = np.frombuffer(base64.b64decode(req['y']), np.uint8)
            s = np.frombuffer(base64.b64decode(req['s']), np.uint8)
            dec.max_iter = int(req['max_iter'])
            dec.update_channel_probs(np.frombuffer(base64.b64decode(req['probs']), np.float64))
            e = np.asarray(dec.decode(((pcm @ y) + s) % 2), np.uint8)
            return {'x': base64.b64encode((y ^ e).tobytes()).decode(), 'converged': bool(dec.converge)}
        raise ValueError(f'unknown op {op}')


def serve():
    '''Length-prefixed JSON requests on stdin, answers on stdout, until stdin closes'''
    svc, inp, out = _Service(), sys.stdin.buffer, sys.stdout.buffer
    while True:
        head = inp.read(4)
        if len(head) < 4:
            return
        req = json.loads(inp.read(struct.unpack('>I', head)[0]))
        try:
            resp = svc.handle(req)
        except Exception as e:  # reported to the caller, which fails the run
            resp = {'error': f'{type(e).__name__}: {e}'}
        body = json.dumps(resp).encode()
        out.write(struct.pack('>I', len(body)) + body)
        out.flush()


if __name__ == '__main__':
    if sys.argv[1:] == ['serve']:
        serve()
    else:
        sys.exit('usage: ldpc5g.py serve')
