### Key management system

**Status**: specification of the key management system implemented in `cmd/kms`, `kms/` and `relay/`. It defines the entities, the network description, the protocols between nodes and between nodes and applications, the lifecycle of a key, and the limits of what the system provides. `doc/security.md` proves its security properties; `doc/deployment.md` explains how to run it.

### Entities

| Entity | What it is | Identified by |
|---|---|---|
| Node | one site of the network, running one key management entity (KME), the `kms` program | its node ID, the common name of its certificate with role `kme` |
| QKD link | a pair of nodes that run QKD between them; each end holds the link's pool | the two node IDs |
| Pool | the keys a link has produced, stored identically at both ends under the same key_IDs | the link |
| Application (SAE) | a program that consumes keys, attached to exactly one node, its home | its SAE ID, the common name of its certificate with role `sae` |
| Node services | the node's agent and processing services (`services.md`), holding two certificates: role `admin` for management and the post-processing mailbox, and role `sae` for the dashboard's key exchange test; the agent also shows the admin certificate to the network controller, which never sees a key | the node ID (admin) and `dashboard-<node ID>` (SAE) |
| Network CA | the private certificate authority that signs every certificate above; its key stays offline | `deploy/ca/ca.pem` |

A certificate's organizational unit is its role and its common name its ID. The KMS authorizes every request from the certificate alone, never from anything inside the request: an application certificate can never act as a node, a node certificate never as an application, and a dashboard's admin certificate can read its own node's status and audit trail but cannot act as the node toward others.

### Network description

Every node reads the same `network.json`:

```json
{
  "nodes": {
    "A": {"kme_url": "https://a.example.org:8443"},
    "B": {"kme_url": "https://b.example.org:8443"},
    "C": {"kme_url": "https://c.example.org:8443"}
  },
  "links": [["A", "B"], ["B", "C"]],
  "saes": {"alice": "A", "carol": "C", "dashboard-A": "A", "dashboard-B": "B", "dashboard-C": "C"}
}
```

