### qkd_post_processing

Classical post-processing for entanglement-based (BBM92) quantum key distribution, from raw TimeTagger timestamps to finite-key-certified secret keys, and the key management system that stores them per QKD link and delivers them to applications across a network of trusted nodes. Every node runs in Docker and is operated entirely from its web dashboard.

```mermaid
flowchart TB
    subgraph ACQ["Acquisition · acquisition/"]
        AT[/"Alice TimeTagger"/]
        BT[/"Bob TimeTagger"/]
    end

    subgraph PP["Post-processing · qkd/"]
        CF["<b>Coincidence filtering</b><br/>drift removal · HWHM window"]
        SI["<b>Sifting</b><br/>keep matching bases"]
        EC["<b>Error correction</b><br/>optimized Cascade"]
        VE{{"<b>Verification</b><br/>CRC-16 + universal hash"}}
        PA["<b>Privacy amplification</b><br/>Toeplitz hashing"]
        PE["Phase-error bound φ<br/>hypergeometric tail"]
        LK["Leakage ledger<br/>parities + CRC + tag bits"]
        FK["Finite-key length ℓ<br/>ε<sub>sec</sub> = 1e-10 · ε<sub>cor</sub> = 1e-15"]
        AB(["abort, no key"])
    end

    subgraph KD["Key management system · cmd/kms, one per node"]
        POOL[("<b>Link pool</b><br/>encrypted, split between the two ends")]
        NB["<b>Neighbor delivery</b><br/>pool key reserved at both ends"]
        RL["<b>Relay</b><br/>one-time pad + Poly1305 per hop"]
        APP[/"Applications<br/>ETSI GS QKD 014, mutual TLS"/]
    end

    AT -- timestamps --> CF
    BT -- timestamps --> CF
    CF -- "true coincidences" --> SI
    SI -- "sifted key" --> EC
    SI -. "QBER sample" .-> PE
    EC -- "corrected key" --> VE
    EC -. "disclosed bits" .-> LK
    VE -. "tag bits" .-> LK
    VE -- mismatch --> AB
    VE -- match --> PA
    PE -.-> FK
    LK -.-> FK
    FK -. "output length" .-> PA
    PA -- "secret key" --> POOL
    POOL --> NB
    POOL --> RL
    NB --> APP
    RL --> APP

    classDef hw fill:#eef0f3,stroke:#6b7280,color:#111827
    classDef stage fill:#e3ecfd,stroke:#2f62c9,color:#0b1a33,stroke-width:1.5px
    classDef acct fill:#fff3dc,stroke:#c98500,color:#3a2600
    classDef stop fill:#fde4e4,stroke:#c43c3c,color:#4a0d0d
    classDef deliver fill:#e2f5e9,stroke:#2f9e5a,color:#0d331b,stroke-width:1.5px
    class AT,BT hw
    class CF,SI,EC,VE,PA stage
    class PE,LK,FK acct
    class AB stop
    class POOL,NB,RL deliver
    class APP hw
    style ACQ fill:none,stroke:#9ca3af,stroke-dasharray:5 4
    style PP fill:none,stroke:#2f62c9,stroke-dasharray:5 4
    style KD fill:none,stroke:#2f9e5a,stroke-dasharray:5 4
```

Solid arrows carry key material; dotted arrows carry public information that only sets the final key length.

### Repository layout

| Path | Contents |
|---|---|
| `qkd/` | the post-processing library (package `qkd`), one file per stage |
| `kms/` | the encrypted key store: single delivery, reservation for one application pair, wiping, pool split |
| `relay/` | hop-by-hop key relay: one-time pad and Poly1305, both keyed from QKD output |
| `cmd/kms/` | the key management system, one instance per node: ETSI GS QKD 014, node-to-node coordination, relay, audit |
| `cmd/qkdpki/` | the network CA, node credential bundles, application certificates, the shared network description |
| `cmd/importtt/` | the chain on a real dual-TimeTagger recording |
| `cmd/qkd_post_processing/` | the chain on simulated click streams |
| `cmd/controller/` | the network controller: nodes, links and settings for every node's agent ([`doc/services.md`](doc/services.md)) |
| `acquisition/` | Python: TimeTagger recording, clock alignment, CHSH diagnostic ([`acquisition/README.md`](acquisition/README.md)) |
| `evaluation/` | evaluates any recordings through the whole chain, one report with figures per pair and a pooled summary per link ([`evaluation/README.md`](evaluation/README.md)) |
| `services/` | the node's services from one image: agent, acquisition, processing, web ([`doc/services.md`](doc/services.md)) |
| `ui/` | the dashboard page: template, script, styles, logo |
| `api/` | OpenAPI files of the controller and the node services |
| `deploy/` | one node's containers, the controller's, the lab PC's, `setup.sh`, `trust-ca.sh` ([`doc/deployment.md`](doc/deployment.md)) |
| `.github/` | CI, weekly drift checks, image releases to GitHub Container Registry, Dependabot |
| `doc/` | procedures, security analysis, key management, deployment, code reference |
| `scripts/` | `release.sh`, `cross_check_toeplitz.py` |
| `references/` | vendor documents, local only: ignored by git and Docker and refused by the pre-commit hook |

