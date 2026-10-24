### Deployment

How to run the QKD network, all in Docker: one administration machine that holds the network CA and runs the network controller, and any number of nodes, each running its key management system, four services from one image on their own ports, and its HTTPS front end ([`services.md`](services.md) describes the parts). Nodes need Docker (with Compose) and a clone of this repository, nothing else; images build for amd64 and arm64 from the same `Dockerfile`, or come from GitHub Container Registry. Everything goes through `deploy/setup.sh`.

### Administration machine

Create the network CA once, then one bundle per node, then declare the QKD links:

```bash
cd deploy
./setup.sh ca
./setup.sh node A a.example.org,192.0.2.1          # dashboard on 443, KMS on 8443
./setup.sh node B b.example.org,192.0.2.2 9443 9444 # or choose the two ports
./setup.sh link A B                                # or ./setup.sh link all: every pair of nodes, A-B to C-D
./setup.sh sae alice A                             # an application of node A
./setup.sh bundle A
./setup.sh bundle B
./setup.sh controller init ctl.example.org,192.0.2.10  # the network controller, here or on another host
./setup.sh controller up
```

`node` writes the node's credentials into `nodes/<ID>/pki/` (one certificate for the KMS, one for the dashboard's management access, one for the dashboard's key-exchange test application, one for HTTPS, and the node's master key), adds the node and its dashboard application to `nodes/network.json`, and writes `nodes/<ID>/.env` with a fresh dashboard password, printed once (only its hash is stored), and a random token the node's services show each other. The first host name is the address other nodes use to reach the node's KMS; list every name and IP the node is reached by. `sae` writes an application certificate into `saes/<SAE_ID>/` and registers the application under its node. `bundle` packs `nodes/<ID>/`, with the current `network.json`, into `nodes/<ID>.tar.gz`.

`controller init` writes the controller's certificate and an operator certificate into `controller/pki/`; `controller up` starts it on port 8080 (`CONTROLLER_PORT` in `controller/.env`), keeping the network in `controller/data/network.json`.

`deploy/ca/ca.key` can mint credentials for any node: keep it offline, never copy it to a node, and back it up. Each bundle holds that node's private keys: move it over a secure channel and delete the copy afterwards.

### On each node

```bash
git clone https://github.com/werefin/qkd_post_processing.git
cd qkd_post_processing/deploy
./setup.sh install /path/to/A.tar.gz
./setup.sh use-controller A https://ctl.example.org:8080   # optional: the controller sets this node's QKD
./setup.sh up A
```

`install` unpacks the bundle and makes the containers run as the installing user; `up` builds the images and starts the node's containers, and again after any change. `./setup.sh status A`, `logs A` and `down A` do what they say. With `APP_IMAGE`, `KMS_IMAGE` and `IMAGE_TAG` in the node's `.env`, `NO_BUILD=1 ./setup.sh up A` pulls the published images instead of building them (CI and releases in [`services.md`](services.md)).

Open the firewall for the two published ports only: the dashboard port to the operators, the KMS port to the other nodes and to the node's applications; on the controller's host, port 8080 to the nodes and the operators. The dashboard itself is never exposed without its login, and the node's services are not published at all.

### Opening dashboards

Every node's HTTPS certificate is signed by the network CA, so trusting `ca.pem` once on a machine makes every dashboard trusted there:

```bash
./trust-ca.sh nodes/A/pki/ui/ca.pem
```

Then open `https://<node host>:<UI_PORT>` and log in as `admin` with the password `node` printed. The dashboard has Live QKD (two-party QKD with the neighbors: start, stop, settings, every block), the link monitor for each QKD link as it runs, the key management system (links and their reachability, pool halves, routes, applications, an end-to-end ETSI key exchange test between dashboards, and the audit trail), the coincidence log and the CHSH diagnostic; its home page loads the TimeTagger config.

### Controlling the network

With the controller running and the nodes pointed at it, the operator sets the whole network in one place, with the operator certificate:

```bash
./setup.sh ctl PUT /v1/nodes/A '{"site": "A"}'               # the node's site in its own acquisition config
./setup.sh ctl PUT /v1/nodes/B '{"site": "B"}'
./setup.sh ctl PUT /v1/links/A-B '{"a": "A", "b": "B"}'                     # announcer auto: the end that clicks less
./setup.sh ctl PUT /v1/links/A-D '{"a": "A", "b": "D", "announcer": "D"}'   # any pair declared in network.json, announcer fixed
./setup.sh ctl PUT /v1/settings '{"block_s": 900, "block_auto": true, "segment_s": 10}'
./setup.sh ctl GET /v1/network                                  # every node: config, status, up or down
./setup.sh ctl PUT /v1/links/A-B '{"a": "A", "b": "B", "active": false}'   # pause a link
```

Any two nodes linked in `network.json` can run Live QKD together, and a node runs it with all its neighbors at once, announcing to some and answering others. Within 10 s each node's agent picks up its config and starts or stops recording and announcing by itself. A managed node's dashboard shows its Live QKD but refuses Start and Stop; disable it at the controller (`"enabled": false`) or delete it there to give it back to its dashboard. The controller only coordinates: if it is down, every node keeps running on the last config it received.

### Changing network

Adding a node, a link or an application changes `network.json`. Run the matching `setup.sh` command on the administration machine, then `bundle` and `install` the new file on every node and run `up` there; a node reads the network description when it starts. A node removed from `network.json` is no longer reachable by the others.

### What runs, and what each part can see

| Container | Image | Holds | Reachable on |
|---|---|---|---|
| `kms` | `kms`, distroless | node certificate and key, master key, the encrypted stores, the audit trail | `KMS_PORT`, mutual TLS |
| `agent` | `app`, `SERVICE=agent` | admin certificate, the two-party settings | 9100, inside the node |
| `acquisition` | `app`, `SERVICE=acquisition` | recordings, acquisition config, segments, coincidence logs | 9101, inside the node |
| `processing` | `app`, `SERVICE=processing` | admin and dashboard-application certificates, recordings, job history, block log | 9102, inside the node |
| `web` | `app`, `SERVICE=web` | nothing | 9823, the `ui` container only |
| `ui` | `caddy:2.8` | HTTPS certificate, the login hash | `UI_PORT` |
| `controller` | `kms`, `--entrypoint controller`, one per network | controller certificate, the desired network | `CONTROLLER_PORT`, mutual TLS |

Every container runs read-only, as the installing user, without Linux capabilities (Caddy keeps only the one its binary needs), with no-new-privileges and rotated logs. The node's services never see the master key or the node key, so they can manage and use their node but cannot act as the node toward other nodes; each inner service answers only calls carrying the node's `SERVICE_TOKEN`.

### Data on disk

Everything a node keeps is under `deploy/nodes/<ID>/`:

| Path | Contents | Back up |
|---|---|---|
| `pki/` | the node's credentials and master key | yes, securely: losing the master key loses every stored key |
| `network.json`, `.env` | the network description and the node's settings | yes |
| `data/kms/` | link pools, reserved keys, audit trail (rotated by size), all encrypted except the audit trail | yes, with `pki/kms/master.key` kept apart |
| `data/recordings/`, `data/config/` | raw recordings, TimeTagger config with its auth keys, job history | recordings as needed, config yes |
| `data/import/` | the pipeline's plaintext keys for the few seconds before the KMS imports them | no |
| `data/sim-keys/`, `data/cache/`, `data/caddy/`, `data/tmp/` | demo keys of the simulator, compiled Numba code, Caddy state, uploads in transit and job scratch space | no |

### Settings in `.env`

