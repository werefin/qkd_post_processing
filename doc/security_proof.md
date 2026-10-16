### Formal security analysis of the post-processing chain and the key management system

**Status:** formal analysis, companion to `doc/finite_key_procedure.md` and `doc/key_management.md`. The finite-key document fixes the privacy-amplification parameters and cites the bound they rest on; the key management document specifies the protocols between nodes and applications. This document proves the security properties of the key store, the relay and key delivery, and states how they compose with the finite-key guarantee into one end-to-end claim.

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
| $\varepsilon_{\mathrm{forge}}$ | probability a one-time authenticator accepts a message it did not legitimately produce |
| $P_{uv}$ | the pool of link $uv$: the same set of (key_ID, key) records at node $u$ and at node $v$ |
| $O_{u,v}$ | the predicate `OriginatorOf(u, v)` on key_IDs, true for $u$'s half of $P_{uv}$ |
| $d(X;Y)$ | statistical (trace) distance between the distributions of $X$ and $Y$ |

**Definition (statistical distance).** For random variables $X, Y$ over the same finite set,

$$d(X;Y) = \frac{1}{2}\sum_{z} \bigl|\Pr[X=z] - \Pr[Y=z]\bigr|.$$

Two distributions with $d(X;Y) \le \varepsilon$ are $\varepsilon$-close: no test distinguishes them with advantage above $\varepsilon$.

**Definition (information-theoretic and computational security).** A guarantee is *information-theoretic* (unconditional) when it holds against an adversary of unbounded computing power, as a bound on a statistical distance or on a probability over the choice of a key. It is *computational* when it holds only against a time-bounded adversary, contingent on an unproven hardness assumption, such as that AES-256, SHA-256 or ECDSA P-256 has no efficient attack. This document says which of the two applies to every claim; conflating them is the most common error in this kind of analysis.

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

**Theorem (finite-key security, cited).** For a block of $n$ sifted, error-corrected bits with phase-error bound $\varphi$, disclosed bits $\mathrm{leakedBits}$ and parameters $(\varepsilon_{\mathrm{cor}}, \varepsilon_{\mathrm{PA}})$, the key $K$ of length

$$l = \mathrm{FiniteKeySecureKeyLength}(n, \varphi, \mathrm{leakedBits}, \varepsilon_{\mathrm{cor}}, \varepsilon_{\mathrm{PA}})$$

output by Toeplitz hashing satisfies: with probability at least $1 - \varepsilon_{\mathrm{cor}}$ the two ends' keys are identical, and conditioned on that event, $d(K, E;\, U_l, E) \le \varepsilon_{\mathrm{PA}}$ for an adversary holding side information $E$.

*Proof.* This is the result of Tomamichel, Lim, Gisin and Renner, *Tight finite-key analysis for quantum cryptography*, Nat. Commun. 3, 634 (2012), applied through the source-replacement scheme to the entanglement-based (BBM92) setting. The Phase-error estimation and Privacy amplification sections of `doc/finite_key_procedure.md` state the exact formula the code implements and its correspondence to that paper. That document flags the BB84 --> BBM92 adaptation as a step a specialist should confirm; this analysis inherits the caveat and does not resolve it. $\blacksquare$

**Lemma (the $\varepsilon_{\mathrm{PA}}$ term is a leftover hash bound).** The term

$$l = k - 2\log_2\!\frac{1}{2\varepsilon_{\mathrm{PA}}}, \qquad
k := n\bigl(1-h_2(\varphi)\bigr) - \mathrm{leakedBits} - \log_2\!\frac{2}{\varepsilon_{\mathrm{cor}}},$$

inside `FiniteKeySecureKeyLength` is exactly the bound of the Leftover Hash Lemma.

*Proof.* The Leftover Hash Lemma (Impagliazzo, Levin and Luby 1989, in the smooth-entropy form of Renner's and Tomamichel et al.'s QKD proofs) states: if $F$ is a 2-universal family $\{0,1\}^n \to \{0,1\}^l$, $S$ a uniform, independent choice of $f\in F$, and $H_{\min}(X\mid E) \ge k$, then

$$d\bigl(f_S(X),\, S,\, E \;;\; U_l,\, S,\, E\bigr) \;\le\; \frac{1}{2}\sqrt{2^{\,l-k}}.$$

