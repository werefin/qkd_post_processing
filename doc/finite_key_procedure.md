# Finite-key post-processing and key management procedure

**Status:** formal specification of the procedure this codebase implements,
responding to the recommendations in `doc/references/bbm92_post_processing_open_questions_v3.pdf`
("BBM92 network post-processing review"), Section 8. That review found the
deployed pipeline reported an asymptotic key rate with an undocumented,
link-specific margin rather than a composable finite-key guarantee. This
document fixes the parameters, states each stage precisely, and names the code
that implements it. The default path for the Winnow, LDPC and polar strategies
(`RunWinnow`, `RunLDPC`, `RunPolar`) follows it end to end.

## 1. Security claim and parameters

The output of this procedure is a key that is
$(\varepsilon_\mathrm{cor}, \varepsilon_\mathrm{sec})$-secure in the standard
composable sense: with probability at least $1 - \varepsilon_\mathrm{cor}$ the two
sides hold identical keys (correctness), and conditioned on that, the key is
within trace distance $\varepsilon_\mathrm{sec}$ of a uniform key independent of
an adversary's information (secrecy). The two are tracked separately, not
combined into one number, following the review's Appendix B.

| Parameter | Default | Meaning | Code |
|---|---|---|---|
| $\varepsilon_\mathrm{sec}$ | $10^{-10}$ | secrecy failure probability per PA block | `FiniteKeyConfig.EpsSec`, CLI `-eps-sec` |
| $\varepsilon_\mathrm{cor}$ | $10^{-15}$ | correctness failure probability per PA block | `FiniteKeyConfig.EpsCor`, CLI `-eps-cor` |
| $\varepsilon_\mathrm{PE}$ | $\varepsilon_\mathrm{sec}/3$ | parameter-estimation failure, split $\varepsilon_\mathrm{PE}/2$ per basis | `SplitSecurityBudget` |
| $\varepsilon_\mathrm{PA}$ | $\varepsilon_\mathrm{sec}/3$ | privacy-amplification (leftover hash) failure | `SplitSecurityBudget` |

These are starting values, not mandated constants. Choose them for the
application's risk tolerance, but choose them explicitly and record them.

## 2. Procedure, stage by stage

### 2.1 Sifting and coincidence filtering

Coincidences are found from public metadata only (`MatchCoincidences`:
timestamp, basis, multi-photon flag). On real recordings, the clock drift
between the two sites is first tracked and removed (iteratively, with linear
extrapolation), and the window half-width is set to the half width at half
maximum of a Gaussian fit to the drift-corrected peak, all from public timing
(`FilterCoincidences`, `doc/coincidence_filtering.md`, on by default in
`cmd/importtt`). No bit value is read before sifting; every pair inside the
window is kept. Fine timestamps must be announced one way only (Alice to Bob),
so that $\Delta t$ is never public (`doc/coincidence_filtering.md` §4).

### 2.2 Choice of PA block size

A PA block is the unit over which one Toeplitz hash is applied and one set of
$\varepsilon$-costs is charged. Each block pays its own
$\log_2(2/\varepsilon_\mathrm{cor}) + 2\log_2\!\bigl(1/(2\varepsilon_\mathrm{PA})\bigr)$
plus the statistical fluctuation term of §2.5; these are not amortized across
blocks. The review's Table 6 shows a finite-size penalty of 26-40% at 1-minute
blocks and 7-12% at 1-hour blocks for its link. **Use blocks of at least
$10^6$-$10^7$ sifted bits** (review Section 8, item 4).

**Pooling.** Ten-second recordings are far below that. `cmd/importtt` accepts a
comma-separated `-dir` list and treats all the recordings as one PA block, each
drift-corrected on its own; every recording given is included, never a subset
chosen by its QBER (`doc/coincidence_filtering.md` §2, step 7).

Predicted finite key for the Bratislava-Trnava link after filtering (about
3,300 sifted pairs per 10 s at 6.6% QBER), with the exact bound of §2.5, the
optimal sample share, and error correction at $f = 1.2$:

