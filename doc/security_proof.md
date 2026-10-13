# Formal security analysis of the QKD post-processing and relay pipeline

**Status:** formal analysis, companion to `doc/finite_key_procedure.md`. That
document specifies the finite-key parameter choices and cites the literature
the privacy-amplification bound rests on; it does not address the key
management store (`kms.go`) or the multi-hop relay (`relay.go`,
`polymac.go`). This document proves the security properties of those two
components precisely, and states — rather than asserts — how they compose
with the finite-key guarantee into one end-to-end claim.

Every theorem below is either proved in full or reduced to a named, cited
result. Where a claim is empirically checked rather than proved (e.g. by a
test), that is stated explicitly; an empirical check is evidence that an
implementation matches its specification, never a substitute for the
specification's own proof.

## 0. Notation and definitions

| Symbol | Meaning |
|---|---|
| $n$ | number of bits in a block before privacy amplification |
| $l$ | number of bits in a block's distilled key, after privacy amplification |
| $\varphi$ | the block's estimated phase-error rate (see `doc/finite_key_procedure.md` §2.5) |
| $\varepsilon_{\mathrm{cor}}$ | probability the two sides end with different keys after claiming success |
| $\varepsilon_{\mathrm{sec}}$ | trace distance between the real output key (with side information) and an ideal uniform key |
| $\varepsilon_{\mathrm{PA}}$ | the privacy-amplification (leftover hash) failure probability, part of $\varepsilon_{\mathrm{sec}}$ |
| $\varepsilon_{\mathrm{forge}}$ | probability a one-time authenticator accepts a message it did not legitimately produce |
| $H_{\min}^{\varepsilon}(X\mid E)$ | $\varepsilon$-smooth conditional min-entropy of $X$ given an adversary's information $E$ |
| $d(X;Y)$ | statistical (trace) distance between the distributions of $X$ and $Y$ |

**Definition 1 (statistical distance).** For random variables $X, Y$ over the
same finite set,

$$d(X;Y) = \frac{1}{2}\sum_{z} \bigl|\Pr[X=z] - \Pr[Y=z]\bigr|.$$

Two distributions with $d(X;Y) \le \varepsilon$ are called $\varepsilon$-close;
no statistical test can distinguish them with advantage greater than
$\varepsilon$.

**Definition 2 (information-theoretic vs. computational security).** A
guarantee is *information-theoretic* (equivalently: *unconditional*) when it
holds against an adversary of unbounded computational power, usually
expressed as a bound on a statistical distance or a probability over a random
choice of key. A guarantee is *computational* when it holds only against an
adversary bounded in running time, contingent on an unproven hardness
assumption (e.g. that AES-256 or a hash function has no efficient attack).
This document is precise about which of the two applies to each component;
conflating them is the single most common error in this kind of analysis.

## 1. Threat model

The adversary, Eve, has:

- Full access to the quantum channel and to every classical message
  exchanged during sifting, error correction, error verification, and
  parameter estimation (standard QKD adversary model).
- Full read and tamper access to the classical links between relay nodes
  (`RelayNode.forward` / `RelayNode.Receive`): she can read, drop, delay,
  reorder, duplicate, or modify any `RelayedPacket` in transit.
- No access to the inside of a relay node's memory while it is processing a
  packet, and no access to the KMS master key (`kms.go`) of any node she does
  not control.

Eve does **not** have:

- Control of a relay node on the path (the **trusted-node assumption**,
  stated formally in §6).
- The ability to break AES-256-GCM, SHA-256, or HKDF (needed only for the
  computational claim in §7, not for anything information-theoretic below).

This model is an extension, not a replacement, of the finite-key protocol's
own threat model in `doc/finite_key_procedure.md`; §3 of that document lists
what remains open on the quantum/classical QKD side (classical-channel
authentication for sifting and reconciliation, detector assumptions). Those
items are carried forward unresolved here, not re-examined.

## 2. Security of the distilled key (privacy amplification)

