### Deployment

How to run the QKD network: one administration machine that holds the network CA, and any number of nodes, each running its key management system, its dashboard and its HTTPS front end in three containers. Nodes need Docker (with Compose) and a clone of this repository, nothing else; images build for amd64 and arm64 from the same `Dockerfile`. Everything goes through `deploy/setup.sh`.

### Administration machine

Create the network CA once, then one bundle per node, then declare the QKD links:

```bash
cd deploy
./setup.sh ca
./setup.sh node A a.example.org,192.0.2.1          # dashboard on 443, KMS on 8443
./setup.sh node B b.example.org,192.0.2.2 9443 9444 # or choose the two ports
./setup.sh link A B
./setup.sh sae alice A                             # an application of node A
./setup.sh bundle A
./setup.sh bundle B
```

`node` writes the node's credentials into `nodes/<ID>/pki/` (one certificate for the KMS, one for the dashboard's management access, one for the dashboard's key-exchange test application, one for HTTPS, and the node's master key), adds the node and its dashboard application to `nodes/network.json`, and writes `nodes/<ID>/.env` with a fresh dashboard password, printed once; only its hash is stored. The first host name is the address other nodes use to reach the node's KMS; list every name and IP the node is reached by. `sae` writes an application certificate into `saes/<SAE_ID>/` and registers the application under its node. `bundle` packs `nodes/<ID>/`, with the current `network.json`, into `nodes/<ID>.tar.gz`.

`deploy/ca/ca.key` can mint credentials for any node: keep it offline, never copy it to a node, and back it up. Each bundle holds that node's private keys: move it over a secure channel and delete the copy afterwards.

### On each node

```bash
git clone https://github.com/werefin/qkd_post_processing.git
cd qkd_post_processing/deploy
./setup.sh install /path/to/A.tar.gz
./setup.sh up A
```

`install` unpacks the bundle and makes the containers run as the installing user; `up` builds the images and starts the three containers, and again after any change. `./setup.sh status A`, `logs A` and `down A` do what they say. `NO_BUILD=1 ./setup.sh up A` starts from images already present, for nodes that receive images from a registry instead of building them.

Open the firewall for the two published ports only: the dashboard port to the operators, the KMS port to the other nodes and to the node's applications. The dashboard itself is never exposed without its login.

### Opening dashboards

Every node's HTTPS certificate is signed by the network CA, so trusting `ca.pem` once on a machine makes every dashboard trusted there:

```bash
./trust-ca.sh nodes/A/pki/ui/ca.pem
```

Then open `https://<node host>:<UI_PORT>` and log in as `admin` with the password `node` printed. Everything a node does is available from its dashboard: TimeTagger configuration and recording, upload of recordings from remote boxes, alignment of any recording pair, two-party key extraction with a neighbor (the production mode: each node uses only its own TimeTagger's recordings and both pools receive the same keys), single-site key extraction into a link's pool (one pair or several pooled), detector delay calibration, the CHSH diagnostic, the simulator, and the key management system (links and their reachability, pool halves, routes, applications, an end-to-end ETSI key exchange test between dashboards, and the audit trail).

### Changing network

Adding a node, a link or an application changes `network.json`. Run the matching `setup.sh` command on the administration machine, then `bundle` and `install` the new file on every node and run `up` there; a node reads the network description when it starts. A node removed from `network.json` is no longer reachable by the others.

### What runs, and what each part can see

| Container | Image | Holds | Reachable on |
|---|---|---|---|
| `kms` | `qkd_post_processing/kms`, distroless, about 13 MB | node certificate and key, master key, the encrypted stores, the audit trail | `KMS_PORT`, mutual TLS |
| `app` | `qkd_post_processing/app`, about 550 MB | admin and dashboard-application certificates, recordings, acquisition config, job history | the `ui` container only |
| `ui` | `caddy:2.8` | HTTPS certificate, the login hash | `UI_PORT` |

Every container runs read-only, as the installing user, without Linux capabilities (Caddy keeps only the one its binary needs), with no-new-privileges and rotated logs. The dashboard never sees the master key or the node key, so it can manage and use its node but cannot act as the node toward other nodes.

### Data on disk

Everything a node keeps is under `deploy/nodes/<ID>/`:

| Path | Contents | Back up |
|---|---|---|
| `pki/` | the node's credentials and master key | yes, securely: losing the master key loses every stored key |
| `network.json`, `.env` | the network description and the node's settings | yes |
| `data/kms/` | link pools, reserved keys, audit trail, all encrypted except the audit trail | yes, with `pki/kms/master.key` kept apart |
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
| `APP_UID`, `APP_GID` | set by `install` | the user the containers run as |
| `TWO_PARTY_AUTO_ACCEPT` | 1 | answer a neighbor's two-party extraction by itself; 0 = ignore them |

### Sizing

8 GB of RAM at minimum, 16 GB comfortable: the coincidence-peak search caps itself at about 4 GiB, and a pooled extraction of seventy seconds of recordings peaks at about 2 GB. Four or more cores; the peak refinement and skew fit run in parallel. Size the disk for the recordings, which are large; keys are small.

### Lab PC, Windows included

A photonics lab PC needs the dashboard only, for recordings, alignment and the coincidence log: no key management system, no certificates and no login. `deploy/lab.yml` runs it as one container, the same on Windows, macOS and Linux.

**Windows**: install Docker Desktop with the WSL2 backend (`wsl --install` in an administrator PowerShell, reboot, then Docker Desktop), and clone the repository. In PowerShell, from the repository:

```powershell
docker compose -f deploy/lab.yml up -d --build      # first start builds the image, a few minutes
start http://localhost:9823                         # the dashboard; load the config on the Acquisition page
docker compose -f deploy/lab.yml down               # stop; the data stays
```

**Data**: config, recordings and coincidence logs live in the Docker volume `qkd-lab_lab-data` and survive restarts and rebuilds; logs and recordings are downloaded and uploaded from the dashboard. `docker compose -f deploy/lab.yml down -v` deletes them.

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
- The dashboard answers `KMS unreachable`: `./setup.sh logs <ID>` shows why the `kms` container stopped; most often `network.json` does not list this node.
- Get Key answers 503: this node's half of the pool toward that application is empty; extract more key on that link.
- Get Key with Key IDs answers 404: the key_ID belongs to another application pair, was requested at the wrong node, or names the wrong master.
