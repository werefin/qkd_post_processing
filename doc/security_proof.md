### Formal security analysis of the post-processing chain

**Status**: formal analysis, companion to `doc/finite_key_procedure.md` and `doc/key_management.md`. The finite-key document fixes the privacy-amplification parameters and cites the bound they rest on; the key management document specifies the protocols between nodes and applications. This document proves the security properties of the key store, the relay and key delivery, and states how they compose with the finite-key guarantee into one end-to-end claim.

Every result below is either proved in full or reduced to a named, cited result. Where a claim is checked by a test rather than proved, that is said explicitly; a test is evidence that an implementation matches its specification, never a substitute for the specification's proof. Results are referred to by name.

### Notation and definitions

| Symbol | Meaning |
|---|---|
| $n$ | number of bits in a block before privacy amplification |
| $l$ | number of bits in a block's distilled key |
| $\varphi$ | the block's phase-error bound (Parameter estimation in `doc/finite_key_procedure.md`) |
| $\varepsilon_{\mathrm{cor}}$ | probability the two ends hold different keys after claiming success |
| $\varepsilon_{\mathrm{sec}}$ | trace distance between the real key, with side information, and an ideal uniform key |
| $\varepsilon_{\mathrm{PA}}$ | the leftover-hash failure probability, part of $\varepsilon_{\mathrm{sec}}$ |
| $d(X;Y)$ | statistical (trace) distance between the distributions of $X$ and $Y$ |

**Definition (statistical distance)**: for random variables $X, Y$ over the same finite set:

$$d(X;Y) = \frac{1}{2}\sum_{z} \bigl|\Pr[X=z] - \Pr[Y=z]\bigr|.$$

Two distributions with $d(X;Y) \le \varepsilon$ are $\varepsilon$-close: no test distinguishes them with advantage above $\varepsilon$.

**Definition (information-theoretic and computational security)**: a guarantee is *information-theoretic* (unconditional) when it holds against an adversary of unbounded computing power, as a bound on a statistical distance or on a probability over the choice of a key. It is *computational* when it holds only against a time-bounded adversary, contingent on an unproven hardness assumption, such as that AES-256, SHA-256 or ECDSA P-256 has no efficient attack. This document says which of the two applies to every claim; conflating them is the most common error in this kind of analysis.

### Threat model

The adversary, Eve, has:

- Full access to the quantum channel and to every classical message of sifting, error correction, verification and parameter estimation (the standard QKD adversary).
- Full read and tamper access to every network link between nodes and between a node and its applications: she can read, drop, delay, reorder, replay or modify any message, including relay packets and reservation requests.
- Applications of her own, registered on any node, with valid application certificates (role `sae`).

Eve does **not** have:

- Control of a node, meaning its KMS process, its memory, its master key or its node key (the **trusted-node assumption**, stated formally under End-to-end composition over a multi-hop path).
- Another application's private key, or the network CA's private key.
- The ability to break AES-256-GCM, SHA-256, HKDF, ECDSA P-256 or TLS 1.3, needed only for the computational claims below, never for an information-theoretic one.

This extends, without replacing, the finite-key protocol's threat model; Assumptions and open items in `doc/finite_key_procedure.md` lists what stays open on the QKD side (authentication of the classical channel between the two sites of a link, detector assumptions). Those items are carried forward here, not re-examined.

### Security of the distilled key (privacy amplification)