**Theorem 1 (finite-key security, cited).** For a block of $n$ sifted,
error-corrected bits with estimated phase error $\varphi$, leaked bits
$\mathrm{leakedBits}$, and parameters $(\varepsilon_{\mathrm{cor}},
\varepsilon_{\mathrm{PA}})$, the key $K$ of length

$$l = \mathrm{FiniteKeySecureKeyLength}(n, \varphi, \mathrm{leakedBits}, \varepsilon_{\mathrm{cor}}, \varepsilon_{\mathrm{PA}})$$

output by `PrivacyAmplify`-style Toeplitz hashing satisfies: with probability
at least $1 - \varepsilon_{\mathrm{cor}}$ the two sides' keys are identical,
and conditioned on that event, $d(K, E;\, U_l, E) \le \varepsilon_{\mathrm{PA}}$
for an adversary holding side information $E$.

*Proof.* This is the result proved in Tomamichel, Lim, Gisin, Renner, *Tight
finite-key analysis for quantum cryptography*, Nat. Commun. 3, 634 (2012),
applied via the source-replacement scheme to the entanglement-based (BBM92)
setting. `doc/finite_key_procedure.md` §2.5–2.6 states the exact formula
this codebase implements and its correspondence to that paper; it is not
re-derived here. That document's §3 explicitly flags the BB84→BBM92
adaptation as a protocol-specific step a specialist should confirm — this
analysis inherits that same caveat and does not resolve it. $\blacksquare$

**Lemma 1 (the $\varepsilon_{\mathrm{PA}}$ term is a Leftover Hash Lemma
instance).** The term

$$l = k - 2\log_2\!\frac{1}{2\varepsilon_{\mathrm{PA}}}, \qquad
k := n\bigl(1-h_2(\varphi)\bigr) - \mathrm{leakedBits} - \log_2\!\frac{2}{\varepsilon_{\mathrm{cor}}},$$

inside `FiniteKeySecureKeyLength` is exactly the bound given by the Leftover
Hash Lemma.

*Proof.* The Leftover Hash Lemma (Impagliazzo-Levin-Luby 1989; smooth-entropy
form as used in Renner's and Tomamichel et al.'s QKD proofs) states: if $F$
is a 2-universal family of hash functions $\{0,1\}^n \to \{0,1\}^l$, $S$ is a
uniform, independent choice of $f\in F$, and $X$ has conditional min-entropy
$H_{\min}(X\mid E) \ge k$, then

$$d\bigl(f_S(X),\, S,\, E \;;\; U_l,\, S,\, E\bigr) \;\le\; \frac{1}{2}\sqrt{2^{\,l-k}}.$$

Setting this bound equal to $\varepsilon_{\mathrm{PA}}$ and solving for $l$:

$$
\begin{aligned}
\frac{1}{2}\sqrt{2^{\,l-k}} &= \varepsilon_{\mathrm{PA}} \\[2pt]
\sqrt{2^{\,l-k}} &= 2\,\varepsilon_{\mathrm{PA}} \\[2pt]
2^{\,l-k} &= 4\,\varepsilon_{\mathrm{PA}}^{2} \\[2pt]
l-k &= 2 + 2\log_2 \varepsilon_{\mathrm{PA}} \\[2pt]
l &= k - \left(2\log_2\frac{1}{\varepsilon_{\mathrm{PA}}} - 2\right) \\[2pt]
l &= k - 2\log_2\!\frac{1}{2\varepsilon_{\mathrm{PA}}}
\end{aligned}
$$

which is exactly `finitekey.go`'s formula. The seed $S$ is `toeplitzSeed`'s
output; the Toeplitz matrix family is a textbook 2-universal hash family
(Krawczyk, *LFSR-based Hashing and Authentication*, CRYPTO 1994), so the
hypothesis of the lemma holds for `toeplitzHashNaive`. $\blacksquare$