| Acquisition | Sifted pairs | Optimal sample | Phase-error bound $\varphi$ | Finite key |
|---|---|---|---|---|
| 10 s | 3,300 | — | 0.500 | 0 bits |
| 1 min | 19,800 | 34% | 0.106 | 829 bits |
| 5 min | 99,000 | 17% | 0.087 | 10,978 bits (36.6 bit/s) |
| 10 min | 198,000 | 13% | 0.083 | 26,010 bits (43.4 bit/s) |
| 30 min | 594,000 | 10% | 0.077 | 92,600 bits (51.4 bit/s) |
| 60 min | 1,188,000 | 7% | 0.075 | 198,899 bits (55.2 bit/s) |

### 2.3 Error correction

The review's central finding: the deployed detect-and-discard scheme (Hamming
[7,4,3] blocks, a whole block dropped on any detected error) has a
reconciliation inefficiency $f \approx 3.2$-$4.1$, against
$f \approx 1.05$-$1.2$ for actual error correction, because it discards correct
bits along with the errors (review Sections 5 and 8).

**Use polar (`-polar`) or LDPC (`-ldpc`) correction.** Polar reconciliation is
rate-adaptive (`PolarReconcileSoft`): each block starts at a margin of 0.02 below
its capacity; after decoding, Alice and Bob compare a 32-bit universal hash of
the block, and on a mismatch Alice reveals the next least reliable positions
(margin +0.02) and Bob retries. The frozen sets are nested, so the disclosure is
exactly the final frozen count plus 32 bits per attempt, every attempt charged.
A short final block's zero padding is treated as known and costs nothing. LDPC
needs its rate chosen comfortably below $1 - h_2(\mathrm{QBER})$. Winnow
(`-winnow`) is a simpler fallback. Block discard remains available only as a
baseline and is always sized asymptotically (§2.6), because it has no random
sample to bound the phase error with. Optionally, per-bit reliabilities from
public timing (`-soft-bins`, `doc/timing_aware_reconciliation.md`) lower the
disclosure further.

Every strategy reports the bits it actually disclosed (syndromes, frozen
values, parities, CRCs), and that exact count is charged as
$\mathrm{leakedBits}$ in §2.6.

### 2.4 Error verification and correctness

**Finding (review Section 7.2):** CRC-16 alone bounds correctness only to about
$2^{-16} \approx 1.5\times10^{-5}$ per frame, because a fixed CRC polynomial is not
drawn from a random family.

**Implemented** (`distillFiniteKey`, `universalhash.go`): CRC-16 still runs
after error correction to localize and drop chunks with residual errors. Then,
immediately before privacy amplification, Alice and Bob compare a
$t = \lceil \log_2(1/\varepsilon_\mathrm{cor}) \rceil$-bit Toeplitz universal hash
(50 bits at $\varepsilon_\mathrm{cor} = 10^{-15}$) of their entire surviving
strings, with a freshly drawn seed independent of the PA seed. A match bounds
the probability of accepting unequal strings by $2^{-t} \le \varepsilon_\mathrm{cor}$.
A mismatch aborts the block: no key, rather than two keys that disagree. The
$t$ tag bits are charged to $\mathrm{leakedBits}$.

### 2.5 Phase-error estimation

The privacy-amplification bound needs the **phase error** $\varphi$ of the kept
bits, not their bit error. In BBM92, the phase error of the key bits measured in
one basis is estimated from the bit errors of the sacrificed sample in the
**other** basis.

**Implemented** (`crossBasisPhaseError`). Let the sacrificed sample contain
$k_\mathrm{HV}, k_\mathrm{DA}$ bits with $e_\mathrm{HV}, e_\mathrm{DA}$ errors,
and let the reconciled block contain $n_\mathrm{HV}, n_\mathrm{DA}$ bits,
$n = n_\mathrm{HV} + n_\mathrm{DA}$. The review's fluctuation term (Tomamichel et al.),

$$\mu(n_b, k, \varepsilon) = \sqrt{\frac{n_b + k}{n_b\,k}\cdot\frac{k+1}{k}\cdot\ln\frac1\varepsilon},$$