Setting this equal to $\varepsilon_{\mathrm{PA}}$ and solving for $l$:

$$
\begin{aligned}
\frac{1}{2}\sqrt{2^{\,l-k}} &= \varepsilon_{\mathrm{PA}} \\[2pt]
2^{\,l-k} &= 4\,\varepsilon_{\mathrm{PA}}^{2} \\[2pt]
l &= k - 2\log_2\!\frac{1}{2\varepsilon_{\mathrm{PA}}}
\end{aligned}
$$

which is `finitekey.go`'s formula. The seed $S$ is `toeplitzSeed`'s output, and Toeplitz matrices form a 2-universal family (Krawczyk, *LFSR-based hashing and authentication*, CRYPTO 1994), so the lemma's hypothesis holds for the defining implementation `toeplitzHashNaive`. $\blacksquare$

**Remark (fast implementations).** `toeplitzHashFast` dispatches to a POPCNT or CLMUL implementation chosen for speed, not to a different function. 2-universality holds for the definition; it transfers to the fast paths only because `toeplitz_test.go` and `toeplitz_vectors_test.go` check them bit for bit against it on random and fixed vectors.

**Remark (entropy-rate and correctness terms).** The $n(1-h_2(\varphi))$ term and the $\log_2(2/\varepsilon_{\mathrm{cor}})$ term come from the cited paper's full chain-rule argument, cited here as a unit. The Error verification and correctness and Phase-error estimation sections of `doc/finite_key_procedure.md` fix how $\varphi$ and $\mathrm{leakedBits}$ must be computed for the bound to apply; using the same-basis QBER in place of the conjugate-basis bound $\varphi$ would be a misuse of the theorem, not a variant of it.

**Remark (the bound needs $\varphi < \tfrac12$).** $1 - h_2(\varphi)$ is a valid entropy rate only for $\varphi < \tfrac12$; at $\varphi = 1$ it would equal 1 and certify the whole block. `EstimatePhaseError` caps $\varphi$ at $\tfrac12$ and `FiniteKeySecureKeyLength` returns 0 for $\varphi \ge \tfrac12$ (`TestFiniteKeyNoKeyAtOrAboveHalf`).

### Single use of pool keys across both ends of a link

The one-time pad and the one-time MAC below are secure only if every pool key protects at most one message. Single delivery inside one store is not enough: both ends of link $uv$ hold the same records, so two stores could each deliver the same key once. This section proves the stronger property the system actually needs.

**Definition (pool split).** For distinct node IDs $u, v$, $O_{u,v}(\mathit{id})$ is true exactly when the parity of the first hex digit of $\mathit{id}$ is even and $u < v$, or odd and $u > v$. Hence $O_{v,u}(\mathit{id}) = \lnot O_{u,v}(\mathit{id})$ for every key_ID (`TestOriginatorOfSplitsThePool`).

**Lemma (single delivery in one store).** In one store, a key_ID's material is returned at most once. *Proof.* Every path that returns material (`Get`, `GetReserved`, `GetNextMatching`, `GetNextBytesMatching`, `GetBytes`) runs under the store's mutex, checks the in-memory index for an available, matching record, and in the same critical section rewrites the record without its ciphertext (atomically: temporary file, sync, rename) and marks the index entry consumed. A second request finds it consumed or wiped and is refused; a tombstoned key_ID is refused by every `PutWithID`, so it cannot be re-imported. $\blacksquare$

**Theorem (no pool key is used twice).** Every record of $P_{uv}$ is used, across both ends together, for at most one of: padding one relay payload, keying one MAC, or forming part of one application key.

*Proof.* Node $u$ originates with a pool key only through `GetNextBytesMatching` filtered by $O_{u,v}$ (relay pads and MAC keys, `forward`; direct application keys, `deliverDirect`). Node $v$ consumes a record of $u$'s half only when $u$ names it: in `Receive`, which refuses any named key outside $O_{u,v}$ before consuming anything, or in `handleReserve`, which checks every component against $O_{u,v}$, against repetition and against availability before consuming any. Symmetrically for $v$'s half. So each record has one owner end, which alone may choose it; the other end consumes it only when the owner names it in an authenticated message. By single delivery at the owner, the owner chooses it at most once; by single delivery at the other end, a replayed or forged naming of an already used record is refused. $\blacksquare$