**Remark (implementation correctness, not a separate proof).**
`toeplitzHashFast` dispatches to a POPCNT- or CLMUL-based implementation
(`toeplitz.go`, `toeplitz_clmul_amd64.go`) chosen for speed, not to a
different mathematical function. The 2-universality property proved above
holds for the *definition* `toeplitzHashNaive`; it transfers to the fast
paths only because they are checked, by `toeplitz_test.go` and
`toeplitz_vectors_test.go`, to compute bit-for-bit identical output across
randomized and fixed test vectors. A security proof about a specification
says nothing about a nonconforming implementation of it — this is why that
equivalence is tested, not assumed.

**Remark (the entropy-rate and correctness terms).** The $n(1-h_2(\varphi))$
term and the $\log_2(2/\varepsilon_{\mathrm{cor}})$ correctness-smoothing term
come from the cited paper's full finite-key chain-rule argument, which is
substantially longer than the single Leftover Hash Lemma application above
and is not reproduced here; Theorem 1 cites it as a unit.
`doc/finite_key_procedure.md` §2.4–2.5 specifies exactly how $\varphi$ and
$\mathrm{leakedBits}$ must be computed for the cited bound to apply — using a
different QBER estimate (e.g. same-basis QBER in place of the
conjugate-basis $\varphi$) in this formula is a misuse of Theorem 1, not a
variant of it.

**Remark (the bound requires $\varphi < \tfrac12$).** $h_2$ rises on
$[0, \tfrac12]$ and falls again on $[\tfrac12, 1]$, so $1 - h_2(\varphi)$ is a valid
entropy rate only for $\varphi < \tfrac12$; at $\varphi = 1$ it would equal 1 and
certify the whole block. `EstimatePhaseError` previously clamped $\varphi$ at 1 in
exactly the case that should yield nothing (no bits surviving error
correction). It now clamps at $\tfrac12$, and `FiniteKeySecureKeyLength` returns
0 for any $\varphi \ge \tfrac12$ (`TestFiniteKeyNoKeyAtOrAboveHalf`).

## 3. Security of the relay's one-time pad

**Definition 3 (perfect secrecy, Shannon 1949).** An encryption scheme with
plaintext space $M$, ciphertext space $C$, and key space $K$ has *perfect
secrecy* if for every plaintext distribution, $\Pr[M=m \mid C=c] = \Pr[M=m]$
for every $m\in M$, $c\in C$ with $\Pr[C=c] > 0$. Equivalently, $M$ and $C$
are statistically independent.

**Theorem 2 (`RelayXOR` is perfectly secret).** For any fixed-length
plaintext $m \in \{0,1\}^n$, if the pad $k$ is uniform over $\{0,1\}^n$,
independent of $m$, and used for exactly one message, then
$c = \mathrm{RelayXOR}(m,k) = m \oplus k$ is perfectly secret in the sense of
Definition 3.

*Proof.* Fix any ciphertext value $c \in \{0,1\}^n$. For every plaintext
$m \in \{0,1\}^n$, there is exactly one key value, $k = m \oplus c$,
consistent with that $(m,c)$ pair — XOR is a bijection on $\{0,1\}^n$ for
fixed $c$. Since $k$ is uniform over $\{0,1\}^n$ and independent of $m$:

$$\Pr[C=c \mid M=m] = \Pr[K = m\oplus c] = 2^{-n} \qquad \text{for every } m.$$

This is independent of $m$, so $\Pr[C=c\mid M=m] = \Pr[C=c]$ for every $m$,
which is Definition 3. $\blacksquare$

This is the general, arbitrary-length statement of the property that
`relay_test.go`'s `TestRelayXORIsPerfectSecrecy` checks empirically for the
$n=8$ (one byte) case by exhaustive enumeration of all 256 keys: the test is
a finite instance of the theorem above, not an independent argument.

**Corollary 1.** `RelayXOR`'s secrecy guarantee is unconditional (Definition
2): it does not depend on any computational hardness assumption, is not
weakened by an adversary with unbounded computing power, and requires no key
length beyond the message length.