added to the sample error rate, is a Serfling-type bound. The implementation replaces it with the **exact**
bound for the same sampling model (`HypergeomUpperRate`): the $k$ sample bits
are a uniformly random subset of the $n_b + k$ positions, so the number of
sample errors is hypergeometric. With $e$ sample errors, the bound $\bar q$ is
the largest error rate among the $n_b$ unsampled bits for which

$$\Pr\nolimits_{X \sim \mathrm{Hyp}(n_b + k,\; e + \bar q n_b,\; k)}\bigl[X \le e\bigr] \ge \varepsilon .$$

It is never looser than Serfling and is 30-40% tighter at these sizes
(`TestHypergeomUpperRateTighterThanSerfling`), and a Monte Carlo test confirms
it fails at most $\varepsilon$ of the time (`TestHypergeomUpperRateCoverage`).
The bound is

$$\varphi_0 = \frac{n_\mathrm{HV}}{n}\,\bar q(n_\mathrm{HV}, k_\mathrm{DA}, e_\mathrm{DA}, \tfrac{\varepsilon_\mathrm{PE}}{2}) + \frac{n_\mathrm{DA}}{n}\,\bar q(n_\mathrm{DA}, k_\mathrm{HV}, e_\mathrm{HV}, \tfrac{\varepsilon_\mathrm{PE}}{2}),$$

each basis's term capped at $\tfrac12$. Because the CRC stage discards chunks
after seeing their errors, a post-selection correction is then applied
(review Sections 6.1 and 7.3):

$$\varphi = \min\!\left(\tfrac12,\ \varphi_0 \cdot \frac{n}{n_\mathrm{surviving}}\right).$$

The two per-basis estimates each fail with probability at most
$\varepsilon_\mathrm{PE}/2$, so by a union bound the combined estimate fails with
probability at most $\varepsilon_\mathrm{PE}$. A basis with no sample bits gives
$\varphi = \tfrac12$, which means no key.

**The cap is $\tfrac12$, not 1.** $h_2$ rises on $[0, \tfrac12]$ and falls on
$[\tfrac12, 1]$, so at $\varphi = 1$ the rate $1 - h_2(\varphi)$ would equal 1 and
certify the whole block. An earlier version of `EstimatePhaseError` capped at 1 in
exactly the case that should yield nothing; it now caps at $\tfrac12$, and
`FiniteKeySecureKeyLength` returns 0 for any $\varphi \ge \tfrac12$.