`TestBothEndsAtOnce` checks the consequence empirically: twenty concurrent requests from both ends of one link deliver eighty distinct keys, each identical at its two ends.

### Security of the relay's one-time pad

**Definition (perfect secrecy, Shannon 1949).** An encryption scheme has *perfect secrecy* if for every plaintext distribution $\Pr[M=m \mid C=c] = \Pr[M=m]$ for all $m$ and all $c$ with $\Pr[C=c] > 0$.

**Theorem (`RelayXOR` is perfectly secret).** If the pad $k$ is uniform over $\{0,1\}^n$, independent of the plaintext $m \in \{0,1\}^n$ and used for one message only, then $c = m \oplus k$ is perfectly secret.

*Proof.* For a fixed $c$ and every $m$, exactly one key, $k = m \oplus c$, is consistent with $(m, c)$. With $k$ uniform and independent of $m$,

$$\Pr[C=c \mid M=m] = \Pr[K = m\oplus c] = 2^{-n} \qquad \text{for every } m,$$

which is independent of $m$. $\blacksquare$

The pad is QKD pool material, uniform by the finite-key theorem up to $\varepsilon_{\mathrm{sec}}$, and used once by the theorem on single use of pool keys. `TestRelayXORIsPerfectSecrecy` checks the one-byte case exhaustively; it is a finite instance of the theorem, not an independent argument.

**Fact (XOR is malleable).** Flipping bit $i$ of $c$ flips bit $i$ of the decrypted plaintext without knowledge of $m$ or $k$. This is why `RelayXOR` is never used alone: every packet is authenticated as below.

### Security of the per-hop authenticator (Poly1305)

**Definition (one-time MAC).** A tagging scheme keyed by $\kappa$ is an $\varepsilon_{\mathrm{forge}}$-secure one-time MAC if, for any adversary who sees one message $m$ and its tag under a uniform key used for that message only, the probability of producing $(m', t')$ with $m' \neq m$ that verifies is at most $\varepsilon_{\mathrm{forge}}$.

**Theorem (Poly1305, cited).** Poly1305 keyed with a uniform, single-use 32-byte key is an $\varepsilon_{\mathrm{forge}}$-secure one-time MAC with

$$\varepsilon_{\mathrm{forge}} \;\le\; \frac{8\,\lceil \mathrm{len}(m)/16 \rceil}{2^{106}}.$$

*Proof.* Bernstein, *The Poly1305-AES message-authentication code*, FSE 2005, restated for a directly supplied one-time key in RFC 8439. Poly1305 evaluates the message's 16-byte blocks as a polynomial over $\mathrm{GF}(2^{130}-5)$ at the secret point $r$ and masks the result with $s$; two distinct messages of at most $L$ blocks collide at a fixed $r$ with probability bounded by the degree of their difference polynomial, and the mask turns that into the forgery bound, with $r$'s entropy reduced to about 106 bits by clamping. $\blacksquare$

**Theorem (QKD keying makes the MAC unconditional).** `ComputePolyMAC` and `VerifyPolyMAC` achieve the theorem's bound information-theoretically.

*Proof.* In common deployments, such as ChaCha20-Poly1305 in TLS, the one-time key comes from a pseudorandom function of a reusable key, so the MAC is only computationally secure. Here `forward` takes the 32-byte key from pool material in the same draw as the pad, and `Receive` reconstructs it from the same key_IDs; the material is QKD output and, by the theorem on single use of pool keys, keys one message only. That is the cited theorem's hypothesis with no computational relaxation. $\blacksquare$

**Lemma (constant-time verification).** `VerifyPolyMAC` compares tags with `crypto/subtle.ConstantTimeCompare`, so the comparison's timing does not reveal how many leading bytes of a forged tag were right. `VerifyUniversalHash`, used for the correctness check of the pipeline, compares the same way.

### Single-hop composition: authenticate, then decrypt

**Theorem (fail-closed hop).** For one hop, `Receive` guarantees: (a) an unmodified packet yields exactly the sender's payload; (b) a packet modified in any field the attacker can influence is refused, and no attacker-influenced payload is ever returned, except with probability at most $\varepsilon_{\mathrm{forge}}$; (c) an eavesdropper who only reads learns nothing about the payload, unconditionally.

