## Deploying dashboard

Run this **on-prem, on the same lab network as the TimeTaggers** --> not a public cloud VM. Three reasons, not just preference:

1. **Acquisition needs reachability**: `/api/acquire/start` launches `tt_record_dual.py` against the TT IPs in `config.json`; it only works if the host can actually route to them
2. **Real key material**: `/api/align/run` writes real distilled secret keys to `KEYSTORE_DIR`. Keep that on infrastructure you control, not a shared or third-party host
3. **Large recordings**: uploads can be tens of GB; LAN beats WAN

### Quick start

```bash
cd ui
UID=$(id -u) GID=$(id -g) docker compose up -d --build
sudo bash trust_caddy_ca.sh      # adds the hosts entry, trusts Caddy's CA
```

Open `https://qoj.postproc:62444` (edit hostname/port in `docker-compose.yml` and `Caddyfile` to taste; same shape as skQCI's demo). Without the trust script your browser will warn on every visit, since Caddy's cert comes from its own local CA (`tls internal`), not a public one.

**Other machines on the LAN**: copy `caddy-root-ca.crt` (written by `trust_caddy_ca.sh`) and `trust_ca_client.sh` to each one, then run `bash trust_ca_client.sh <deploy-host-lan-ip>`.

### What's running

- `app`: gunicorn + this Flask app, plus the `qoj_post_processing` and `importtt` Go binaries it shells out to (both built into the image).
- `caddy`: TLS termination + reverse proxy, the only container with a published port.

Both restart automatically (`unless-stopped`) on host reboot.

### Config

Everything lives in env vars (`ui/Dockerfile` sets sane defaults) or in `ui/config/config.json` (uploaded via the Acquisition page, or drop a file into the bind-mounted `ui/config/` directory --> see `acquisition/config.example.json` for the shape).

| Var | Default | What |
|---|---|---|
| `UPLOAD_MAX_MB` | 102400 (100 GiB) | Recording upload cap; `0` = unlimited |
| `KEYSTORE_MAX` | 30000 | Real-key ceiling (never evicts existing keys, see `app.py`) |
| `RECORDINGS_MAX_GB` | 0 (unlimited) | Opt-in: once set, oldest recordings auto-delete past this size |
| `KEYSTORE_DIR` / `RECORDINGS_DIR` / `CONFIG_DIR` | `/app/{keys,recordings,config}` | Bind-mounted, see below |

Lower `UPLOAD_MAX_MB` to roughly your largest real recording plus headroom --> 100 GiB is a dev-friendly default, not a sizing recommendation.

`RECORDINGS_DIR` has no retention policy by default (raw captures are irreplaceable); opt in via `RECORDINGS_MAX_GB`, or delete individually from the Recordings page once a recording's key has been extracted.

### Persistent data

`docker-compose.yml` bind-mounts three host directories:

```
ui/keys/        # real + (briefly) simulated keys --> back this up
ui/recordings/  # uploaded *_timestamp_sequence.npy / *_channel_sequence.npy
ui/config/      # config.json
```

Back up `ui/keys/` like any secret store; `ui/recordings/` is regenerable from the original TimeTagger captures, so lower priority.

### Sizing

- **RAM**: 8 GB minimum, 16 GB comfortable. The coincidence-peak FFT search caps itself at `max_memory_gib` (4 GiB default, in `config.json`'s `coincidence_peak` section) --> don't raise that on a tight host
- **CPU**: 2+ cores; the FFT search and LDPC/Winnow correction benefit from more
- **Disk**: size `ui/recordings/` for your actual recording volume, not the 100 GiB cap.
- Gunicorn runs 2 workers (`ui/Dockerfile`'s `CMD`); add `--max-requests 200 --max-requests-jitter 50` to recycle them if memory grows over time, and a Docker memory limit (`deploy.resources.limits.memory` in `docker-compose.yml`) on a genuinely tight host.

### Updating

```bash
git pull
UID=$(id -u) GID=$(id -g) docker compose up -d --build
```

Rebuilds Go binaries and Python deps; bind-mounted data is untouched.

### Troubleshooting

- `"qoj_post_processing"`/`"importtt" not found on PATH"` --> the image build didn't finish; check `docker compose logs app`
- Alignment stuck on "Waiting for data" --> need more than 2 uploaded recordings in `ui/recordings/`
- Browser cert warning --> trust Caddy's local CA (see Quick start)