**Precondition this theorem depends on, stated explicitly.** The proof
requires the pad to be (a) uniform and (b) used exactly once. `KMS.GetBytes`
/ `KMS.GetNextBytes` draw pad material by calling `KMS.Get`, which sets a
`Consumed` flag and refuses a second delivery of the same `key_ID`
(`ErrKeyConsumed`); the pad bytes themselves are QKD output, which is the
system's uniformity source. **If this single-delivery invariant were ever
violated — the same pad bytes reused for two messages — Theorem 2 would not
apply**, and the two ciphertexts would leak $m_1 \oplus m_2$ to anyone
observing both. The invariant is therefore load-bearing for this proof, not
an incidental implementation detail.

**Fact 1 (XOR is malleable).** Perfect secrecy says nothing about integrity.
For $c = m \oplus k$, flipping bit $i$ of $c$ flips bit $i$ of the decrypted
plaintext, deterministically, without the pad holder learning the plaintext
or the pad. This is why `RelayXOR` is never used by itself in this codebase
— every call site pairs it with the authenticator proved in §4.

## 4. Security of the per-hop authenticator (Poly1305)

**Definition 4 (one-time MAC existential unforgeability).** A tagging scheme
$(\mathrm{Tag}, \mathrm{Verify})$ keyed by $\kappa$ is an
$\varepsilon_{\mathrm{forge}}$-secure one-time MAC if, for any (possibly
unbounded) adversary that sees one message $m$ and its tag
$\mathrm{Tag}(\kappa, m)$ for a key $\kappa$ drawn uniformly and used for
exactly one message, the probability the adversary produces $(m', t')$ with
$m' \neq m$ and $\mathrm{Verify}(\kappa, m', t') = \mathrm{true}$ is at most
$\varepsilon_{\mathrm{forge}}$.

**Theorem 3 (Poly1305, cited).** Poly1305, keyed with a uniform, single-use
32-byte key $(r,s)$, is an $\varepsilon_{\mathrm{forge}}$-secure one-time MAC
per Definition 4, with

$$\varepsilon_{\mathrm{forge}} \;\le\; \frac{8\,\lceil \mathrm{len}(m)/16 \rceil}{2^{106}}$$

for a message $m$.

*Proof.* This is Theorem 3.3 of D. J. Bernstein, *The Poly1305-AES
message-authentication code*, FSE 2005, restated for the unkeyed-cipher case
in RFC 8439 §2.5. The mechanism: Poly1305 evaluates $m$'s 16-byte blocks as
coefficients of a polynomial over $\mathrm{GF}(2^{130}-5)$ at the secret point
$r$, then masks the result with $s$. Two distinct messages of at most $L$
blocks collide at a fixed $r$ with probability bounded via a
Schwartz-Zippel-type argument on the degree-$(L-1)$ difference polynomial;
the mask $s$ then converts that collision bound into a forgery bound, and the
published constant ($8$, with $r$'s effective entropy reduced to $\sim 106$
bits by the standard clamping) is proved, not re-derived, in the cited
source. $\blacksquare$

**Theorem 4 (the QKD keying removes the usual computational assumption).**
`polymac.go`'s `ComputePolyMAC` / `VerifyPolyMAC` achieve the *unconditional*
form of Theorem 3's guarantee — not merely the guarantee RFC 8439's typical
deployment (e.g. ChaCha20-Poly1305 in TLS) achieves.

*Proof.* Theorem 3's hypothesis is a uniform, single-use key. In a typical
Poly1305 deployment, that one-time key is itself derived from a *reusable*
secret key and a nonce via a pseudorandom function (ChaCha20): the one-time
key is only *computationally* close to uniform, so the overall MAC security
is computational, contingent on ChaCha20 being a secure PRF. In this
codebase, `relay.go`'s `forward` draws the 32-byte key directly from
`KMS.GetNextBytes(PolyMACKeyBytes)` — genuine QKD output, not a PRF output —
and `Receive` reconstructs the identical bytes via `KMS.GetBytes` on the same
`key_ID`s, consuming them exactly once (§3's single-delivery argument applies
identically to the MAC key pool draw). The key is therefore *actually*
uniform and single-use, which is precisely Theorem 3's hypothesis with no
computational relaxation — the theorem's conclusion then holds
unconditionally. $\blacksquare$