**Sample size** (`OptimalSampleFraction`, CLI `-sample-auto`, on by default).
The share of sifted bits sacrificed is chosen to maximize the predicted finite
key, from the sifted count and a declared expected QBER (default: the coincidence
filter's timing-based prediction), before any sample bit is examined, so the
choice cannot depend on the errors it then measures.

Whether bit and phase errors may be treated as independent for this specific
detector and source, which would allow a less conservative estimate, is one of
the review's open action items (Section 7.3). This procedure does not assume it.

### 2.6 Privacy amplification

**Implemented** (`FiniteKeySecureKeyLength`, review Section 6.1). For the
$n_\mathrm{surviving}$ bits left after the CRC stage, once verified:

$$l = \left\lfloor n_\mathrm{surviving}\bigl(1 - h_2(\varphi)\bigr) - \mathrm{leakedBits} - \log_2\frac{2}{\varepsilon_\mathrm{cor}} - 2\log_2\frac{1}{2\varepsilon_\mathrm{PA}} \right\rfloor^+ ,$$

with $\mathrm{leakedBits}$ the exact disclosure of §2.3 plus the tag of §2.4.
There is a single entropy term $h_2(\varphi)$: error correction is already paid
for inside $\mathrm{leakedBits}$. Alice's surviving bits are hashed to $l$ bits
with a Toeplitz matrix from a fresh public seed. `doc/security_proof.md` §2
shows that the last term is exactly the Leftover Hash Lemma bound.

The **asymptotic mode** (CLI `-asymptotic`, and always for block discard) sizes
the key as $n\bigl(1 - h_2(\mathrm{QBER})\bigr) - \mathrm{leakedBits}$. An earlier
version used $n\bigl(1 - 2h_2(\mathrm{QBER})\bigr) - \mathrm{leakedBits}$, which
charges error correction twice, once as $h_2$ and again inside
$\mathrm{leakedBits}$; that has been corrected. The asymptotic figure carries no
finite-size guarantee and should be used only for comparison.

**Measured, simulated** (`TestRunLDPCFiniteKeyAtScale`, fixed 10% sample):
599,259 sifted bits, QBER 2.1%, phase-error bound 2.7%, tag verified: finite key
171,837 bits against 190,188 asymptotic. With the optimal sample
(`cmd/qkd_post_processing -n 1200000 -err 0.02 -ldpc`), 199,311 bits.

**Measured, real** (`cmd/importtt -polar`, all defaults fixed in advance):

| Block | Sifted | QBER | $\varphi$ bound | Finite key |
|---|---|---|---|---|
| one recording | about 3,300 | 5-9% | 0.5 | 0 bits |
| all 7 recordings pooled (70 s) | 23,408 | 6.7% | 0.111 | **535 bits** |
| 6 recordings with detector-delay correction (60 s, run_trigger02 used for calibration) | 20,083 | 7.3% | 0.121 | 0 bits |

The 535-bit key is at the edge of the threshold and is certified only under
one-way timestamp announcement (§2.1). Correcting detector delays removes the
timing leak but raises QBER from 6.7% to 7.3%, because off-centre error classes
move into the window, and 60 s is then not enough. Several minutes of
acquisition are needed for a robust certified key (§2.2).

### 2.7 Key delivery (KMS)

**Implemented**: `kms.go` (store) and `cmd/kms-server` (ETSI GS QKD 014-shaped
HTTP endpoints: Get Key, Get Key with Key IDs, Get Status). Each finished PA
block's key bytes are handed to `KMS.Put` (minting a fresh `key_ID`) on the side
that computed it first. That `key_ID`, never the key, is sent to the peer over
the application's own channel, and the peer's KMS stores its own, already
bit-identical key under that `key_ID` via `KMS.PutWithID`. Applications then
retrieve key material once each via `GetNext` (master) or `Get` (slave); both
enforce single delivery, so a retried request can never hand the same key to two
callers. With `-tls-ca`, `cmd/kms-server` requires and verifies client
certificates (TLS 1.3 mutual TLS), for applications and for relay peers alike;
the multi-hop relay endpoints are enabled with `-node-id`, `-relay-routes`,
`-relay-peers` and `-relay-pools` (`TestRelayOverMutualTLS`).
Keys at rest are encrypted under an operator-provisioned master key, which is a
computational, not information-theoretic, protection (`doc/security_proof.md` §7).

## 3. What this does not resolve

- **Detector assumptions.** Double-click handling, detector dead time and
  detection-efficiency mismatch (review Section 9, item 6) are assumptions of
  the security proof, not something post-processing parameters can fix. The
  recordings show a strong efficiency asymmetry between detector pairs (the A1-B1
  peak carries roughly eight times the counts of A0-B0), so efficiency mismatch
  in particular should be assessed against the proof this product relies on
  (review Section 9, item 4).
- **Classical-channel authentication.** The sifting, reconciliation and
  verification traffic between the two sites must be authenticated (review
  Section 9, item 7). `cmd/kms-server`'s mutual TLS covers key delivery to
  applications, not this traffic.
- **Detector timing side channel.** Measured on these recordings: per-detector
  delays differ by up to about 35 ps, and announced $\Delta t$ would carry about
  0.07 bits of information per sifted bit; calibration (`-calibrate-delays`,
  `-detector-delays`) lowers this to about 0.01 but not to zero. The procedure
  therefore requires one-way timestamp announcement, under which $\Delta t$ is
  never public (`doc/coincidence_filtering.md` §4).
- **Protocol adaptation.** The finite-key formula follows the review, which cites
  Tomamichel, Lim, Gisin, Renner, *Tight finite-key analysis for quantum
  cryptography*, Nat. Commun. 3, 634 (2012). That proof is for
  prepare-and-measure BB84; applying it to entanglement-based BBM92 through the
  source-replacement scheme is standard practice, but it is a protocol-specific
  step a specialist should sign off on before this is relied on as a certified
  security claim. This document gives a precise, documented and tested starting
  point, not a substitute for that review.
