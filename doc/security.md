## Security and finite-key procedure

What a key from this system is proven to be, how each block's key length is computed, and what the key management system adds on top. The protocol is BBM92: a source between the two sites sends one photon of each entangled pair to each, and both measure in a randomly chosen basis. Its finite-key security is that of Tomamichel and Leverrier, Quantum 1, 14 (2017), arXiv:1506.08458, proved for this entanglement-based setting with no source assumption; its key length is the one of Tomamichel, Lim, Gisin and Renner, Nat. Commun. 3, 634 (2012).

### What is proven

| Property | Guarantee | Type |
|---|---|---|
| Both ends hold the same key | except with probability $\varepsilon_\mathrm{cor}$, plus $H\varepsilon_\mathrm{forge}$ if relayed over $H$ hops | information-theoretic |
| Secrecy against an eavesdropper on any link | within $\varepsilon_\mathrm{sec} + H\varepsilon_\mathrm{forge}$ of an ideal key | information-theoretic |
| Every delivered key, direct or relayed | QKD output, $\varepsilon_\mathrm{sec}$-close to uniform | information-theoretic |
| Only the reserved application pair receives a key, once | holds | computational (certificates, TLS 1.3) |
| A key waiting in a store | AES-256-GCM under the node's master key | computational |
| Secrecy against a compromised node | **not claimed** | trusted-node model |

**Adversary**: full access to the quantum channel and to every classical message; read, drop, delay, replay or modify anything on any network link; her own registered applications. She does not control a node (its process, memory, master key or node key), hold another application's or the CA's private key, or break AES-256, SHA-256, ECDSA P-256 or TLS 1.3, which only the computational rows need.

### Parameters

A block is $\varepsilon_\mathrm{cor}$-correct if the two keys differ with probability at most $\varepsilon_\mathrm{cor}$, and $\varepsilon_\mathrm{sec}$-secret if $(1 - p_\mathrm{abort})\tfrac12\lVert \rho_{KE} - \omega_K \otimes \rho_E \rVert_1 \le \varepsilon_\mathrm{sec}$; it is then $(\varepsilon_\mathrm{cor} + \varepsilon_\mathrm{sec})$-secure and blocks compose by adding their $\varepsilon$.

| Parameter | Default | Meaning |
|---|---|---|
| $\varepsilon_\mathrm{sec}$ | $10^{-10}$ per block | secrecy, $= 2\varepsilon_\mathrm{PE} + \varepsilon_\mathrm{PA}$ (`-eps-sec`) |
| $\varepsilon_\mathrm{cor}$ | $10^{-15}$ per block | correctness (`-eps-cor`) |
| $\varepsilon_\mathrm{PE}$, $\varepsilon_\mathrm{PA}$ | $\varepsilon_\mathrm{sec}/3$ each | estimation smoothing, leftover hash (`SplitSecurityBudget`) |

These are starting values: choose them for the application and record them with the keys.

### One block, stage by stage

A recording, a set of pooled recordings (`-dir a,b,c`, none selected by its error rate) or a continuous-mode block is one block. Every classical message is public and authenticated.

**Sifting**: coincidences are matched from public timing, basis and multi-photon flag only; drift is removed and the window set from the peak's half width (`doc/coincidence_filtering.md`); pairs in different bases are dropped. Timestamps are announced one way only, Alice to Bob, so a pair's offset $\Delta t$ is never public. Output: $n_\mathrm{sift}$ bit pairs.