| Variable | Default | Meaning |
|---|---|---|
| `NODE_ID` | set by `node` | the node's ID in `network.json` |
| `UI_PORT`, `KMS_PORT` | 443, 8443 | published ports |
| `UI_USER`, `UI_PASSWORD_HASH` | `admin`, set by `node` | dashboard login; replace the hash with `docker run --rm -i caddy:2.8 caddy hash-password` |
| `UPLOAD_MAX_MB` | 102400 | largest recording upload; 0 = no limit |
| `RECORDINGS_MAX_GB` | 0 | when set, the oldest recordings are deleted beyond this size |
| `AUDIT_MAX_MB`, `AUDIT_KEEP` | 64, 16 | the KMS audit trail moves to `audit.log.1` past this size, older files one up, this many kept (about 1 GB at most); 0 MB never rotates |
| `BLOCKS_LOG_MAX_MB`, `BLOCKS_LOG_KEEP` | 16, 4 | the same for Live QKD's block log `blocks.jsonl` (80 MB at most, years of blocks) |
| `APP_UID`, `APP_GID` | set by `install` | the user the containers run as |
| `TWO_PARTY_AUTO_ACCEPT` | 1 | answer a neighbor's two-party extraction by itself; 0 = ignore them |
| `SERVICE_TOKEN` | set by `node` | shown by the node's services to each other |
| `CONTROLLER_URL` | none | the network controller; unset, the node runs on its dashboard's settings |
| `APP_IMAGE`, `KMS_IMAGE`, `IMAGE_TAG` | local builds | images to pull with `NO_BUILD=1`, e.g. `ghcr.io/<owner>/<repo>/app` and a version |
| `ALERT_WEBHOOK` | none | URL the node posts its alerts to as JSON whenever they change, e.g. a chat or paging service |
| `ALERT_MIN_KEYS` | 100 | warn when this node may originate fewer keys than this toward a neighbor |
| `ALERT_MAX_QBER` | 0.11 | warn when a block's QBER is above this |

### Sizing

8 GB of RAM at minimum, 16 GB comfortable: the coincidence-peak search caps itself at about 4 GiB, and a pooled extraction of seventy seconds of recordings peaks at about 2 GB. Four or more cores; the peak refinement and skew fit run in parallel. Size the disk for the recordings, which are large; keys are small.

### Production: Live QKD

With the nodes deployed, each link fills its pools by itself, at both ends, with the same keys under the same key_IDs; the key management system relays them to nodes further away (`doc/key_management.md`).

**Clocks**: run NTP on every node. Each node cuts its recording into segments that start on the same UTC seconds as its neighbors', so the clocks must agree to well under a segment; the TimeTaggers' own clocks are aligned from the data and need nothing.

**TimeTagger config**: on each node's dashboard, home page, load the acquisition `config.json` (same shape as `acquisition/config.example.json`) with the sites and their TimeTagger addresses; the node's recorder reads its own site's TimeTagger from it and refuses to start without it. Any TimeTagger whose timestamps `tt_record_dual.py` reads works: everything after the timestamps is this software's.

**Start**: with the controller, add the nodes and links there (Controlling the network); leave each link's announcer at auto, so the controller makes the end whose detectors click less announce (the site of the source should answer on all its links) and moves it only once the other end clicks 20% less. Without it, on each node open Live QKD, choose this node's TimeTagger under Two-party, production, and press Start; on one end of each link also tick the neighbor under Announce to: that node offers every block, the other answers by itself. Block length is planned per link by default, the shortest within 5% of the best key rate up to the longest block (15 minutes), from the link's QBER, pair rate and failed blocks (Automatic block length in `doc/security.md`); untick Automatic for every block at the longest.