**Lemma 2 (constant-time verification).** `VerifyPolyMAC` compares the
recomputed and received tags with `crypto/subtle.ConstantTimeCompare`, not a
short-circuiting byte loop. This closes a timing side channel: a
byte-at-a-time comparison that returns on the first mismatch lets a network
adversary recover the correct tag one byte at a time by timing repeated
forgery attempts, which would undermine Theorem 3's bound in practice even
though the bound is correct in principle. (`VerifyUniversalHash` in
`universalhash.go`, used for the unrelated correctness-verification step
of §2.4 of `doc/finite_key_procedure.md`, was audited and fixed identically
in this pass, for the same reason.)

## 5. Single-hop composition: authenticate, then decrypt

**Theorem 5 (fail-closed composition).** For one relay hop, `RelayNode.Receive`
guarantees: (a) if the received packet is exactly what the sender produced,
the original key bytes are recovered exactly; (b) if the packet was modified
in any way after sending, `Receive` returns an error and never returns
attacker-influenced key bytes, except with probability at most
$\varepsilon_{\mathrm{forge}}$ (Theorem 3/4); and (c) an eavesdropper who only
reads, and never modifies, packets on this hop learns nothing about the key,
unconditionally (Theorem 2).

*Proof.* `Receive` computes `msg = macMessage(pkt.Dest, pkt.FromHop,
pkt.PadKeyIDs, pkt.Cipher)` and calls `VerifyPolyMAC(msg, pkt.Tag, macKey)`
**before** any call to `RelayXOR` on `pkt.Cipher` — the ciphertext is never
touched if verification fails. `macMessage` is a length-prefixed encoding of
every field an attacker could alter (destination, originating hop, pad
`key_ID`s, ciphertext); tampering with any one of those fields changes `msg`,
and by Theorem 3/4, a tampered $(\mathrm{msg}', \mathrm{tag})$ pair passes
verification with probability at most $\varepsilon_{\mathrm{forge}}$. If
verification succeeds and the packet was not tampered with, correctness
follows from `RelayXOR`'s self-inverse property
($\mathrm{RelayXOR}(\mathrm{RelayXOR}(m,k),k) = m$, proved by the same
bijection argument as Theorem 2). Passive secrecy against reading alone
follows directly from Theorem 2, independent of whether the MAC is ever
invoked, since `Tag`/`Cipher` observation alone gives an eavesdropper no
advantage over not observing them. $\blacksquare$

**Fact 2 (`MACKeyIDs` need not themselves be authenticated).** `macMessage`
does not include `pkt.MACKeyIDs`. This is not an omission: altering
`MACKeyIDs` only causes `Receive` to reconstruct a *different* `macKey` via
`KMS.GetBytes`, which — except with the same $\varepsilon_{\mathrm{forge}}$
probability a direct tag forgery would need — causes `VerifyPolyMAC` to fail
exactly as a corrupted tag would. Tampering with `MACKeyIDs` therefore cannot
produce a forged acceptance; at worst it denies service on an otherwise
legitimate packet, which is outside this document's scope (no availability
claim is made anywhere in this analysis).

`relay_test.go`'s three tampering tests
(`TestRelayNodeDetectsCiphertextTampering`,
`TestRelayNodeDetectsRoutingMetadataTampering`,
`TestRelayNodeDetectsTagTampering`) are finite, empirical instances
confirming the implementation matches Theorem 5(b) for one tampered field at
a time; they do not, and cannot, establish the probability bound itself —
that bound is Theorem 3/4's.

## 6. End-to-end composition over a multi-hop path

