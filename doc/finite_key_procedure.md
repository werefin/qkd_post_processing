### Finite-key post-processing procedure

**Status**: formal specification of the post-processing this codebase implements, from sifted detections to a key that is composably secure for finite blocks, written against the recommendations of `references/bbm92_post_processing_open_questions_v3.pdf` ("the review"). Every quantity below is the one the code computes, and Correspondence with the code names where. The protocol is BBM92: a source between the two sites sends one photon of each entangled pair to each, and both measure in a basis chosen at random. Its security argument is that of Tomamichel and Leverrier, *A largely self-contained and complete security proof for quantum key distribution*, Quantum 1, 14 (2017), arXiv:1506.08458, proved for exactly this entanglement-based setting, with no source assumption and no passage through prepare-and-measure BB84; its key length coincides with that of Tomamichel, Lim, Gisin and Renner, Nat. Commun. 3, 634 (2012), cited below for the same terms. Assumptions and open items lists what is checked against them and what is still open.

### Notation

| Symbol | Meaning |
|---|---|
| $n_\mathrm{sift}$ | sifted bit pairs of the block: coincidences inside the window, measured in the same basis |
| $b \in \{\mathrm{HV}, \mathrm{DA}\}$, $\bar b$ | a basis and the other one |
| $S$, $R$ | the sacrificed sample and the remaining bits, $\lvert S \rvert + \lvert R \rvert = n_\mathrm{sift}$; $S = \emptyset$ when errors are counted by correction |
| $n_b$, $k_b$, $e_b$ | bits of basis $b$ in $R$; in the estimation set of basis $b$; errors among those |
| $n_R$ | $n_\mathrm{HV} + n_\mathrm{DA}$, the bits of $R$ |
| $\mathrm{leak}_\mathrm{EC}$ | every bit disclosed by error correction, its CRC chunk checks included |
| $t$ | verification tag length, $\lceil \log_2(1/\varepsilon_\mathrm{cor}) \rceil$ |
| $n_\mathrm{surv}$ | bits of $R$ in chunks whose CRCs matched |
| $\varphi$ | upper bound on the phase-error rate of the $n_\mathrm{surv}$ bits |
| $h_2$ | binary entropy, $h_2(x) = -x\log_2 x - (1-x)\log_2(1-x)$ |

### Security definition and parameters

A block's output is $\varepsilon_\mathrm{cor}$-correct if the two ends' keys differ with probability at most $\varepsilon_\mathrm{cor}$, and $\varepsilon_\mathrm{sec}$-secret if $(1 - p_\mathrm{abort})\,\tfrac12\lVert \rho_{KE} - \omega_K \otimes \rho_E \rVert_1 \le \varepsilon_\mathrm{sec}$, with $\omega_K$ the uniform key and $E$ everything an adversary holds; the block is then $(\varepsilon_\mathrm{cor} + \varepsilon_\mathrm{sec})$-secure in the composable sense, and blocks compose by adding their $\varepsilon$.

The secrecy budget splits as $\varepsilon_\mathrm{sec} = 2\varepsilon_\mathrm{PE} + \varepsilon_\mathrm{PA}$: the phase-error estimate fails with probability at most $\varepsilon_\mathrm{PE}$ ($\varepsilon_\mathrm{PE}/2$ for each basis, by a union bound) and enters twice through the smoothing of the min-entropy; the leftover hash fails with probability at most $\varepsilon_\mathrm{PA}$.

| Parameter | Default | Code |
|---|---|---|
| $\varepsilon_\mathrm{sec}$ | $10^{-10}$ per block | `FiniteKeyConfig.EpsSec`, CLI `-eps-sec` |
| $\varepsilon_\mathrm{cor}$ | $10^{-15}$ per block | `FiniteKeyConfig.EpsCor`, CLI `-eps-cor` |
| $\varepsilon_\mathrm{PE}$ | $\varepsilon_\mathrm{sec}/3$ | `SplitSecurityBudget` |
| $\varepsilon_\mathrm{PA}$ | $\varepsilon_\mathrm{sec}/3$ | `SplitSecurityBudget` |

These are starting values: choose them for the application, explicitly, and record them with the keys.

### Protocol

One privacy-amplification block runs the stages below in order; every message between the ends is public and authenticated (Assumptions and open items). A recording, a set of pooled recordings (`-dir a,b,c`, all included, none selected by its error rate) or a continuous-mode block is one block.