*Proof.* `Receive` first refuses any packet naming a pool key outside the sender's half, and any packet that carries its key by key_IDs without coming straight from its origin. It then reconstructs the MAC key and checks the tag over `macMessage`, which encodes, each with a uvarint length prefix, the destination, the sending hop, the origin, the end-to-end key_ID, the master and slave SAE IDs, the pad key_IDs, the payload key_IDs and the ciphertext. The length prefixes make the encoding injective, so any change to any of these fields changes the authenticated message, and by the Poly1305 theorems it verifies with probability at most $\varepsilon_{\mathrm{forge}}$. Decryption runs only after verification, so (b) holds; (a) follows from $(m \oplus k) \oplus k = m$; (c) is the one-time pad theorem. $\blacksquare$

**Fact (MAC key IDs need not be authenticated).** `macMessage` omits `MACKeyIDs`. Changing them makes `Receive` reconstruct a different key, and the tag then fails exactly as a forged tag would; at worst a legitimate packet is refused, and availability is not claimed anywhere in this analysis.

`TestRelayNodeDetectsCiphertextTampering`, `TestRelayNodeDetectsRoutingMetadataTampering` (destination, key_ID, master, slave and origin, one at a time) and `TestRelayNodeDetectsTagTampering` are empirical instances of part (b); the probability bound is the theorem's, not theirs.

### End-to-end composition over a multi-hop path

**Definition (trusted-node model).** A relay path is $N_0$ (origin) --> $N_1$ --> ... --> $N_H$ (destination), each edge an independent hop as above. The **trusted-node assumption** is that every intermediate node runs `Handle` faithfully and does not leak the payload it holds between `Receive` and the next `forward`. This is the standard trusted-node QKD network model, a physical and operational trust assumption that software cannot establish about itself.

**Theorem (end-to-end bound over $H$ hops).** Under the trusted-node model, for a payload originated by $N_0$ and stored by $N_H$:

- An adversary who observes and modifies the links but compromises no node learns nothing about the payload, unconditionally, for any $H$.
- The probability that she makes $N_H$ store a payload, a key_ID or an SAE pair different from what $N_0$ sent, undetected, is at most $H \cdot \varepsilon_{\mathrm{forge}}$.

*Proof.* *Secrecy:* each hop pads with fresh pool material of its own link, used once by the theorem on single use of pool keys, so by the one-time pad theorem her view of each hop is independent of the payload whatever she saw elsewhere, and chaining adds nothing. *Integrity:* to succeed she must make at least one hop accept a modified packet, which by the fail-closed hop theorem happens at each hop with probability at most $\varepsilon_{\mathrm{forge}}$; the union bound over $H$ hops gives $H \cdot \varepsilon_{\mathrm{forge}}$. Otherwise every hop forwards exactly what it received. $\blacksquare$

**Corollary (numbers).** A relayed 256-bit key gives a `macMessage` of at most about 230 bytes (node and SAE IDs, a 36-byte end-to-end key_ID, one 36-byte pad key_ID, 32 bytes of ciphertext, length prefixes), at most 15 sixteen-byte blocks, so

$$\varepsilon_{\mathrm{forge}} \;\le\; \frac{8\cdot 15}{2^{106}} \;\approx\; 1.5\times10^{-30}$$

per hop. Even for $H = 1000$ hops,

$$H\cdot\varepsilon_{\mathrm{forge}} \;\approx\; 1.5\times10^{-27},$$

about seventeen orders of magnitude below the finite-key $\varepsilon_{\mathrm{sec}} = 10^{-10}$. The relay's contribution is negligible next to the privacy-amplification term.

**What this theorem does not claim.** Security against a compromised intermediate node: a malicious $N_i$ can read and replace any payload it relays, as any classical trusted relay can. Confidentiality against the relays themselves needs quantum repeaters or entanglement swapping, which no post-processing software can supply.

### Key delivery to applications

This section covers what the key management system adds on top of the transport above: which application may receive a key, and when.

**Theorem (a key reaches only its SAE pair).** A key delivered through Get Key to master SAE $a$ for slave SAE $b$ is returned by Get Key with Key IDs only to $b$, only once, and only when $b$ names $a$ as master.