### Quick start

```bash
go build ./...
go test ./...
go run ./cmd/qkd_post_processing -n 1200000 -err 0.02
```

Requires Go 1.24 for local builds; the only Go dependency is `golang.org/x/crypto` (Poly1305). Run `git config core.hooksPath .githooks` once per clone to enable the hook that keeps vendor documents out of every commit.

### Running the network

```bash
cd deploy
./setup.sh ca                                   # once, on the administration machine
./setup.sh node A a.example.org,192.0.2.1         # credentials, login and settings for node A
./setup.sh node B b.example.org,192.0.2.2
./setup.sh link A B                             # a QKD link between A and B
./setup.sh bundle A                             # nodes/A.tar.gz, to copy to node A
./setup.sh controller init ctl.example.org && ./setup.sh controller up   # the network controller
# on node A, inside a clone of this repository:
./setup.sh install A.tar.gz && ./setup.sh use-controller A https://ctl.example.org:8080 && ./setup.sh up A
```

Everything runs in Docker. One controller holds the network (nodes, links, settings) and every node's agent takes its settings from it. Each node runs hardened containers: `kms` (the key management system, a distroless image, the only holder of the node's key and master key), four services from one `app` image on their own ports (`agent` 9100, `acquisition` 9101, `processing` 9102, `web` 9823) and `ui` (HTTPS with a certificate from the network CA, behind a login); [`doc/services.md`](doc/services.md) describes them and their APIs. Trust the CA once with `./trust-ca.sh`, then every node's dashboard opens without warnings. A lab PC that only needs the dashboard, Windows included, runs `docker compose -f deploy/lab.yml up -d --build` and opens `http://localhost:9823` (Lab PC in [`doc/deployment.md`](doc/deployment.md)). Images build for amd64 and arm64 from the same `Dockerfile`. [`doc/deployment.md`](doc/deployment.md) is the full guide.

### What a node does from its dashboard

- **Acquisition**: load the TimeTagger config, record from both sites, and watch the coincidence peak live while the TimeTaggers stream: the tracker locks onto the lag and clock skew within seconds, then shows the clock-corrected peak, its HWHM, the true-pair rate and the accidentals as they evolve. Stored recordings can be replayed through the same tracker.
- **Recordings**: upload recordings copied off remote boxes; pairs are recognized by their shared start time.
- **Alignment**: coincidence-peak lag, window, score and clock skew for any pair, with zoomable plots.
- **Two-party key extraction**: the production mode; this node announces its own recordings' timing to a neighbor, which aligns, matches and decodes with its own, and both pools receive the same certified key under the same key_IDs.
- **Key extraction**: the chain on one pair or several pooled at one site, as a background job with a live log; detector delay calibration.
- **Continuous QKD**: in production, each node records its own TimeTagger without end and, every block, offers it to the neighbor it announces to; both run optimized Cascade and both pools receive the same keys, resuming by themselves after a restart.
- **Link monitor**: one link as it runs, with the same views as the TimeTagger vendor's QKD monitor: both systems and the channel between them, the quantum channel's stage, sync, fidelity, QBER and key-rate gauges, singles and coincidences per polarization, heralding, the bit rate after each protocol stage, the run's history, and every block as CSV or JSON.
- **Evaluation**: every stage of the chain on chosen recordings, with figures, kept on the node.
- **Network and KMS**: links and their reachability, each end's half of every pool, routes, applications, an end-to-end ETSI key exchange between two dashboards (key IDs and fingerprints only), the audit trail.
- **CHSH diagnostic** and the **simulator**, whose demo keys never reach the KMS.

### Real detector data from the command line

```bash
cd acquisition
python3 tt_record_dual.py                       # record from both TimeTaggers, tracking the peak live
python3 coincidence_peak.py <recording dir>     # coarse clock offset and skew
cd ..
go run ./cmd/importtt -dir acquisition/<recording dir> -cascade
```