**Definition 5 (trusted-node model).** A relay path is a sequence of nodes
$N_0 \text{ (origin)} \to N_1 \to \dots \to N_H \text{ (destination)}$, where
each edge $(N_{i-1}, N_i)$ has an independent, single-use-keyed hop as
analyzed in §5. The **trusted-node assumption** is that every intermediate
node $N_1, \dots, N_{H-1}$ executes `RelayNode.Handle` faithfully and does
not leak the plaintext key it holds in memory between `Receive` and the next
`forward` call. This is the standard trusted-node (as opposed to
quantum-repeater) QKD network model; it is a hardware/operational trust
assumption, not something software can establish about itself.

**Theorem 6 (end-to-end bound over $H$ hops).** Under Definition 5, for a key
originated by $N_0$ with per-hop MAC forgery bound $\varepsilon_{\mathrm{forge}}$
(Theorem 3/4) and delivered to $N_H$:

- An external adversary who only observes the classical links between
  consecutive nodes (never compromises a node) learns nothing about the key,
  unconditionally, regardless of $H$ (secrecy is not degraded by chaining —
  each hop's pad is independent, so Theorem 2 applies hop by hop with no
  accumulation of leakage).
- The probability that such an adversary causes $N_H$ to accept a key
  different from the one $N_0$ originated, undetected at every hop, is at
  most $H \cdot \varepsilon_{\mathrm{forge}}$.

*Proof.* **Secrecy:** each hop re-encrypts under a freshly drawn, independent
pad (§3's precondition, enforced per-hop by `KMS`'s single-delivery
invariant across every `HopPools` entry); an eavesdropper's view of hop $i$
is, by Theorem 2, statistically independent of the key regardless of what
she learned from any other hop, so the $H$-hop composition leaks nothing
beyond what one hop alone leaks — i.e. nothing. **Authenticity:** for the
adversary to succeed, she must cause at least one hop's `Receive` to accept
a tampered packet; by Theorem 5(b) this happens at hop $i$ with probability
at most $\varepsilon_{\mathrm{forge}}$, and a union bound over the $H$
independent hop keys gives total probability at most
$H \cdot \varepsilon_{\mathrm{forge}}$. If she causes zero hops to accept a
tampered packet, every intermediate node forwards the unmodified, correctly
originated key by Theorem 5(a), so $N_H$ receives exactly what $N_0$ sent.
$\blacksquare$

**Corollary 2 (numeric example).** A relayed 256-bit key
(`ETSI014KeySizeBytes = 32`) produces a `macMessage` of roughly 90-150 bytes
depending on node-ID and `key_ID` lengths (each pad `key_ID` is a 36-byte
UUID, `keystore.go`), i.e. at most 10 sixteen-byte blocks. By Theorem 3,

$$\varepsilon_{\mathrm{forge}} \;\le\; \frac{8\cdot 10}{2^{106}} = \frac{80}{2^{106}} \;\approx\; 9.9\times10^{-31}$$

per hop. Even over a path of $H = 1000$ hops (far beyond any realistic
deployment), the union bound gives a total forgery probability of at most

$$H\cdot\varepsilon_{\mathrm{forge}} \;\approx\; 9.9\times10^{-28},$$

still about seventeen orders of magnitude below the finite-key
$\varepsilon_{\mathrm{sec}} = 10^{-10}$ baseline from §2:

$$\frac{10^{-10}}{9.9\times10^{-28}} \;\approx\; 1.0\times10^{17}.$$

The relay's contribution to the end-to-end failure probability is, for any
realistic topology, negligible next to the privacy-amplification term it
carries.

**What Theorem 6 does not claim.** It does not claim security against a
compromised intermediate node — the trusted-node assumption is a hard
boundary, not a detail to be optimized away; a malicious $N_i$ can read, and
substitute, any key passing through it, exactly as a classical trusted relay
can. Achieving confidentiality against the relay nodes themselves would
require quantum repeaters or end-to-end entanglement swapping, which is a
hardware capability this (or any) post-processing software cannot supply.

## 7. At-rest storage security (KMS): a distinct, weaker claim