**Estimation set**, fixed before any bit is seen: either a **sacrificed sample** (LDPC, polar, Winnow, Cascade with `-cascade-sample`), a share chosen from $n_\mathrm{sift}$ and a declared QBER only, picked by a public seed, revealed and discarded; or **errors counted by correction** (Cascade's default), where nothing is revealed and every remaining bit counts.

**Error correction**: Bob's bits are corrected toward Alice's and every disclosed bit is counted in $\mathrm{leak}_\mathrm{EC}$. The strings are cut into chunks with a CRC-16 each; a mismatched chunk is dropped and each kept chunk's 16 bits join $\mathrm{leak}_\mathrm{EC}$. Output: $n_\mathrm{surv}$ bits.

**Verification**: Alice sends a $t = \lceil\log_2(1/\varepsilon_\mathrm{cor})\rceil$-bit Toeplitz hash under a fresh public seed; a mismatch aborts the block, a match lets unequal strings through with probability at most $2^{-t} \le \varepsilon_\mathrm{cor}$.

**Phase-error estimate**: the basis of each pair is chosen at random, so the estimation set of one basis is a uniformly random subset with respect to the other basis's key bits. For basis $b$ with $n_b$ remaining bits, the other basis's $k_{\bar b}$ estimation bits with $e_{\bar b}$ errors bound its phase errors by the exact hypergeometric tail (`HypergeomUpperRate`):

$$\bar q(n, k, e, \varepsilon) = \max\Bigl\{ \tfrac{m}{n} : \Pr\nolimits_{X \sim \mathrm{Hyp}(n+k,\, e+m,\, k)}[X \le e] \ge \varepsilon \Bigr\},$$

$$\varphi = \min\Bigl(\tfrac12,\ \frac{n_R}{n_\mathrm{surv}} \sum_b \frac{n_b}{n_R} \min\bigl(\tfrac12,\ \bar q(n_b, k_{\bar b}, e_{\bar b}, \tfrac{\varepsilon_\mathrm{PE}^2}{2})\bigr)\Bigr).$$

The bound fails with probability $\varepsilon_\mathrm{PE}^2$ in total, not $\varepsilon_\mathrm{PE}$: it enters the key through a smoothing parameter equal to the square root of that probability, and $2\varepsilon_\mathrm{PE}$ in $\varepsilon_\mathrm{sec}$ counts that square root (Tomamichel et al. 2012, Eqs. (S14)-(S15); Tomamichel and Leverrier 2017, Eq. (81)). The factor $n_R/n_\mathrm{surv}$ covers the CRC stage having dropped chunks after seeing their errors. The hypergeometric tail is 30-40% tighter than the Serfling bound at these sizes and never looser.

**Key length**: if the block verified and $\varphi < \tfrac12$,

$$\ell = \Bigl\lfloor n_\mathrm{surv}\bigl(1 - h_2(\varphi)\bigr) - \mathrm{leak}_\mathrm{EC} - \log_2\frac{2}{\varepsilon_\mathrm{cor}} - 2\log_2\frac{1}{2\varepsilon_\mathrm{PA}} \Bigr\rfloor^+,$$

else $\ell = 0$. The third term is the verification tag's cost, so the tag is not charged again in $\mathrm{leak}_\mathrm{EC}$; the last is the leftover hash lemma's (below). The cap $\varphi < \tfrac12$ matters, since $1 - h_2$ rises again past $\tfrac12$. Alice's surviving bits are hashed to $\ell$ bits by a Toeplitz matrix from a fresh public seed, and $\ell$ is rounded down to whole 256-bit records for the key management system.

**Asymptotic mode** (`-asymptotic`, and always for block discard): $n_\mathrm{surv}(1 - h_2(\mathrm{QBER})) - \mathrm{leak}_\mathrm{EC}$, for comparison only, with no finite-size guarantee.

### Why the key length is secure

**Theorem (finite-key security)**: with $\varphi$ failing with probability at most $\varepsilon_\mathrm{PE}^2$, the key of length $\ell$ is $\varepsilon_\mathrm{cor}$-correct and $\varepsilon_\mathrm{sec}$-secret with $\varepsilon_\mathrm{sec} = 2\varepsilon_\mathrm{PE} + \varepsilon_\mathrm{PA}$.

*Proof.* Tomamichel and Leverrier 2017, Theorems 2 and 3, Eqs. (56)-(58), for the entanglement-based protocol with mutually unbiased bases, give $n(1 - h_2(\varphi)) - \mathrm{leak} - t - 2\log_2(1/(2\varepsilon_\mathrm{PA}))$, which $\ell$ meets since $\log_2(2/\varepsilon_\mathrm{cor}) \ge t$; the same terms are Supplementary Theorem 2 of Tomamichel et al. 2012.

**Leftover hash term**: for a 2-universal family to $l$ bits, a uniform independent seed and $H_\mathrm{min}(X \mid E) \ge k$, the output is within $\tfrac12\sqrt{2^{\,l-k}}$ of uniform; setting this to $\varepsilon_\mathrm{PA}$ gives $l = k - 2\log_2(1/(2\varepsilon_\mathrm{PA}))$. Toeplitz matrices are 2-universal (Krawczyk 1994); the fast POPCNT and CLMUL paths are checked bit for bit against the defining implementation.

### Errors counted by correction

Cascade's default sacrifices nothing: after correction and verification Bob announces, per basis, how many bits he corrected, and those counts bound the other basis's phase errors.

**Theorem**: with $\bar\varepsilon = \varepsilon_\mathrm{PE}/4$, each basis's bound failing with probability at most $\bar\varepsilon^2$, and $f(\bar\varepsilon) = \log_2\bigl(2/(1 - \sqrt{1 - \bar\varepsilon^2})\bigr)$ added to $\mathrm{leak}_\mathrm{EC}$ (76 bits at $\varepsilon_\mathrm{sec} = 10^{-10}$), the key is $\varepsilon_\mathrm{cor}$-correct and $(\varepsilon_\mathrm{sec} + \varepsilon_\mathrm{cor})$-secret.

*Proof.* Write $K = (K_Z, K_X)$ for Alice's surviving bits of the two bases, $Y$ for Bob's and $E$ for Eve's information.

- **Chain rule** (Vitanov, Dupuis, Tomamichel and Renner, arXiv:1205.5231, Theorem 13): $H_\mathrm{min}^{4\bar\varepsilon}(K_Z K_X \mid E) \ge H_\mathrm{min}^{\bar\varepsilon}(K_Z \mid K_X E) + H_\mathrm{min}^{\bar\varepsilon}(K_X \mid E) - f(\bar\varepsilon)$.
- **Each term is the standard protocol**: conditioning on more lowers min-entropy, so $H_\mathrm{min}^{\bar\varepsilon}(K_Z \mid K_X E) \ge H_\mathrm{min}^{\bar\varepsilon}(K_Z \mid K_X Y_X E)$, the Z key with every X round given to Eve: Tomamichel and Leverrier's protocol with the X rounds as its test set, giving $n_Z(1 - h_2(\varphi_Z))$; symmetrically for X. No bit is both key and test.
- **Joining**: $h_2$ is concave, so the two terms are at least $n(1 - h_2(\varphi))$ with the weighted $\varphi$ above; disclosure, tag and the leftover hash then follow as in the finite-key theorem.
- **The counts are the true errors** whenever Bob's corrected string equals Alice's; otherwise the tag passed unequal strings, probability at most $\varepsilon_\mathrm{cor}$, which is added to the secrecy. A dropped chunk's bits all count as errors.
- **The counts disclose nothing new** with Cascade, whose corrected positions follow from the public parities already charged; with LDPC they would cost $\lceil\log_2(n_b+1)\rceil$ bits per basis, so the mode is used with Cascade only.

**What it buys**: 1.2 to 2.3 times the key of a sacrificed sample, and a key where a sample leaves none (`TestCorrectionBeatsSampleAcrossSizes`).

### Block size

Each block pays about 116 bits for the tag and the leftover hash, plus the estimate's fluctuation, so short blocks give little. Predicted key for an illustrative link of 300 sifted pairs per second at 6% QBER: a sacrificed sample with $f = 1.2$ against Cascade at $f = 1.05$ with errors counted by correction.

| Acquisition | Sifted pairs | Sample | $\varphi$ | Key, sample | $\varphi$ | Key, by correction |
|---|---|---|---|---|---|---|
| 10 s | 3,000 | --> | 0.500 | 0 bits | 0.131 | 172 bits (17.2 bit/s) |
| 1 min | 18,000 | 28% | 0.103 | 1,539 bits (25.6 bit/s) | 0.086 | 4,100 bits (68.3 bit/s) |
| 5 min | 90,000 | 15% | 0.083 | 14,759 bits (49.2 bit/s) | 0.071 | 25,692 bits (85.6 bit/s) |
| 10 min | 180,000 | 12% | 0.077 | 33,741 bits (56.2 bit/s) | 0.068 | 53,721 bits (89.5 bit/s) |
| 30 min | 540,000 | 8% | 0.072 | 116,311 bits (64.6 bit/s) | 0.064 | 168,184 bits (93.4 bit/s) |
| 60 min | 1,080,000 | 6% | 0.069 | 246,753 bits (68.5 bit/s) | 0.063 | 341,899 bits (95.0 bit/s) |

Continuous QKD distils 5-minute blocks by default: about 90% of the hour-long rate with errors counted by correction, about 70% with a sacrificed sample.

### Error correction methods

Real post-processing uses **optimized Cascade** only: $\mathrm{leak}_\mathrm{EC}$ is every parity Alice discloses, each once; $f \approx 1.03$-$1.08$ over 1-11% QBER. The legacy methods still in the library count their own disclosure exactly (`doc/ec_comparison.md`): **5G NR LDPC** (`-ldpc5g`), one-way, for two nodes: syndrome rows, a 32-bit tag per frame and Bob's verdicts, less punctured values still hidden. **Polar** (`-polar`): the final frozen set plus 33 bits per attempt. **LDPC** and **Winnow**: their syndromes and parities.

### Two-party post-processing

Each site runs its own half on its own recording (`RunAlice`, `RunBob`, `importtt -role`). The messages carry Alice's announcement (per click timestamp, basis, multi-photon flag), Bob's pair list, sample seeds and bits or Cascade's parities, frame syndromes and tags, CRCs, the verification tag and the privacy-amplification seed, key length and key_IDs; Bob never sends a timestamp. In continuous mode Alice announces segment by segment as each closes, and the rest is unchanged: Bob answers each segment with a bare receipt, keeps for himself the announced clicks that can pair and their indices, and at the block's end sends the same pair list, in the whole announcement's indices, that the whole-block run would; both check they hashed the same announcement (`two_party_stream.py`, `RunAliceClicks`, `TwoPartyConfig.AnnouncementIndex`, `TestTwoPartyReducedAnnouncement`). The disclosure charged is the same as in the joint pipeline (`TestTwoPartyMatchesJoint`, `TestTwoPartyCascade`). The messages travel between the nodes' KMEs over TLS 1.3, computational authentication that only has to hold while the protocol runs: breaking it later does not help against a key already produced.

### Pools: no key used twice

Both ends of link $uv$ hold the same records, so each record has one owner: $u$ originates with a key_ID only if its first hex digit's parity is even and $u < v$, or odd and $u > v$, the exact complement of $v$'s rule. In one store every read and wipe of a record happens in one locked step, and a wiped key_ID can never be stored again. The other end consumes a record of the owner's half only when the owner names it in an authenticated request, and checks every named key before consuming any. Hence every pool record pads one relay message, keys one MAC or forms part of one application key, once in the whole network (`TestBothEndsAtOnce`).

### Relay

**One-time pad**: a uniform pad used once makes $c = m \oplus k$ perfectly secret, since every $m$ is consistent with $c$ through exactly one pad (Shannon 1949). The pad is pool material, uniform up to $\varepsilon_\mathrm{sec}$ and used once.

**Poly1305**: keyed with a uniform single-use 32-byte key it forges with probability $\varepsilon_\mathrm{forge} \le 8\lceil \mathrm{len}(m)/16\rceil / 2^{106}$ (Bernstein 2005, RFC 8439). The key is QKD pool material drawn with the pad, so the MAC is information-theoretic, unlike in TLS where the key comes from a pseudorandom function. Tags are compared in constant time.

**Each hop authenticates, then decrypts**: the tag covers destination, sending hop, origin, key_ID, SAE pair, pad key_IDs and ciphertext, each length-prefixed so the encoding is injective; any change is refused except with probability $\varepsilon_\mathrm{forge}$, and nothing is decrypted before.

**Over $H$ hops**, every intermediate node trusted: an eavesdropper on the links learns nothing about the key, and makes the destination store anything other than what the origin sent with probability at most $H\varepsilon_\mathrm{forge}$. A relayed 256-bit key is at most 15 sixteen-byte blocks, so $\varepsilon_\mathrm{forge} \approx 1.5\times10^{-30}$ per hop, negligible next to $\varepsilon_\mathrm{sec} = 10^{-10}$ for any realistic path. The first hop's key is pool material itself, sent by key_IDs, so every relayed key is QKD output end to end. A compromised intermediate node can read and replace what it relays, as any trusted relay can.

### Key delivery to applications

A key given to master SAE $a$ for slave $b$ is stored reserved for that pair at $b$'s node and returned only to $b$, once, when $b$ names $a$; the slave identity comes from its client certificate, never from the request, and an ID of another pair answers as an unknown one. The master gets its key only after the slave's node confirmed it stored the same key under the same key_ID; otherwise the key is burned. This authorization is computational: it rests on certificates from the network CA and TLS 1.3.

### Storage

Every record is AES-256-GCM under a key derived from the node's master key by HKDF-SHA256, so a stored key is protected computationally and only while the master key stays secret. A delivered or expired key's ciphertext is wiped, leaving a tombstone, so a later theft reveals only keys still waiting. Pipeline output is plaintext only between `importtt` writing it (mode 0600) and the next import pass, a few seconds. The master key is mounted in the KMS container alone.

### Assumptions and open items

- **Fixed-length proof, variable-length use**: Tomamichel and Leverrier prove a protocol with a fixed abort threshold; here the length follows the observed errors, as treated by Tupkary, Tan and Lütkenhaus (arXiv:2311.01600), not yet checked line by line.
- **Stopping rule**: a block ends after a time or data volume fixed in advance, never after a count of detections in one basis (Pfister et al., arXiv:1506.07502).
- **Detectors**: basis-independent detection, double clicks, dead time and efficiency mismatch are assumptions on the hardware, not parameters of post-processing; per-channel coincidence counts show asymmetries to assess.
- **Timing side channel**: per-detector delays make an announced $\Delta t$ carry information about the bit; calibration lowers it, and timestamps are announced one way only (`doc/coincidence_filtering.md`).
- **Bit and phase errors** are not assumed independent.
- **Authentication of post-processing messages** is TLS between the nodes; Poly1305 under pool keys would make it information-theoretic at one pool key per message.
- **Trusted nodes**, custody of master keys and of the CA key, and an implementation audit (side channels, fault injection) are outside what software can prove about itself.

### Correspondence with the code

| Specification | Code |
|---|---|
| $\varepsilon$ budget | `SplitSecurityBudget`, `FiniteKeyConfig` |
| Sifting | `MatchCoincidences`, `FilterCoincidences`, `Sift` |
| Sacrificed sample | `OptimalSampleFraction`, `siftSample` |
| Errors counted by correction | `correctionSample`, `CascadeRunConfig.SacrificeSample`, `chainRuleBits`, `chain_rule_bits` in `importtt -json` |
| Error correction, $\mathrm{leak}_\mathrm{EC}$ | `RunCascade`, `RunLDPC5G`, `RunPolar`, `RunLDPC`, `RunWinnow`, `CRCVerify` |
| Verification | `distillFiniteKey`, `UniversalHashTagBits`, `VerifyUniversalHash` |
| $\bar q$, $\varphi$ | `HypergeomUpperRate`, `crossBasisPhaseError`, `estimationTail` |
| $\ell$ | `FiniteKeySecureKeyLength` |
| Two-party | `RunAlice`, `RunBob` (`qkd/twoparty.go`) |
| Pools, relay, delivery, storage | `OriginatorOf`, `relay/`, `cmd/kms`, `kms/encryption.go` |
