# Timing-aware soft reconciliation

**Status:** formal procedure, implemented in `softrecon.go` and wired into the
LDPC and polar strategies (`RunLDPC`, `RunPolar`, CLI flag `-soft-bins`). It
changes how efficiently error correction uses information that is already
public; it does not change what is disclosed to an adversary beyond what the
reconciliation code itself discloses, and it does not change privacy
amplification.

## 1. Motivation

Every reconciliation strategy in this codebase treats the $n$ sifted bits of a
block as passing through one binary symmetric channel with crossover
probability equal to the sampled QBER $\bar q$. On an entangled-photon link this
is not the physical error mechanism. A sifted pair is either

- a **true pair**, both photons from one entangled state, whose arrival-time
  offset $\Delta t = t_\text{Bob} - t_\text{Alice}$ is concentrated near zero
  (detector jitter, typically $\sim 10^2$ ps) and whose bits disagree with a small
  intrinsic probability; or
- an **accidental coincidence**, two unrelated photons that happened to fall
  inside the coincidence window, whose $\Delta t$ is uniform over the window
  $[-W, W]$ and whose bits disagree with probability $\tfrac12$.

The probability that a bit is in error therefore depends on its own $\Delta t$.
Write $q(\Delta t)$ for that function. $\Delta t$ is already public: sifting
(`MatchCoincidences`) compares timestamps over the classical channel to find
the pairs at all.

## 2. Result

**Definition 1.** For a block with per-bit error probabilities $q_1,\dots,q_n$,
define

$$H_\text{flat} = h_2\!\left(\frac1n\sum_{i=1}^n q_i\right), \qquad H_\text{soft} = \frac1n\sum_{i=1}^n h_2(q_i),$$

where $h_2$ is the binary entropy. $nH_\text{flat}$ is the Slepian-Wolf minimum
disclosure for a code that only knows the average error rate; $nH_\text{soft}$
is the minimum for a code that also uses each bit's $\Delta t$.

**Theorem 1.** $H_\text{soft} \le H_\text{flat}$, with equality if and only if
all $q_i$ are equal.

*Proof.* $h_2$ is strictly concave on $[0,1]$. By Jensen's inequality,
$\frac1n\sum_i h_2(q_i) \le h_2\!\left(\frac1n\sum_i q_i\right)$, with equality if
and only if the $q_i$ are all equal. $\blacksquare$

**Corollary 1.** Conditioning on $\Delta t$ can only lower the information-theoretic
cost of reconciliation. The reduction $n(H_\text{flat} - H_\text{soft})$ is
largest when error probabilities are most spread out, i.e. when bits near
$\Delta t = 0$ are mostly true pairs and bits near $|\Delta t| = W$ are mostly
accidentals.

## 3. Procedure

Parameters: number of offset bins $B$ (`TimingBins`, CLI `-soft-bins`); the
existing QBER sample fraction; for polar, the rate margin $\delta$
(`RateMargin`).

1. **Sift** as before, keeping each sifted pair's $\Delta t_i$
   (`siftSampleWithTiming`). The bit order and the sacrificed sample are
   identical to the flat procedure for the same seed.
2. **Estimate $q(\Delta t)$** from the sacrificed QBER sample only. Partition
   $[-W, W]$ into $B$ equal bins; for bin $b$ with $c_b$ sample bits of which
   $e_b$ disagree, set
   $$\hat q_b = \min\!\left(\tfrac12,\ \max\!\left(10^{-4},\ \frac{e_b + 1}{c_b + 2}\right)\right).$$
   The Laplace estimator keeps a sparsely populated bin near $\tfrac12$ (no
   information) rather than a confident $0$ or $1$ (`EstimateTimingErrorModel`).
3. **Assign** each remaining bit $i$ its bin's rate $q_i = \hat q_{b(\Delta t_i)}$
   and its channel log-likelihood ratio
   $$L_i = (1 - 2y_i)\,\ln\frac{1-q_i}{q_i}, \qquad L_i = 0 \text{ if } q_i = \tfrac12,$$
   where $y_i$ is Bob's bit (`SoftLLRs`).
4. **Reconcile.**
   - *LDPC* (`LDPCReconcileSoft`): the parity-check matrix and its syndrome are
     unchanged; Bob's belief-propagation decoder starts from $L_i$ instead of one
     flat LLR. Disclosure is identical; more blocks converge.
   - *Polar* (`PolarReconcileSoft`): per block of $N$ bits, propagate per-bit
     Gaussian-approximation means $m_i^{(0)}$ (capacity-matched to BSC($q_i$))
     through the polar recursion to rank synthetic channels, and set
     $$K = \left\lfloor \sum_{i=1}^{N} \bigl(1 - h_2(q_i)\bigr) - \delta N \right\rfloor,$$
     clamped to $[1, N-1]$. Alice discloses the $N-K$ frozen values; Bob
     decodes with $L_i$. Disclosure falls from about $N h_2(\bar q)$ toward
     $N H_\text{soft}$.