**Sifting**: coincidences are matched from public metadata only, timestamp, basis and multi-photon flag (`MatchCoincidences`). The clock drift is tracked and removed and the window set to the half width at half maximum of a Gaussian fit to the drift-corrected peak, from public timing alone (`FilterCoincidences`, `doc/coincidence_filtering.md`). Pairs measured in different bases are discarded. Fine timestamps are announced one way only, Alice to Bob, so that the offset $\Delta t$ of a pair is never public. Output: $n_\mathrm{sift}$ bit pairs with their bases.

**Choice of the estimation set**: one of two modes, fixed before any bit is examined.

- **Sacrificed sample** (5G NR LDPC, polar, LDPC, Winnow, the two-party protocol, and Cascade with `-cascade-sample`): a share $s$ of the sifted pairs, chosen by `OptimalSampleFraction` from $n_\mathrm{sift}$ and a declared expected QBER only, is selected with a public seed (`siftSampleOrder`), both ends reveal those bits, and they are discarded. The estimation set of basis $b$ is the sample's bits of basis $b$, $k_b$ of them with $e_b$ errors; $R$ is the rest.
- **Errors counted by correction** (Cascade's default): nothing is revealed or discarded, $R$ is every sifted pair, and the estimation set of basis $b$ is all of $R$'s bits of basis $b$, whose errors are counted after verification (Parameter estimation).

**Error correction**: Bob's bits of $R$ are corrected toward Alice's (Error correction methods), and every bit either end discloses is counted exactly in $\mathrm{leak}_\mathrm{EC}$. The corrected strings are cut into chunks of $c$ bits (2,048 by default, CLI `-chunk`) and each chunk's CRC-16 is compared; a chunk whose CRCs differ is dropped, and each kept chunk's 16 bits join $\mathrm{leak}_\mathrm{EC}$ (`CRCVerify`). Output: $n_\mathrm{surv}$ bits on each side.

**Verification**: Alice sends a $t$-bit Toeplitz hash of her $n_\mathrm{surv}$ bits under a fresh public seed independent of any other, and Bob compares it with his own. A mismatch aborts the block with no key; a match bounds the probability that unequal strings pass by $2^{-t} \le \varepsilon_\mathrm{cor}$ (`distillFiniteKey`, `universalhash.go`).

**Parameter estimation**: for each basis $b$ the phase errors of its $n_b$ bits in $R$ are bounded from the bit errors of the estimation set of basis $\bar b$, because which basis a pair is measured in is chosen at random, so the estimation set of $\bar b$ is a uniformly random subset of all positions with respect to them. The bound is the exact hypergeometric one (`HypergeomUpperRate`): $\bar q(n, k, e, \varepsilon)$ is the largest rate $q$ for which $n q$ errors among $n$ unsampled bits are consistent with $e$ errors in a uniformly drawn $k$,

$$\bar q(n, k, e, \varepsilon) = \max\Bigl\{ \tfrac{m}{n} : \Pr\nolimits_{X \sim \mathrm{Hyp}(n + k,\; e + m,\; k)}\bigl[X \le e\bigr] \ge \varepsilon \Bigr\},$$

never looser than the Serfling term of the review and 30-40% tighter at these sizes (`TestHypergeomUpperRateTighterThanSerfling`, `TestHypergeomUpperRateCoverage`). Then

$$\varphi_0 = \sum_{b} \frac{n_b}{n_R} \min\!\Bigl(\tfrac12,\ \bar q\bigl(n_b,\ k_{\bar b},\ e_{\bar b},\ \tfrac{\varepsilon_\mathrm{PE}^2}{2}\bigr)\Bigr), \qquad \varphi = \min\!\Bigl(\tfrac12,\ \varphi_0 \cdot \frac{n_R}{n_\mathrm{surv}}\Bigr),$$

The bound may fail with probability $\varepsilon_\mathrm{PE}^2$, half per basis, not $\varepsilon_\mathrm{PE}$: the estimate enters the key through a smoothing parameter equal to the square root of its failure probability (Tomamichel et al. 2012, Supplementary Eqs. (S14) and (S15); Tomamichel and Leverrier 2017, Eq. (81)), and that square root is what $2\varepsilon_\mathrm{PE}$ in $\varepsilon_\mathrm{sec}$ counts (`estimationTail`). In the formula above, the factor $n_R / n_\mathrm{surv}$ covers the CRC stage having dropped chunks after seeing their errors, since all phase errors of $R$ may lie in the kept bits (`crossBasisPhaseError`). With errors counted by correction, $k_{\bar b} = n_{\bar b}$ and $e_{\bar b}$ counts, in each verified chunk, the positions where Bob's bit before correction differed from Alice's; a dropped chunk's errors are not known, so every one of its bits counts as an error (`correctionSample`). The per-basis error counts are announced as public parameters, as a sample's are. A basis with an empty estimation set gives $\varphi = \tfrac12$.

**Privacy amplification**: if the block verified and $\varphi < \tfrac12$, the key length is

$$\ell = \Bigl\lfloor n_\mathrm{surv}\bigl(1 - h_2(\varphi)\bigr) - \mathrm{leak}_\mathrm{EC} - \log_2\frac{2}{\varepsilon_\mathrm{cor}} - 2\log_2\frac{1}{2\varepsilon_\mathrm{PA}} \Bigr\rfloor^+,$$

With errors counted by correction, each basis's bound fails with probability $(\varepsilon_\mathrm{PE}/4)^2$ instead of $\varepsilon_\mathrm{PE}^2/2$, and $f(\varepsilon_\mathrm{PE}/4) = \log_2\bigl(2/(1 - \sqrt{1 - (\varepsilon_\mathrm{PE}/4)^2})\bigr)$, 76 bits at $\varepsilon_\mathrm{sec} = 10^{-10}$, joins $\mathrm{leak}_\mathrm{EC}$: the cost of the chain rule that makes the mode secure (`chainRuleBits`, Theorem (errors counted by correction) in `doc/security_proof.md`). Otherwise $\ell = 0$ when the block did not verify or $\varphi \ge \tfrac12$. The term $\log_2(2/\varepsilon_\mathrm{cor}) \ge t$ is the verification tag's cost, so the tag is not charged again in $\mathrm{leak}_\mathrm{EC}$ (`TestVerificationTagChargedOnce`); the last term is the leftover hash lemma's (Security of the distilled key in `doc/security_proof.md`). The cap $\varphi < \tfrac12$ matters: $h_2$ falls again past $\tfrac12$, and at $\varphi = 1$ the rate $1 - h_2(\varphi)$ would certify the whole block (`TestFiniteKeyNoKeyAtOrAboveHalf`). Alice's surviving bits are hashed to $\ell$ bits by a Toeplitz matrix from a fresh public seed (`FiniteKeySecureKeyLength`, `toeplitzHashFast`); when the key goes to the key management system, $\ell$ is rounded down to a multiple of 256 bits, its record size.

**Output**: both ends hold the same $\ell$-bit key, which the key management system stores in the link's pool under shared key_IDs (`doc/key_management.md`). The block's report records $n_\mathrm{sift}$, $n_\mathrm{surv}$, the estimation set, $\varphi$, every disclosed bit with the tag, $t$, and $\ell$ (`FiniteKeyReport`).

**Asymptotic mode** (CLI `-asymptotic`, and always for block discard, which has no estimation set): the key is sized as $n_\mathrm{surv}(1 - h_2(\mathrm{QBER})) - \mathrm{leak}_\mathrm{EC}$, error correction charged once. It carries no finite-size guarantee and is for comparison only.

### Error correction methods

Each method's $\mathrm{leak}_\mathrm{EC}$ is the exact count of what it disclosed (`doc/ec_comparison.md`):

- **Cascade** (`-cascade`, `qkd/cascade.go`), where both strings are on one machine: every parity Alice discloses, each counted once; parities inferred from known ones cost nothing. $f \approx 1.03$-$1.08$ across 1-11% QBER. With no sample, it plans its blocks from the QBER the coincidence filter predicts from timing, which affects only how much it discloses.
- **5G NR LDPC** (`-ldpc5g`), the one-way method for two nodes: the syndrome rows, a 32-bit tag per frame and one bit per verdict of Bob's, less punctured values still hidden where their columns are linearly independent.
- **Polar** (`-polar`): the final frozen set plus 33 bits per attempt, the tag and Bob's verdict.
- **LDPC** (`-ldpc`) and **Winnow** (`-winnow`): the syndromes, or the parities and the syndromes of mismatched blocks.

### Block size

Each block pays $\log_2(2/\varepsilon_\mathrm{cor}) + 2\log_2(1/(2\varepsilon_\mathrm{PA}))$, about 116 bits, and the estimation's fluctuation, so short blocks give little. Predicted key for an illustrative link of 300 sifted pairs per second at 6% QBER, with no recording involved: two-party with a sacrificed sample and $f = 1.2$, against Cascade at $f = 1.05$ with errors counted by correction.

| Acquisition | Sifted pairs | Sample | $\varphi$ | Key, sacrificed sample | $\varphi$ | Key, counted by correction |
|---|---|---|---|---|---|---|
| 10 s | 3,000 | --> | 0.500 | 0 bits | 0.131 | 172 bits (17.2 bit/s) |
| 1 min | 18,000 | 28% | 0.103 | 1,539 bits (25.6 bit/s) | 0.086 | 4,100 bits (68.3 bit/s) |
| 5 min | 90,000 | 15% | 0.083 | 14,759 bits (49.2 bit/s) | 0.071 | 25,692 bits (85.6 bit/s) |
| 10 min | 180,000 | 12% | 0.077 | 33,741 bits (56.2 bit/s) | 0.068 | 53,721 bits (89.5 bit/s) |
| 30 min | 540,000 | 8% | 0.072 | 116,311 bits (64.6 bit/s) | 0.064 | 168,184 bits (93.4 bit/s) |
| 60 min | 1,080,000 | 6% | 0.069 | 246,753 bits (68.5 bit/s) | 0.063 | 341,899 bits (95.0 bit/s) |

The review recommends blocks of $10^6$-$10^7$ sifted bits, an hour or more at these rates. Continuous QKD distils 5-minute blocks by default (`acquisition/continuous_qkd.py`) as the trade between latency and rate: with errors counted by correction they reach about 90% of the hour-long rate, with a sacrificed sample about 70%.

### Correspondence with the code

| Specification | Code |
|---|---|
| $\varepsilon$ budget | `SplitSecurityBudget`, `FiniteKeyConfig` |
| Sifting | `MatchCoincidences`, `FilterCoincidences`, `Sift` |
| Sacrificed sample | `OptimalSampleFraction`, `siftSample` |
| Errors counted by correction | `correctionSample`, `CascadeRunConfig.SacrificeSample` |
| Error correction and $\mathrm{leak}_\mathrm{EC}$ | `RunCascade`, `RunLDPC5G`, `RunPolar`, `RunLDPC`, `RunWinnow`, `CRCVerify` |
| Verification | `distillFiniteKey`, `UniversalHashTagBits`, `VerifyUniversalHash` |
| $\bar q$, $\varphi$ | `HypergeomUpperRate`, `crossBasisPhaseError` |
| $f(\varepsilon_\mathrm{PE}/4)$, errors counted by correction | `chainRuleBits`, `FiniteKeyReport.ChainBits`, `chain_rule_bits` in `importtt -json` |
| $\ell$ | `FiniteKeySecureKeyLength` |
| Two-party, each end on its own recording | `RunAlice`, `RunBob` (`qkd/twoparty.go`) |

### Changes in this revision

- **Verification tag charged once**: the tag was charged both in $\mathrm{leak}_\mathrm{EC}$ and through $\log_2(2/\varepsilon_\mathrm{cor})$, which is the tag's cost in the formula; it is now charged once, about 51 more key bits per block in every mode.
- **Composition stated**: $\varepsilon_\mathrm{sec} = 2\varepsilon_\mathrm{PE} + \varepsilon_\mathrm{PA}$, which the code always used, is now stated here and in the theorem of `doc/security_proof.md`, which bounded secrecy by $\varepsilon_\mathrm{PA}$ alone.
- **Estimation failure squared**: the sampling bound failed with probability $\varepsilon_\mathrm{PE}$ where the composition $\varepsilon_\mathrm{sec} = 2\varepsilon_\mathrm{PE} + \varepsilon_\mathrm{PA}$ needs $\varepsilon_\mathrm{PE}^2$ (Tomamichel et al. 2012, (S14)-(S15)); the secrecy actually reached was near $2\sqrt{\varepsilon_\mathrm{PE}}$, about $10^{-5}$ for a target of $10^{-10}$. The bound now fails with probability $\varepsilon_\mathrm{PE}^2$, which widens the phase-error bound by about $\sqrt 2$ in its fluctuation term.
- **Errors counted by correction** is specified as the second estimation mode, Cascade's default, and proved by the chain rule over the two bases at a cost of 76 bits and a smaller smoothing per estimate; it still gives 1.2 to 2.3 times the key of a sacrificed sample (`TestCorrectionBeatsSampleAcrossSizes`).
- **Block size**: the review's recommendation and the 5-minute default of continuous QKD are reconciled, and the table covers both modes.
- **Results measured on recordings** were removed: results from real data stay with the data, never in this repository.

### Assumptions and open items

- **Checked against the literature**: the key length matches Tomamichel et al. 2012, Supplementary Theorem 2, Eq. (S3), with $q = 1$, and Tomamichel and Leverrier 2017 (arXiv:1506.08458), Theorems 2 and 3, Eqs. (56)-(58), which prove it for entanglement-based protocols directly; $\log_2(2/\varepsilon_\mathrm{cor})$ is at most one bit more than their tag cost $t$. The composition $\varepsilon_\mathrm{sec} = 2\varepsilon_\mathrm{PE} + \varepsilon_\mathrm{PA}$ holds with the estimation failing with probability $\varepsilon_\mathrm{PE}^2$ (Parameter estimation). The exact hypergeometric bound is at least as tight as the sampling bound of Lim, Xu, Pan and Ekert 2021 (arXiv:2009.04882, Lemma 2), which relaxes the same tail.
- **Both bases as key, with a sacrificed sample**: each basis's phase errors bounded from the other basis's sample follows Lo, Chau and Ardehali (quant-ph/0011056, Section 4); with independent random basis choices the sample of one basis is a uniformly random subset with respect to the key bits of the other, so the sampling bound applies, and the factor $n_R / n_\mathrm{surv}$ is a valid way to cover the CRC stage's drops.
- **Estimation by correction**: proved in `doc/security_proof.md`, Theorem (errors counted by correction), by the chain rule of Vitanov et al. (arXiv:1205.5231, Theorem 13), splitting the key's entropy into each basis's key given the other basis's rounds, so that each term is Tomamichel and Leverrier's protocol with the other basis as its test set and no bit is both key and test. It costs $f(\varepsilon_\mathrm{PE}/4) = 76$ bits and estimates failing with probability $(\varepsilon_\mathrm{PE}/4)^2$ each, and adds $\varepsilon_\mathrm{cor}$ to the secrecy for the counts being exact only when correction succeeded. Kiktenko et al. (arXiv:1603.08387, arXiv:1612.03673) estimate from the corrections in practice without a proof. The mode is used with Cascade only, whose corrections are public; with LDPC the counts would add $\lceil\log_2(n_b + 1)\rceil$ bits per basis to $\mathrm{leak}_\mathrm{EC}$.
- **Fixed-length proof, variable-length use**: Tomamichel and Leverrier prove a protocol that aborts above a fixed error threshold; this code sets the key length from the observed errors. Variable-length protocols are treated by Tupkary, Tan and Lütkenhaus (arXiv:2311.01600), whose conditions the per-block bound above is meant to meet; this is not yet checked line by line.
- **Stopping rule**: a block ends after a fixed time or data volume chosen in advance, never after a number of detections in one basis, which Pfister et al. (arXiv:1506.07502) show breaks the random-sampling assumption.
- **Basis-independent detection**: the proof needs detection probabilities that do not depend on the basis (Tomamichel and Leverrier 2017, Part II, Lemma 16); detection-efficiency mismatch between the bases is an assumption on the hardware (Detector assumptions).
- **Bit and phase errors** are not assumed independent; whether they may be for this detector and source, allowing a less conservative estimate, is open in the review.
- **Detector assumptions**: double clicks, dead time and detection-efficiency mismatch are assumptions of the proof, not parameters of post-processing; efficiency asymmetry between detector pairs shows in the per-channel coincidence counts and should be assessed against the proof.
- **Timing side channel**: per-detector delays of tens of picoseconds make an announced $\Delta t$ carry information about the bit; calibration (`-calibrate-delays`, `-detector-delays`) lowers it but not to zero, so timestamps are announced one way only and $\Delta t$ is never public (`doc/coincidence_filtering.md`).
- **Authentication of the classical channel**: sifting, reconciliation and verification messages must be authenticated; between nodes they travel over the key management system's mutual TLS, which is computational and only has to hold while the protocol runs (`doc/security_proof.md`).