**Theorem 7 (KMS confidentiality is computational, not information-theoretic).**
`KMS.PutWithID` stores each key record as `EncryptWithKey(masterKey,
keyBytes)`, i.e. AES-256-GCM under a key derived from `masterKey` via
HKDF-SHA256 (`encryption.go`). This protects the on-disk record's
confidentiality only against an adversary bounded by AES-256 and SHA-256's
conjectured hardness (Definition 2's computational case), and only for as
long as `masterKey` itself stays secret.

*Proof (by construction, not reduction).* `masterKey` is "operator
provisioned, not QKD-derived" (`kms.go`'s own doc comment) — it is an
ordinary 32-byte secret the deploying operator generates and manages, with
none of the physical generation process that gives the relayed key material
(§2-§6) its unconditional guarantees. The confidentiality of everything
`KMS` stores is therefore upper-bounded by the weaker of (a) AES-256-GCM's
security and (b) the operational security of `masterKey`'s custody — neither
of which this codebase can establish or improve on its own. $\blacksquare$

**This is a deliberate, bounded scope, not an oversight.** `KMS`'s job is
short-term, single-delivery-enforced holding of already-generated key
material between its creation and its one-time retrieval by an application
(§2.7 of `doc/finite_key_procedure.md`) — not long-term information-theoretic
archival. Conflating this computational at-rest guarantee with the
information-theoretic guarantees of §2-§6 would be exactly the kind of
imprecision this document exists to avoid. `masterKey` generation, rotation,
and custody are operational prerequisites, out of scope for this analysis,
exactly as the finite-key procedure document treats the classical channel's
own authentication as out of scope for itself.

## 8. Composed end-to-end claim

Pulling §2, §6, and §7 together, for a key that is generated by one QKD
link, privacy-amplified with parameters $(\varepsilon_{\mathrm{cor}},
\varepsilon_{\mathrm{sec}})$, and relayed over $H$ trusted hops to its
consuming application:

| Property | Guarantee | Type | Depends on |
|---|---|---|---|
| Correctness | holds except with probability $\varepsilon_{\mathrm{cor}}$ | information-theoretic | Theorem 1 |
| Secrecy vs. an external eavesdropper on any link (quantum, classical EC/PE traffic, or relay hops) | within $\varepsilon_{\mathrm{sec}} + H\cdot\varepsilon_{\mathrm{forge}}$ of ideal | information-theoretic | Theorems 1, 2, 6 |
| Secrecy vs. a compromised relay node | **not claimed** | — | Definition 5 is a hard trust boundary |
| Confidentiality of a key waiting, already generated, inside a KMS store | depends on `masterKey` secrecy and AES-256-GCM | computational | Theorem 7 |

With the Recommended parameters from `doc/finite_key_procedure.md`
($\varepsilon_{\mathrm{sec}} = 10^{-10}$, $\varepsilon_{\mathrm{cor}} =
10^{-15}$) and Corollary 2's per-hop estimate, the dominant term in the
combined secrecy bound $\varepsilon_{\mathrm{sec}} + H\cdot\varepsilon_{\mathrm{forge}}$
is $\varepsilon_{\mathrm{sec}}$ for any topology with fewer than roughly
$10^{20}$ hops. In concrete terms: the relay does not meaningfully weaken the
key's security relative to not relaying it at all; the finite-key
privacy-amplification step, not the relay, is where this system's security
budget is actually spent.

## 9. What this document does not resolve

- Everything already listed in `doc/finite_key_procedure.md` §3 (detector
  assumptions, classical-channel authentication for sifting/EC/PE traffic,
  the BB84→BBM92 adaptation of the cited finite-key bound) remains open and
  is not re-examined here.
- The trusted-node assumption (Definition 5) is structural, not a gap to be
  closed by this codebase; any deployment relying on Theorem 6 must
  independently assure itself of every intermediate node's physical and
  operational integrity.
- `masterKey` generation, rotation, and custody (§7) are operational
  requirements this analysis depends on but does not itself provide.
- This document proves properties of the algorithms as specified and as
  checked against their test suites; it does not constitute an independent
  implementation audit (side-channel analysis beyond Lemma 2's specific fix,
  fault-injection resistance, and similar engineering concerns are out of
  scope).