5. **Verify and amplify** exactly as before: the CRC stage drops any block the
   decoder got wrong, and privacy amplification charges the actual number of
   disclosed bits.

## 4. Security

**Claim.** The procedure discloses to an adversary nothing beyond what the
flat procedure would disclose for the same reconciliation code, and privacy
amplification remains correctly charged.

*Argument.*
(a) $\Delta t_i$ is public before reconciliation starts (it is how pairs are
found), and the sacrificed sample is public by construction, so $\hat q$, every
$q_i$, every $L_i$, the frozen set, and $K$ are deterministic functions of
public data.
(b) The only key-dependent messages are the $N-K$ frozen values (polar) or the
$M$ syndrome bits (LDPC), plus CRCs, exactly as in the flat procedure; their
count is what `LeakedBits` charges.
(c) No bit is discarded or selected based on observed errors: every sifted bit
stays in its block, and the phase-error estimate and privacy amplification see
the same block as the flat procedure. This is the property that distinguishes
the procedure from post-selecting bits by coincidence window.
(d) An adversary able to shape the $\Delta t$ distribution, for example by
injecting timing-shifted light, can only make the model wrong. A wrong model
causes decoding failures, which the CRC and universal-hash checks catch. That
is a denial of service, not a loss of secrecy. $\blacksquare$

## 5. Measured behaviour

**Simulated link** (`softrecon_test.go`: 6% accidentals uniform over $\pm 2$ ns,
true pairs with 100 ps Gaussian jitter and 1% intrinsic error, identical seeds
for flat and soft):

| Link (QBER) | Strategy | Flat | Timing-aware |
|---|---|---|---|
| 6% accidentals (4.1%) | polar, $N=4096$, $L=8$, $\delta=0.1$: disclosed / CRC-dropped / finite-key final key | 38,731 / 0 / 26,340 bits | 29,263 / 0 / 35,808 bits |
| 6% accidentals (3.7%) | LDPC (3,10), rate 0.7: blocks converged / finite-key final key | 3 of 27 / 0 bits | 27 of 27 / 34,792 bits |
| 6% accidentals | disclosure bound, bits per bit | $H_\text{flat} = 0.237$ | $H_\text{soft} = 0.163$ |

**Real recordings** (Bratislava-Trnava, `ent_qkd_data/`, polar $N=4096$,
$\delta = 0.05$, 16 bins, model fitted on the 5% sample; measured before
coincidence filtering existed, `doc/coincidence_filtering.md`):

| Recording | QBER | Disclosed, flat → timing | CRC-dropped chunks, flat → timing |
|---|---|---|---|
| run_trigger02 | 0.195 | 6,250 → 4,554 | 16 → 16 |
| variance 084244 | 0.273 | 7,340 → 6,261 | 0 → 31 |
| variance 084119 | 0.288 | 7,504 → 6,858 | 0 → 0 |
| variance 084056 | 0.358 | 16,236 → 12,925 | 0 → 16 |
| variance 084151 | 0.411 | 24,570 → 22,433 | 0 → 0 |
| run_trigger03 | 0.417 | 24,570 → 22,571 | 0 → 0 |
| run_delayfix | 0.430 | 28,665 → 25,230 | 0 → 0 |

The final key is zero for every real recording under both procedures: their
QBER is above the roughly 11% threshold beyond which no secret key is possible,
and no reconciliation method changes that. The gain on these recordings is
also small (8-27% less disclosure, against 25% less disclosure and a 31% lower
bound on the simulated link)
because accidentals dominate even the centre of the coincidence peak on this
link, so $q(\Delta t)$ is nearly flat.

## 6. Limitations

- The gain depends on how much $q(\Delta t)$ varies. When accidentals dominate
  the peak centre as well as the edges, as on the current Bratislava-Trnava
  recordings, it is small.
- At a fixed margin $\delta$, the polar path's rate rises toward the lower
  bound $H_\text{soft}$, so more blocks may fail (two of the seven real
  recordings above). $\delta$ trades disclosure against block failures, as it
  does for the flat procedure.
- $\hat q_b$ is estimated from a finite sample. Too many bins for the sample
  size makes bins noisy; the Laplace estimator pulls them toward $\tfrac12$,
  which costs efficiency, not security.
- Final-key figures above use the finite-key bound at
  $\varepsilon_\text{sec} = 10^{-10}$ with the exact hypergeometric phase-error
  bound, now the default for LDPC and polar, and polar is rate-adaptive
  (`doc/finite_key_procedure.md` §2.3 and §2.5).
- I have not surveyed the literature. Soft information is standard in
  continuous-variable QKD reconciliation, and timing-derived reliability may
  have been used in discrete-variable systems; treat this as a procedure
  tailored to this pipeline, not a claim of priority.