`importtt` filters coincidences (drift removal, window from the half width at half maximum of a Gaussian fit to the peak, [`doc/coincidence_filtering.md`](doc/coincidence_filtering.md)), corrects errors with optimized Cascade, the least disclosure, f 1.03 to 1.08, counting the QBER from the errors it corrects so no sample is sacrificed  verifies with CRC-16 and a universal hash, and sizes the key by the finite-key bound at $\varepsilon_\mathrm{sec} = 10^{-10}$, $\varepsilon_\mathrm{cor} = 10^{-15}$ ([`doc/security.md`](doc/security.md)). `-dir a,b,c` pools recordings into one block, `-keystore dir` writes the key as ETSI 014 records, `-json` prints one report, and `-window-sweep 50,100,...` adds sifted bits and QBER for each window from the same load.

A single recording of a few seconds is too short for a finite key; continuous QKD distils blocks of minutes (below), and `evaluation/evaluate.py` follows any recording through every stage with figures, kept locally, never committed. Fine timestamps must be announced in one direction only (Alice to Bob), see Per-detector delays and the timing side channel in [`doc/coincidence_filtering.md`](doc/coincidence_filtering.md).

### Continuous QKD

Continuous QKD is the whole chain in production, from each node's own TimeTagger to final keys in the link pools, on any TimeTagger whose timestamps `tt_record_dual.py` can read: each node records without end in UTC-aligned segments, the announcing node streams each segment's timing to its neighbor as it closes, and every block both run optimized Cascade, verification and finite-key privacy amplification, with each link's block length planned from its QBER and pair rate ([`doc/security.md`](doc/security.md)). It runs from the dashboard or the network controller: Production: continuous two-party QKD in [`doc/deployment.md`](doc/deployment.md).

### Applications

An application gets a certificate from `./setup.sh sae <SAE_ID> <NODE_ID>` and calls its node's KMS with ETSI GS QKD 014:

```bash
curl --cert sae.pem --key sae.key --cacert ca.pem "https://a.example.org:8443/api/v1/keys/bob/enc_keys?number=2"
```

The answer's key_IDs go to the peer application, which fetches the same keys once from its own node at `/api/v1/keys/<master>/dec_keys`. Between neighbors the key is QKD pool material reserved at both ends; for distant nodes it is relayed hop by hop. [`doc/key_management.md`](doc/key_management.md) specifies the system.

### Library usage

```go
import (
	"qkd_post_processing/kms"
	"qkd_post_processing/qkd"
)

filtered, f, err := qkd.FilterCoincidences(alice, bob, qkd.DriftConfig{}, qkd.WindowConfig{})
res, err := qkd.RunCascade(alice, filtered, qkd.CascadeRunConfig{
	CoincidenceWindowPS: f.WindowPS,
	ChunkSize:           1024,
	ExpectedQBER:        f.PredictedQBER, // plans Cascade's first block size; the QBER itself is counted by correction
})
// res.FinalKeyBytes is the key; res.FiniteKey records how its length was derived
saved, discarded, err := kms.SaveKeys256("keys/", res.FinalKeyBytes)
```

Every stage also has a one-sided form (for example `ComputeChunkCRC` and `ReconcileChunks`) for running Alice and Bob as separate processes.

### Documentation

| Document | Contents |
|---|---|
| [`doc/coincidence_filtering.md`](doc/coincidence_filtering.md) | drift removal, HWHM window, detector delays, timing side channel |
| [`doc/key_management.md`](doc/key_management.md) | the key management system: entities, protocols, lifecycle, limits |
| [`doc/security.md`](doc/security.md) | security and the finite-key procedure: key length stage by stage, estimation by correction, pools, relay, delivery, storage |
| [`doc/services.md`](doc/services.md) | the controller and the node services: what each does, how they talk, CI and releases |
| [`doc/deployment.md`](doc/deployment.md) | running the network in Docker: CA, controller, nodes, links, applications, updates |
| [`doc/architecture.md`](doc/architecture.md) | code reference: every package, type and command |

### Make and test

```bash
make check      # what CI runs: fmt, vet, build, race tests, golangci-lint, ruff, Python tests
make test       # Go race tests
make pytest     # the services' and acquisition's Python tests
make services   # every node service in one process on http://127.0.0.1:9823, for development
make bench      # benchmarks only
docker build --target tools -t qkd-tools .      # every command, for offline analysis
scripts/release.sh minor                        # tag a release; you publish it with the release workflow
```

GitHub Actions run the same checks on every push and a weekly drift check; images are published only when you start the release workflow for a tag ([`doc/services.md`](doc/services.md)).