*Proof.* The delivering node stores its copy with `PutReserved(id, key, a, b)`, either in `handleReserve` (neighbor) or in `Handle` at the destination (relay). Every pool draw skips reserved records, so no other path can return it. `handleDecKeys` takes the slave identity from the caller's client certificate, never from the request, requires that SAE to belong to this node in the network description, and calls `CheckReserved` for every listed ID before `GetReserved` delivers any; both refuse unless the record's pair equals (URL master, certificate slave). Single delivery in one store makes the delivery unique. The answer for an ID of another pair is the same as for an unknown ID, so an application cannot probe which keys exist. $\blacksquare$

**Remark (that theorem's security is computational).** Application identity rests on the client certificate, issued by the network CA and verified in a TLS 1.3 handshake (ECDSA P-256), so authorization is computational: an adversary who forged an application certificate, or who broke TLS on the reservation messages between two nodes (which carry key_IDs and SAE IDs, never key material), could redirect a key to another application of the same node. Relay packets do not have this weakness, since their SAE pair is inside the information-theoretic MAC; giving reservation messages the same MAC is possible at the cost of one pool key each.

**Theorem (both ends agree before release).** Get Key returns a key to the master only after the slave's home node has stored the same key under the same key_ID for the same pair.

*Proof.* Neighbor: `deliverDirect` takes the pool keys, sends their key_IDs to the slave's node, and returns them only if that node answers success after `handleReserve` stored the reservation; otherwise it clears the material and records each key as burned. The slave's node reconstructs the key from the same pool records, which are identical at both ends except with probability $\varepsilon_{\mathrm{cor}}$ (finite-key theorem). Relay: `deliver` returns a key only when the destination answers with the key_ID the origin chose, which it does only after `PutReserved` succeeded, and the stored payload equals the sent one except with probability $H\cdot\varepsilon_{\mathrm{forge}}$. A lost answer leaves at worst a reservation whose key_ID the master never learned, so the slave never asks for it, and it is wiped at expiry. $\blacksquare$

**Theorem (every delivered key is QKD output).** Between neighbors and across relay paths alike, the delivered key is pool material of a QKD link, $\varepsilon_{\mathrm{sec}}$-close to uniform.

*Proof.* Between neighbors the key is the reserved pool records themselves. For a relay path, `OriginateFromPool` takes the key from the origin's half of the pool it shares with the first hop, together with that hop's MAC key, and sends only their key_IDs under the MAC; the first hop rebuilds the key from its own copy of the same records, consuming them, and from there each hop carries it under its own one-time pad and MAC. No step generates key material any other way. A pad on the first hop would hide nothing the first hop does not already hold, so each relayed key costs two pool keys on its first link, as a padded hop does. $\blacksquare$

### Two-party post-processing

Each site runs its own half (`RunAlice`, `RunBob` in `qkd/twoparty.go`, the `-role` modes of `cmd/importtt`) on its own recording only, and both store the result under the same key_IDs, which is how the pools of a link come to hold the same records.

**Lemma (what the protocol makes public).** The messages carry exactly: Alice's announcement (per click the timestamp, the basis and a multi-photon flag); Bob's pair list (Alice's event indices), the sample's seed and size; both sides' sample bits; per polar block and attempt the frozen values, a hash seed and a 32-bit tag, and Bob's list of failed blocks, or with 5G NR LDPC per frame the syndrome, a hash seed and a 32-bit tag, Bob's lists of failed frames and the punctured values revealed for them; both sides' chunk CRCs; Alice's verification seed and tag and Bob's yes or no; the privacy-amplification seed, the key length and the key_IDs. *Proof.* These are the only message types, and each side checks every received message's kind and shape before using it (`DecodeMessage`, `RunAlice`, `RunBob`). Bob sends no timestamp and no time offset, so the timing is announced one way, as the timing side channel requires (Per-detector delays and the timing side channel in `coincidence_filtering.md`). $\blacksquare$

**Theorem (two-party disclosure equals the joint pipeline's).** The bits charged against the key, $\mathrm{leakedBits}$, are the same as in the joint pipeline: for polar, per block the final frozen count plus a 32-bit tag and Bob's verdict per attempt; for 5G NR LDPC, per frame the syndrome and its tag, less the punctured values still hidden when their columns are independent, plus Bob's verdict per attempt; then 16 bits per kept CRC chunk and the verification tag. The sample bits are disclosed but removed from the key, as in `RunPolar`, and Bob's failed-block lists are functions of the tags already charged. *Proof.* By construction (`polarPlan.leaked`, `finishTwoParty`), and checked: on the same data and sample the two-party protocol and `RunPolar` give the same key length and the same disclosure (`TestTwoPartyMatchesJoint`). $\blacksquare$

**Remark (authentication of the classical channel).** The finite-key theorem assumes that the post-processing messages are authenticated. Here they travel between the two sites' KMEs over TLS 1.3 with the nodes' certificates, which is computational authentication. Because authentication only has to hold while the protocol runs, breaking TLS later does not help an adversary against a key already produced: secrecy stays information-theoretic as long as TLS was not broken during the session itself. Alice and Bob also check that they agree on every protocol parameter and on a SHA-256 digest of Alice's announcement before any bit is used.

### At-rest storage security: a distinct, weaker claim

**Theorem (stored keys are protected computationally).** Every record is stored as AES-256-GCM under a key derived from the node's master key by HKDF-SHA256 (`encryption.go`), so its confidentiality on disk holds only against an adversary bounded by those primitives, and only while the master key stays secret.

*Proof (by construction).* The master key is an operator-provisioned 32-byte secret (`qkdpki node` generates it), with none of the physical process behind QKD output; the confidentiality of a stored record is bounded by the weaker of AES-256-GCM's security and the custody of that key. $\blacksquare$

**Remark (exposure is bounded in time).** A key's ciphertext is wiped when it is delivered or expires, leaving a tombstone, so a later theft of the disk and the master key reveals only keys still waiting, never keys already delivered. The pipeline's output is plaintext only between the moment `importtt` writes it into the link's import folder (mode `0o600`) and the next import pass, at most a few seconds. In the deployed containers the master key is mounted into the KMS container alone; the dashboard and the HTTPS front end never see it.

### Composed end-to-end claim

For a key produced by one QKD link with parameters $(\varepsilon_{\mathrm{cor}}, \varepsilon_{\mathrm{sec}})$ and delivered to an application pair, directly or over $H$ trusted hops:

| Property | Guarantee | Type | Rests on |
|---|---|---|---|
| Both ends hold the same key | except with probability $\varepsilon_{\mathrm{cor}}$, plus $H\cdot\varepsilon_{\mathrm{forge}}$ if relayed | information-theoretic | finite-key theorem, end-to-end bound |
| Secrecy against an eavesdropper on any link | within $\varepsilon_{\mathrm{sec}} + H\cdot\varepsilon_{\mathrm{forge}}$ of ideal | information-theoretic | finite-key theorem, single use of pool keys, one-time pad, end-to-end bound |
| Uniformity of a key between neighbors | $\varepsilon_{\mathrm{sec}}$-close | information-theoretic | finite-key theorem |
| Uniformity of a relayed key | $\varepsilon_{\mathrm{sec}}$-close | information-theoretic | every delivered key is QKD output |
| Only the reserved pair receives the key, once | holds | computational (certificates, TLS) | key delivery to applications |
| Secrecy against a compromised node | **not claimed** | none | trusted-node model |
| Confidentiality of a key waiting in a store | AES-256-GCM and master-key custody | computational | at-rest storage |

With $\varepsilon_{\mathrm{sec}} = 10^{-10}$ and the per-hop estimate above, $\varepsilon_{\mathrm{sec}}$ dominates the secrecy bound for any topology below about $10^{19}$ hops: the relay does not meaningfully weaken the key, and the security budget is spent in privacy amplification.

### What this document does not resolve

- Everything listed in What this does not resolve of `doc/finite_key_procedure.md`: detector assumptions, authentication of the classical channel between the two sites of a link, the BB84 --> BBM92 adaptation of the finite-key bound.
- **Information-theoretic authentication of the post-processing messages.** The two-party protocol's messages are authenticated by TLS, computationally (Two-party post-processing). Tagging them with Poly1305 under keys from the link's pool would make that authentication information-theoretic, at a cost of one pool key per message.
- The trusted-node assumption, which is structural; a deployment must assure itself of every intermediate node's integrity independently.
- Custody, rotation and backup of the master keys and of the network CA's private key.
- An implementation audit: side channels beyond the constant-time comparisons above, fault injection and similar engineering concerns are out of scope.