**Theorem (finite-key security)**: for a block of $n$ sifted, error-corrected bits whose phase-error bound $\varphi$ holds except with probability $\varepsilon_{\mathrm{PE}}^2$ ($\varepsilon_{\mathrm{PE}}^2/2$ for each basis's estimate), error correction having disclosed $\mathrm{leakedBits}$ bits, and parameters $(\varepsilon_{\mathrm{cor}}, \varepsilon_{\mathrm{PA}})$, the key $K$ of length:

$$l = \mathrm{FiniteKeySecureKeyLength}(n, \varphi, \mathrm{leakedBits}, \varepsilon_{\mathrm{cor}}, \varepsilon_{\mathrm{PA}})$$

output by Toeplitz hashing is $\varepsilon_{\mathrm{cor}}$-correct, the two ends' keys differing with probability at most $\varepsilon_{\mathrm{cor}}$, and $\varepsilon_{\mathrm{sec}}$-secret with $\varepsilon_{\mathrm{sec}} = 2\varepsilon_{\mathrm{PE}} + \varepsilon_{\mathrm{PA}}$, the estimation entering through the smoothing parameter $\varepsilon_{\mathrm{PE}}$, the square root of its failure probability, once for each of the two smoothings: $(1 - p_\mathrm{abort})\, d(K, E;\, U_l, E) \le \varepsilon_{\mathrm{sec}}$ for an adversary holding side information $E$; together the protocol is $(\varepsilon_{\mathrm{cor}} + \varepsilon_{\mathrm{sec}})$-secure.

*Proof.* This is Theorems 2 and 3 of Tomamichel and Leverrier, *A largely self-contained and complete security proof for quantum key distribution*, Quantum 1, 14 (2017), arXiv:1506.08458, Eqs. (56)-(58), which prove it for the entanglement-based protocol itself, BBM92 with a source between the two ends, with no assumption on the source; Eq. (81) there is why the estimate must fail with probability at most $\varepsilon_{\mathrm{PE}}^2$. Solved for the key length with the two bases mutually unbiased, their bound is $n(1 - h_2(\varphi)) - \mathrm{leak} - t - 2\log_2(1/(2\varepsilon_{\mathrm{PA}}))$, which $l$ above meets with $\log_2(2/\varepsilon_{\mathrm{cor}}) \ge t$. The same terms appear in Tomamichel, Lim, Gisin and Renner, Nat. Commun. 3, 634 (2012), Supplementary Theorem 2. The Parameter estimation and Privacy amplification stages of `doc/finite_key_procedure.md` state the exact formula the code implements; its Assumptions and open items list what the theorem assumes (a fixed abort threshold, basis-independent detection) and what is not yet proved (estimation by correction). $\blacksquare$

**Theorem (errors counted by correction)**: when nothing is sacrificed and each basis's errors, counted after error correction and verification, bound the other basis's phase errors, the key of length

$$l = \mathrm{FiniteKeySecureKeyLength}\bigl(n, \varphi, \mathrm{leakedBits} + f(\bar\varepsilon), \varepsilon_{\mathrm{cor}}, \varepsilon_{\mathrm{PA}}\bigr), \qquad \bar\varepsilon = \tfrac{\varepsilon_{\mathrm{PE}}}{4}, \qquad f(\bar\varepsilon) = \log_2\frac{2}{1 - \sqrt{1 - \bar\varepsilon^{2}}} \approx \log_2\frac{4}{\bar\varepsilon^{2}},$$

with each basis's phase-error bound failing with probability at most $\bar\varepsilon^{2}$, is $\varepsilon_{\mathrm{cor}}$-correct and $(\varepsilon_{\mathrm{sec}} + \varepsilon_{\mathrm{cor}})$-secret, $\varepsilon_{\mathrm{sec}} = 2\varepsilon_{\mathrm{PE}} + \varepsilon_{\mathrm{PA}}$ as above. At $\varepsilon_{\mathrm{sec}} = 10^{-10}$, $f(\bar\varepsilon)$ is 76 bits.

*Proof.* Write $K = (K_Z, K_X)$ for Alice's surviving bits of the two bases, $Y$ for Bob's and $E$ for everything Eve holds before error correction; error correction and verification are charged afterwards, as in the theorem above.

- **Chain rule**: by Vitanov, Dupuis, Tomamichel and Renner, *Chain rules for quantum Rényi entropies*, arXiv:1205.5231, Theorem 13, with all three smoothing parameters $\bar\varepsilon$:

$$H_{\min}^{4\bar\varepsilon}(K_Z K_X \mid E) \;\ge\; H_{\min}^{\bar\varepsilon}(K_Z \mid K_X E) + H_{\min}^{\bar\varepsilon}(K_X \mid E) - f(\bar\varepsilon), \qquad 4\bar\varepsilon = \varepsilon_{\mathrm{PE}}.$$

- **Each term is the standard setting**: conditioning on more lowers the min-entropy, so $H_{\min}^{\bar\varepsilon}(K_Z \mid K_X E) \ge H_{\min}^{\bar\varepsilon}(K_Z \mid K_X Y_X E)$, which is the entropy of the Z key when every X round, Alice's and Bob's outcomes alike, is handed to Eve. That is exactly the protocol of Tomamichel and Leverrier (arXiv:1506.08458) with the X rounds as its test set: with independent random basis choices and basis-independent detection, given $n_Z$ and $n_X$, which sifted rounds are X is a uniformly random subset, so their error count bounds the Z rounds' phase errors by the sampling bound, failing with probability at most $\bar\varepsilon^{2}$ (their Eq. (81)), and their uncertainty relation gives $n_Z(1 - h_2(\varphi_Z))$. Symmetrically $H_{\min}^{\bar\varepsilon}(K_X \mid E) \ge H_{\min}^{\bar\varepsilon}(K_X \mid K_Z Y_Z E) \ge n_X(1 - h_2(\varphi_X))$. In neither term is a bit both key and test: the counterfactual measurement each term needs is on its own key rounds only.

- **Joining the bases**: $h_2$ is concave, so $n_Z h_2(\varphi_Z) + n_X h_2(\varphi_X) \le n\, h_2(\varphi)$ with $\varphi$ the weighted bound the code computes, and the CRC stage's drops are covered per basis by the factor $n_R / n_\mathrm{surv}$ as before. Hence $H_{\min}^{\varepsilon_{\mathrm{PE}}}(K \mid E) \ge n(1 - h_2(\varphi)) - f(\bar\varepsilon)$; error correction's disclosure and the tag lower it by at most $\mathrm{leakedBits} + \log_2(2/\varepsilon_{\mathrm{cor}})$, and the leftover hash lemma below completes the bound with secrecy $2\varepsilon_{\mathrm{PE}} + \varepsilon_{\mathrm{PA}}$.

- **The counts are those of the real errors**: the error counts are positions where Bob's bits changed in correction. They equal the true errors whenever Bob's corrected string equals Alice's; otherwise, on a key that is output, the verification tag passed unequal strings, which happens with probability at most $\varepsilon_{\mathrm{cor}}$. The protocol therefore differs from one using the true counts only on an event of probability at most $\varepsilon_{\mathrm{cor}}$, adding $\varepsilon_{\mathrm{cor}}$ to the secrecy. A dropped chunk's bits count as errors, which only raises the bound.

- **The counts disclose nothing more**: with Cascade every corrected position follows from the public parities and binary searches, already charged in $\mathrm{leakedBits}$, so announcing the counts reveals nothing new. With a method whose correction is not public, as LDPC decoding, the counts would add $\lceil\log_2(n_b + 1)\rceil$ bits per basis; the code uses this mode with Cascade only. $\blacksquare$

The theorem is assembled from published results: Tomamichel and Leverrier's theorem applied to each basis, Vitanov et al.'s chain rule and the leftover hash lemma. Its cost, $f(\bar\varepsilon)$ and the smaller smoothing per estimate, is far below the bits a sacrificed sample costs (`TestCorrectionBeatsSampleAcrossSizes`: 1.2 to 2.3 times the key, and a key where a sample leaves none).

**Lemma (the $\varepsilon_{\mathrm{PA}}$ term is a leftover hash bound)** --> The term:

$$l = k - 2\log_2\!\frac{1}{2\varepsilon_{\mathrm{PA}}}, \qquad
k := n\bigl(1-h_2(\varphi)\bigr) - \mathrm{leakedBits} - \log_2\!\frac{2}{\varepsilon_{\mathrm{cor}}}$$

where $\mathrm{leakedBits}$ is error correction's disclosure and $\log_2(2/\varepsilon_{\mathrm{cor}}) \ge \lceil \log_2(1/\varepsilon_{\mathrm{cor}}) \rceil$ is the verification tag's, charged once (`TestVerificationTagChargedOnce`).

inside `FiniteKeySecureKeyLength` is exactly the bound of the Leftover Hash Lemma.

*Proof.* The Leftover Hash Lemma (Impagliazzo, Levin and Luby 1989, in the smooth-entropy form of Renner's and Tomamichel et al.'s QKD proofs) states: if $F$ is a 2-universal family $\{0,1\}^n \to \{0,1\}^l$, $S$ a uniform, independent choice of $f\in F$, and $H_{\min}(X\mid E) \ge k$, then:

$$d\bigl(f_S(X),\, S,\, E \;;\; U_l,\, S,\, E\bigr) \;\le\; \frac{1}{2}\sqrt{2^{\,l-k}}$$

Setting this equal to $\varepsilon_{\mathrm{PA}}$ and solving for $l$:

$$
\begin{aligned}
\frac{1}{2}\sqrt{2^{\,l-k}} &= \varepsilon_{\mathrm{PA}} \\[2pt]
2^{\,l-k} &= 4\,\varepsilon_{\mathrm{PA}}^{2} \\[2pt]
l &= k - 2\log_2\!\frac{1}{2\varepsilon_{\mathrm{PA}}}
\end{aligned}
$$

which is `finitekey.go`'s formula. The seed $S$ is `toeplitzSeed`'s output, and Toeplitz matrices form a 2-universal family (Krawczyk, *LFSR-based hashing and authentication*, CRYPTO 1994), so the lemma's hypothesis holds for the defining implementation `toeplitzHashNaive`. $\blacksquare$

**Remark (fast implementations)**: `toeplitzHashFast` dispatches to a POPCNT or CLMUL implementation chosen for speed, not to a different function. 2-universality holds for the definition; it transfers to the fast paths only because `toeplitz_test.go` and `toeplitz_vectors_test.go` check them bit for bit against it on random and fixed vectors.

**Remark (entropy-rate and correctness terms)**: $n(1-h_2(\varphi))$ term and the $\log_2(2/\varepsilon_{\mathrm{cor}})$ term come from the cited paper's full chain-rule argument, cited here as a unit. The Verification and Parameter estimation stages of `doc/finite_key_procedure.md` fix how $\varphi$ and $\mathrm{leakedBits}$ must be computed for the bound to apply; using the same-basis QBER in place of the conjugate-basis bound $\varphi$ would be a misuse of the theorem, not a variant of it.

**Remark**: the bound needs $\varphi < \tfrac12$ --> $1 - h_2(\varphi)$ is a valid entropy rate only for $\varphi < \tfrac12$; at $\varphi = 1$ it would equal 1 and certify the whole block. `EstimatePhaseError` caps $\varphi$ at $\tfrac12$ and `FiniteKeySecureKeyLength` returns 0 for $\varphi \ge \tfrac12$ (`TestFiniteKeyNoKeyAtOrAboveHalf`).
