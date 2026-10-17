### Formal security analysis of the post-processing chain

**Status**: formal analysis, companion to `doc/finite_key_procedure.md` and `doc/key_management.md`. The finite-key document fixes the privacy-amplification parameters and cites the bound they rest on; the key management document specifies the protocols between nodes and applications. This document proves the security properties of the key store, the relay and key delivery, and states how they compose with the finite-key guarantee into one end-to-end claim.

Every result below is either proved in full or reduced to a named, cited result. Where a claim is checked by a test rather than proved, that is said explicitly; a test is evidence that an implementation matches its specification, never a substitute for the specification's proof. Results are referred to by name.

### Notation and definitions

| Symbol | Meaning |
|---|---|
| $n$ | number of bits in a block before privacy amplification |
| $l$ | number of bits in a block's distilled key |
| $\varphi$ | the block's phase-error bound (Phase-error estimation in `doc/finite_key_procedure.md`) |
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

This extends, without replacing, the finite-key protocol's threat model; What this does not resolve in `doc/finite_key_procedure.md` lists what stays open on the QKD side (authentication of the classical channel between the two sites of a link, detector assumptions). Those items are carried forward here, not re-examined.

### Security of the distilled key (privacy amplification)

**Theorem (finite-key security)**: for a block of $n$ sifted, error-corrected bits with phase-error bound $\varphi$, disclosed bits $\mathrm{leakedBits}$ and parameters $(\varepsilon_{\mathrm{cor}}, \varepsilon_{\mathrm{PA}})$, the key $K$ of length:

$$l = \mathrm{FiniteKeySecureKeyLength}(n, \varphi, \mathrm{leakedBits}, \varepsilon_{\mathrm{cor}}, \varepsilon_{\mathrm{PA}})$$

output by Toeplitz hashing satisfies: with probability at least $1 - \varepsilon_{\mathrm{cor}}$ the two ends' keys are identical, and conditioned on that event, $d(K, E;\, U_l, E) \le \varepsilon_{\mathrm{PA}}$ for an adversary holding side information $E$.

*Proof.* This is the result of Tomamichel, Lim, Gisin and Renner, *Tight finite-key analysis for quantum cryptography*, Nat. Commun. 3, 634 (2012), applied through the source-replacement scheme to the entanglement-based (BBM92) setting. The Phase-error estimation and Privacy amplification sections of `doc/finite_key_procedure.md` state the exact formula the code implements and its correspondence to that paper. That document flags the BB84 --> BBM92 adaptation as a step a specialist should confirm; this analysis inherits the caveat and does not resolve it. $\blacksquare$

**Lemma (the $\varepsilon_{\mathrm{PA}}$ term is a leftover hash bound)** --> The term:

$$l = k - 2\log_2\!\frac{1}{2\varepsilon_{\mathrm{PA}}}, \qquad
k := n\bigl(1-h_2(\varphi)\bigr) - \mathrm{leakedBits} - \log_2\!\frac{2}{\varepsilon_{\mathrm{cor}}}$$

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

**Remark (entropy-rate and correctness terms)**: $n(1-h_2(\varphi))$ term and the $\log_2(2/\varepsilon_{\mathrm{cor}})$ term come from the cited paper's full chain-rule argument, cited here as a unit. The Error verification and correctness and Phase-error estimation sections of `doc/finite_key_procedure.md` fix how $\varphi$ and $\mathrm{leakedBits}$ must be computed for the bound to apply; using the same-basis QBER in place of the conjugate-basis bound $\varphi$ would be a misuse of the theorem, not a variant of it.

**Remark**: the bound needs $\varphi < \tfrac12$ --> $1 - h_2(\varphi)$ is a valid entropy rate only for $\varphi < \tfrac12$; at $\varphi = 1$ it would equal 1 and certify the whole block. `EstimatePhaseError` caps $\varphi$ at $\tfrac12$ and `FiniteKeySecureKeyLength` returns 0 for $\varphi \ge \tfrac12$ (`TestFiniteKeyNoKeyAtOrAboveHalf`).