At start-up a node checks the description (known node IDs, no self-links, no duplicate links, every application's home exists, no application shares an ID with a node), opens one pool per link it is part of, and computes its routes: the first hop toward every reachable node on a shortest path through the links, by breadth-first search, ties going to the alphabetically first neighbor, so every node computes the same routes from the same file. A node not reachable through the links gets no route, and keys for its applications are refused. `qkdpki net` edits the file; a change takes effect on each node at its next start.

### How a link's pools are filled

Both ends of a link must store the same keys under the same key_IDs, so post-processing runs as two parties, each node on its own TimeTagger's recording (`qkd/twoparty.go`, `importtt -role alice|bob`):

- Alice's node announces, for every detection after double clicks are squashed, its timestamp and basis, never its bit; the announcement goes one way only.
- Bob's node finds its own recording of the same start time, aligns it to the announcement (offset and clock skew), removes the drift, sets the window from the HWHM of the peak, matches the coincidences and sends back only which of Alice's clicks are paired.
- Error correction is optimized Cascade: Bob asks Alice for the parities he needs, a whole pass's blocks in one request and then one request per halving step of all the searches under way, about a hundred round trips per block; Alice counts every parity she discloses herself. Nothing is sacrificed: after correction Bob announces, per basis, how many bits he corrected, which bound the other basis's phase errors (Errors counted by correction in `doc/security.md`). Both then exchange CRCs, Alice sends the verification tag, Bob confirms.
- Both hash their verified bits with the same Toeplitz seed to the certified length, and Alice names one key_ID per 256-bit record. Each node writes the records into the import folder of the link, and its KMS takes them into the pool.

Live QKD runs this for every block without anyone starting it: each node records its own TimeTagger in segments that start on the same UTC seconds as its neighbor's, the node that announces offers each block's timing in parts, and the other answers with its own segments of the same time (Production: Live QKD in `doc/deployment.md`).

The messages travel between the two nodes through the nodes' KMEs (`/kme/v1/pp/...`), so they are carried over the same mutual TLS as everything between nodes, and only between neighbors. Bob does the heavy part, filtering, matching and decoding, and never reveals his timing; Alice's node only builds events, transforms and hashes. The node whose detectors click less should announce, so that its announcement is the smaller one; the other node answers by itself.

### Pools and how they are split

A link's pool must be identical at its two ends. Both ends drawing from the same pool at the same time would let two keys, or two one-time pads, coincide, so the pool is split: node $u$ may originate with a key_ID only if `OriginatorOf(u, v)` accepts it, that is when the parity of its first hex digit is even and $u$ sorts before $v$, or odd and $u$ sorts after $v$. The two ends' rules are exact complements and every key_ID is random, so each end owns about half of the pool. An end uses a key of the other half only when the owner names it in an authenticated request, and refuses any request naming a key of its own half before consuming anything. Every key is therefore used at most once in the whole network, for one purpose (Pools: no key used twice in `doc/security.md`).

### Key delivery: ETSI GS QKD 014

Applications call their own node over TLS 1.3 with their certificate:

| Call | Request | Answer |
|---|---|---|
| Get Status | `GET /api/v1/keys/{slave_SAE_ID}/status` | source and target KME, master and slave SAE, key size 256, `stored_key_count` (keys deliverable toward that slave now), `max_key_per_request` 128, `min_key_size` 256, `max_key_size` 4096, `max_SAE_ID_count` 0 |
| Get Key | `GET /api/v1/keys/{slave_SAE_ID}/enc_keys?number=&size=` or `POST` with `{"number", "size"}` | `{"keys": [{"key_ID", "key"}]}`, sizes in bits, multiples of 256 |
| Get Key with Key IDs | `GET /api/v1/keys/{master_SAE_ID}/dec_keys?key_ID=` or `POST` with `{"key_IDs": [{"key_ID"}]}` | the same keys, once |

The master application calls Get Key naming the slave, passes the key_IDs to the slave over its own channel, and the slave calls Get Key with Key IDs at its own node naming the master. Both applications must belong to different nodes; additional slave SAEs are not supported. If fewer keys are available than requested, Get Key returns those it has; with none it answers 503. Get Key with Key IDs checks every listed key_ID before delivering any, answers 404 for a key_ID that does not exist or belongs to another pair (indistinguishably), and 410 for one already delivered or expired.

### Delivery between neighbors

When the slave's home is a neighbor, the key is pool material itself:

- The master's node takes the requested amount from its own half of the shared pool, one or more 256-bit pool keys per delivered key, and mints a key_ID for each delivered key.
- It sends the slave's node a reservation: the delivered key_IDs, the pool key_IDs that make up each, and the master and slave SAE IDs.
- The slave's node accepts only if the sender is the neighbor on that link, the master belongs to the sender and the slave to itself, and every named pool key is in the sender's half, unused and named once. It then moves those pool keys into its delivery store, reserved for the pair, and confirms.
- Only on confirmation does the master's node return the keys. Without it, the keys it took are destroyed and recorded as burned; they are never delivered.

### Delivery across the network

When the slave's home is further away, the master's node takes the key from its own half of the pool it shares with the first hop on the route, together with a Poly1305 key, and sends the first hop only their key_IDs, authenticated with the destination, the origin, the key_ID and the SAE pair; the first hop holds the same records and rebuilds the key from them, so the key is QKD output end to end and costs two pool keys on that link. From there each hop draws a one-time pad and a 32-byte Poly1305 key from its own half of the pool it shares with the next hop, in one draw, encrypts, and authenticates the ciphertext together with the destination, the sender, the origin, the key_ID, the SAE pair and the pad key_IDs. A node accepts a relay packet only from the neighbor that the packet names as sender, verifies the tag before decrypting, and either forwards it, re-encrypted under its own fresh draw, or, at the destination, checks that the slave belongs to it and the master to the origin and stores the key reserved for the pair. The destination's answer travels back along the path, and the master's node returns the key only when the destination confirms the key_ID it chose. Intermediate nodes keep nothing.

### Lifecycle of a key

| State | Where | Leaves the state when |
|---|---|---|
| Plaintext output | `<import>/<neighbor>/<key_ID>.json`, written by `importtt -keystore`, mode `0o600` | the importer, every two seconds, stores it encrypted in that link's pool and deletes the file; records younger than a second are left for the next pass, since they may still be being written |
| Pool key | `pools/<neighbor>/`, encrypted, at both ends of the link | it is drawn for a delivered key, a pad or a MAC key, or it expires (`-key-ttl`, 30 days by default) |
| Reserved | `local/` of the slave's node, encrypted, reserved for one master and slave pair | the slave fetches it, or it expires (`-delivery-ttl`, 24 hours by default) |
| Delivered or expired | the same file, ciphertext wiped | never: the tombstone stays, so the key_ID can never be stored again |

Every record is AES-256-GCM under the node's master key and written atomically. Delivery wipes the ciphertext in the same step; a sweeper wipes expired material every ten minutes. Each store keeps an in-memory index of its records, so lookups, draws and status counts never scan the disk.

### Audit trail

Every node appends one JSON line per key event to `audit.log` in its data directory, rotated by size (`-audit-max-mb`, 64 MB, and `-audit-keep`, 16 files, so about 1 GB at most), and keeps the newest thousand in memory for its dashboard: `imported`, `enc_keys`, `reserved`, `dec_keys`, `dec_keys_refused`, `relay_forwarded`, `relay_refused`, `relay_failed`, `burned` and `expired`, each with the time, key_ID, SAE pair, peer and size or count as they apply. The audit trail never holds key material (`TestAuditHoldsNoKeyMaterial`).

### Node-to-node and management calls

| Call | Caller | Purpose |
|---|---|---|
| `POST /kme/v1/reserve` | a neighbor's KME | reservation for delivery between neighbors |
| `POST /kme/v1/relay` | the neighbor named in the packet | one hop of a relayed key |
| `GET /kme/v1/node` | any KME of the network, or this node's admin | counts per store and per link (this node's half and the other), routes, local applications; `?probe=1` (admin) also checks that every neighbor answers over mutual TLS, with its round-trip time |
| `GET /kme/v1/network` | this node's admin | the network description and the computed routes |
| `GET /kme/v1/audit?n=` | this node's admin | the newest audit events |
| `POST /kme/v1/pp/send` | this node's admin | sends one post-processing message to a neighbor's dashboard: JSON, or as `application/x-qkd-pp` a frame (4-byte big-endian header length, JSON header, raw bytes) whose bytes cross both hops as they are and come back from `recv` in the same frame |
| `POST /kme/v1/pp/inbox` | a neighbor's KME | queues a post-processing message for this node's dashboard |
| `GET /kme/v1/pp/recv`, `/kme/v1/pp/sessions` | this node's admin | the next message of a session (long poll), and the sessions with messages waiting |

Every answer from a peer is checked to come from the node it was sent to. Bodies are limited to 1 MiB. A node without its TLS files refuses to start; `-insecure-plain-http` takes identities from request headers and exists for development only.

### What the system does not provide

- **Information-theoretic authentication of post-processing messages.** They are authenticated by TLS between the nodes, which is computational and has to hold only while the protocol runs (Two-party post-processing in `doc/security.md`).
- **Protection against a compromised node.** Every node is trusted with the keys it holds or relays.
- **Information-theoretic authentication of reservations and of application identities.** Both rest on the certificates and TLS, which are computational.
- **Live reconfiguration.** A changed network description takes effect when each node restarts.