**What happens**: as soon as a segment closes, the announcing node sends its timing, never a bit, to the neighbor in about 3.3 bytes per click; the neighbor aligns its own segment of the same time from the previous clock model and answers with a bare receipt. The announcing node keeps only its bit and flags per click; the neighbor keeps, for itself, the announced clicks that can pair, a few percent at MHz rates, with their indices. Nothing else is disclosed, memory stays bounded by one segment and nothing piles up. When the block closes, both run the usual protocol (sifting by the neighbor's pair list, optimized Cascade, verification and finite-key privacy amplification) on what they kept, which takes seconds, and the key lands in the link's pool at both ends. Both nodes list every block with its QBER, phase-error bound, disclosure, round trips and key.

**Clock sync**: the answering node tracks the two clocks continuously: every quarter second of each segment it measures where the coincidence peak sits, and a Kalman filter on the delay, the skew and the skew's drift predicts the next segment's alignment from all of them, so a skew that changes with temperature is followed, not refound. The link monitor shows the share of slices in lock, how far the measured centres sit from the model, and the skew with its uncertainty; a lost lock starts a full search by itself. Segments are 5 s by default.

**Real time**: the block length is set by the finite-key statistics, not by computing: a block certifies key only with enough pairs, so the first key comes one block after the start (5 minutes) and then a new batch every block, seconds after it closes. The pools turn these batches into a steady key rate for applications. The announcement needs about 10 MB/s of link per 3 MHz of the announcing detectors; a slower link or a smaller node takes shorter segments (`segment_s`), which also lower the memory per segment.

**Watching it**: the Link monitor page shows each link as it runs: both systems and the channel between them, the quantum channel's stage from waiting for signal to generating secure keys, sync, fidelity, QBER and key rate, the singles and coincidences per polarization, heralding, the bit rate after each protocol stage, and the whole run's history, with every block as CSV or JSON. The sidebar and the Live QKD page show the alerts: the recorder stopped, a block failed, the QBER above `ALERT_MAX_QBER`, no key with a neighbor for three blocks, a pool below `ALERT_MIN_KEYS`, the key management system not answering; with `ALERT_WEBHOOK` set they are also posted there.

**Restarts**: the settings are kept on the node; after a reboot or a container restart the node starts recording again and continues the schedule where it was, and a recorder left behind by a crash is stopped first. Stop ends recording and scheduling on that node until Start is pressed again.

**Coincidences per polarization** are shown on the answering node only: it alone holds both bits of a pair after error correction, and sending the counts would tell the other node how many zeros the key holds.

### Lab PC, Windows included

A photonics lab PC needs the dashboard only, for the coincidence log and the CHSH diagnostic: no key management system, no certificates and no login. `deploy/lab.yml` runs the four services from one image, the same on Windows, macOS and Linux; only the web page is published.

**Windows**: install Docker Desktop with the WSL2 backend (`wsl --install` in an administrator PowerShell, reboot, then Docker Desktop), and clone the repository. In PowerShell, from the repository:

```powershell
docker compose -f deploy/lab.yml up -d --build      # first start builds the image, a few minutes
start http://localhost:9823                         # the dashboard; load the config on the home page
docker compose -f deploy/lab.yml down               # stop; the data stays
```

**Data**: config, recordings and coincidence logs live in the Docker volume `qkd-lab_lab-data` and survive restarts and rebuilds; coincidence logs are downloaded from the dashboard. `docker compose -f deploy/lab.yml down -v` deletes them.

**TimeTaggers**: the container reaches them through Docker Desktop's network like any program on the PC, so the PC must reach their address and port (40404 by default); allow it in the Windows firewall if asked.

**Access**: the dashboard listens on this PC only (`127.0.0.1:9823`) because it has no login. To open it to other machines, run the full node with its HTTPS login instead (Administration machine, On each node).

**Updating**: `git pull`, then the same `up -d --build`.

### Updating

```bash
git pull
cd deploy && ./setup.sh up A
```

Images are rebuilt; credentials and data are untouched.

### Troubleshooting

- A link shows as unreachable: check that the peer's `kme_url` host resolves from this node and is among the host names its certificate was issued for, and that `KMS_PORT` is open between the two nodes.
- A page answers `the ... service does not answer`: `./setup.sh status <ID>` shows which container stopped, `./setup.sh logs <ID>` why.
- The agent shows the controller as not connected (`/api/controller`): check `CONTROLLER_URL`, that port 8080 is open toward the controller, and that the controller's certificate lists the host name in that URL.
- The dashboard answers `KMS unreachable`: `./setup.sh logs <ID>` shows why the `kms` container stopped; most often `network.json` does not list this node.
- Get Key answers 503: this node's half of the pool toward that application is empty; extract more key on that link.
- Get Key with Key IDs answers 404: the key_ID belongs to another application pair, was requested at the wrong node, or names the wrong master.
