### evaluation

Evaluates TimeTagger recordings through the whole chain and writes, for each recording pair, a report with figures, plus a summary comparing the pairs and pooling each link's recordings. It covers detector rates, coincidence-peak finding, the choice of the coincidence window, Cascade's disclosure against the Shannon limit, the key budget and the finite key. Reports, figures and numbers come from real recordings, so they are written next to the recordings, never into this repository, and the pre-commit hook refuses results and recordings in any case.

### Setup

Once, from the repository root:

```bash
python3 -m venv evaluation/.venv
evaluation/.venv/bin/pip install -r evaluation/requirements.txt
```

Go must be installed: `evaluate.py` builds `cmd/importtt` from this checkout on every run, so the evaluation always uses the code as it stands. Without Go, pass a built binary with `--importtt`.

### Running

```bash
evaluation/.venv/bin/python evaluation/evaluate.py <recording dir> [<recording dir> ...]
```

- A path can be a recording directory, a parent directory (searched recursively) or a single `*_timestamp_sequence.npy`. Two recordings in one directory with the same start time form a pair, whatever else the directory holds. A is the first in sorted order, as `tt_record_dual.py` uses.
- Site names come from the `"sites"` section of `acquisition/config.json` (`--config` for another file). The search settings come from its `"coincidence_peak"` section, the same the recorder uses.
- Output goes next to the recordings, to `<first recording directory>/evaluation/<date>/`, never into this repository (`--out` for another directory): `summary.md` with `comparison.png`, then one directory per pair with `report.md`, `rates.png`, `peak.png`, `window.png`, `ec.png` and `budget.png`, and `results.json` with every number.

| Option | Effect |
|---|---|
| `--no-sweep` | skip the coincidence-window sweep |
| `--no-pool` | skip pooling each link's recordings into one block |
| `--jobs N` | importtt runs in parallel, 4 by default; each needs about 0.6 GB on a 300 MB recording |

In Docker, the tools image carries everything; mount the recordings and a config and write to the working directory:

```bash
docker build --target tools -t qkd_post_processing/tools .
docker run --rm -v "$PWD/recordings:/rec:ro" -v "$PWD/acquisition/config.json:/cfg.json:ro" -v "$PWD/out:/work" \
  qkd_post_processing/tools python /app/evaluation/evaluate.py /rec --config /cfg.json --out /work/eval
```

### What each stage shows

- **Rates**: singles per channel on both sides over time. A dead or drifting channel, or a read that hit the server's batch cap (data may be missing), shows here first.
- **Coincidence-peak finding**: the coarse correlation over every lag, the peak with the lag held constant, the clock skew from per-segment peaks, and the clock-corrected peak with its Gaussian fit and HWHM.
- **Window**: sifted bits and QBER over all sifted pairs against the window half width, with the fitted HWHM marked. Where the QBER starts to climb, accidentals take over.
- **Error correction**: each method's disclosure against the Shannon limit, and whether it corrected everything.
- **Key budget**: sifted bits down to the asymptotic and finite key.
- **Pooled per link (summary)**: all evaluated recordings of one link in one privacy-amplification block. The QBER sample grows with the pool, which is how short recordings reach a finite key.

### Privacy

Reports, figures and `results.json` name sites, never IPs: an address missing from the config is shown as "site A" or "site B", and a last pass masks anything IP-shaped that an error message might quote. The links to the recordings and the coincidence reports, which carry the original file names and so the IPs, live in a temporary directory that is deleted at the end of the run, so the output holds nothing private but the measurements themselves. The repository's pre-commit hook refuses private IPs in any case.
