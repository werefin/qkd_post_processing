### acquisition

Gets real detector clicks from a pair of Swabian TimeTaggers into `qoj_post_processing.Run`, the real-data counterpart to the simulated click streams `cmd/qoj_post_processing` generates.

### Idea

A BBM92 source emits entangled photon pairs and sends one photon from each pair to two separate sites. At each site a TimeTagger timestamps every detector click to picosecond resolution. Two clicks (one recorded at each site) came from the same photon pair if their timestamps line up once the fixed transmission delay between the sites is accounted for; which clicks line up (and, later, which basis/bit each one represents) is the raw material the rest of this repo's post-processing chain runs on.

The catch: two TimeTaggers run on independent local clocks, and there's no wire between the two sites carrying a "start now" signal --> so nothing ties one side's timestamps to the other's a priori. This pipeline solves that in three steps: **record** both sides' raw clicks independently (`tt_record_dual.py`, talking to each TimeTagger through Jena's `tt_stream_external.py` client), **find** the unknown clock offset and drift between the two recordings from the data itself (`coincidence_peak.py`, by correlating the two streams and locating the coincidence peak the shared photon pairs create), then **feed** the now-aligned, per-channel-labeled clicks into the Go pipeline (`cmd/importtt`) that does the actual sifting, error correction, and privacy amplification.

| stage | tool | does |
|---|---|---|
| record | `tt_record_dual.py` (uses `tt_stream_external.py`) | connects to two TimeTaggers over the network, records a fixed duration, writes per-TT `.npy` + `.json` |
| align | `coincidence_peak.py` | finds the clock offset and drift between the two recordings |
| post-process (QKD) | `cmd/importtt` (Go, repo root) | maps detector channels to BBM92 (basis, bit), aligns the clocks, runs the full sifting --> privacy-amplification chain |
| post-process (CHSH diagnostic) | `chsh_diagnostic.py` | standalone alternative to post-process: per-block singles + per-channel-pair ROI coincidence counts for a CHSH/Bell self-testing analysis (see below) |

`config.json` holds every site's connection/detector settings plus both scripts' recording/analysis defaults, so none of that is hardcoded in the `.py` files

### Code structure

| file | key pieces |
|---|---|
| `tt_stream_external.py` | One class, `TimeTaggerStream`: `connect()` (3 retries, custom SHA-256 challenge in place of `multiprocessing.connection`'s default MD5), `start_stream()` (puts the TT into continuous recording), `read_stream()` (drains whatever accumulated since the last call, less than 4M detections). Vendor code --> see Provenance. |
| `tt_record_dual.py` | `load_config`/`resolve_link`/`site_connection_and_settings` turn `config.json` + CLI flags into two `(connection, settings)` pairs. `TTContext` is one TimeTagger's mutable state (buffered frames, error, timing), shared between `main` and its own `reader_thread`. Both `reader_thread`s wait on one `threading.Barrier` so `start_stream()` fires on both TTs at the same moment, then each loops `read_stream()` independently until told to stop. `write_outputs` concatenates the buffered frames into the two `.npy` arrays + the `.json` sidecar. |
| `coincidence_peak.py` | `_coarse_search` (FFT cross-correlation over binned counts, `numba`-jitted `_bin_timestamps`) → `_refine_candidate`/`_best_window` (exact timestamp differences, finds the best coincidence-window width) --> `_fit_clock_skew` (segment-wise linear fit of lag vs. elapsed time, `_grid_search_clock_skew` as a fallback). `analyze_files` orchestrates all three and builds the JSON report; `save_plot` renders the diagnostic figure; `main` is the `click` CLI (`_apply_config_defaults` wires up `--config`). |
| `chsh_diagnostic.py` | Standalone CHSH/Bell diagnostic, unrelated to the QKD chain --> see "Optional: CHSH self-testing diagnostic" below. |
| `config.json` | `"sites"` (per-TT connection + detector settings), `"links"` (named site pairs), `"record_dual"` (which link + recording-mode defaults), `"coincidence_peak"` (analysis-flag defaults). |

### How it actually works

**Recording (`tt_record_dual.py`)** has no math to it: trick is synchronization, not computation. Both TimeTaggers are told to `start_stream()` behind one `threading.Barrier`, so the two calls fire within the same instant (each thread blocks until the other reaches the barrier too). After that no further coordination is needed: the TT hardware itself guarantees `frame_start_ps[N+1] == frame_end_ps[N]`, i.e. reading is gapless as long as `read_stream()` keeps being called. In `'detection_count'` mode, the same idea holds a different way: the on-TT ring buffer is drop-newest with a 4M-detection cap, so as long as the host stops before that cap is hit, the first N detections it captured are guaranteed complete and gap-free, no matter how slow the host's own reads are.

**Finding the coincidence peak** (`coincidence_peak.py`) is the computational core, in three stages:

1. **Coarse search** --> **FFT cross-correlation**: bin each stream's timestamps into `coarse_bin_ps`-wide bins (a histogram of click counts per bin), subtract each histogram's mean, then compute

   $$\text{corr} = \mathrm{IFFT}\!\left(\mathrm{FFT}(\text{counts}_A) \cdot \overline{\mathrm{FFT}(\text{counts}_B)}\right)$$

   the standard identity that the cross-correlation at *every* possible lag, all at once, equals the inverse FFT of one spectrum times the other's complex conjugate. That turns an $O(N^2)$ "try every lag" search into an $O(N \log N)$ FFT over `n_bins = duration / coarse_bin_ps` bins. Peaks are scored against the noise floor with a robust z-score,

   $$\text{score} = \frac{\text{value} - \text{median}}{1.4826 \times \text{MAD}}$$

   rather than mean/stdev — a real coincidence peak would otherwise drag the mean and stdev along with it and hide itself. The handful of highest-scoring, well-separated lags go on to refinement.

2. **Refinement — exact timestamp differences.** For each coarse candidate, `_difference_histogram` builds an exact fine-grained histogram of $(B_i - A_j - \text{lag})$ for every pair within `radius_ps` of the candidate, using a two-pointer sliding window over the (already-sorted) timestamp arrays — $O(N)$, not $O(N^2)$. `_best_window` then picks whichever trial window width (0.5 ns .. 1 μs) maximizes a Poisson significance score

   $$\text{score} = \frac{N_{\text{window}} - B}{\sqrt{B}}$$

   where $B$ (`background`) is the sideband count rate, estimated from histogram bins far from the candidate center via the sideband *mean* (fine bins hold small integer counts, so the median would just quantize and distort it).

3. **Clock skew — segmented, background-subtracted linear fit.** The A stream is split into `segments` time chunks; in each, the true-coincidence pairs' mean arrival time is estimated by subtracting two symmetric sideband windows from the central one:

   $$\bar{t}_{\text{pair}} = \frac{\sum t_{\text{central}} - \tfrac{1}{2}\left(\sum t_{\text{left}} + \sum t_{\text{right}}\right)}{N_{\text{central}} - \tfrac{1}{2}\left(N_{\text{left}} + N_{\text{right}}\right)}$$

   A weighted least-squares line (`numpy.polyfit`, weight $= \sqrt{\text{score}}$) through (elapsed time, local lag) across all segments gives the clock skew directly as its slope — picoseconds of drift per second of elapsed time — with a second pass that drops $>4 \cdot \text{MAD}$ outlier segments and refits if enough remain. If fewer than 3 segments show a significant peak, `_grid_search_clock_skew` falls back to trying a grid of candidate slopes directly and keeping whichever one maximizes the drift-corrected window score.

The result — `lag_at_reference_ps` and `slope_ps_per_s` around `reference_elapsed_ps` — is the affine clock model `cmd/importtt` inverts (see its doc comment in `main.go`) to map every B timestamp onto A's clock before sifting.

### 1. Recording

```bash
pip install -r requirements.txt
python3 tt_record_dual.py
```

All configuration lives in `config.json` (both scripts read it; nothing is hardcoded in the source):

- `"sites"` — one entry per physical TimeTagger endpoint (`tt_ip`/`tt_port`/`auth_key_hex` plus its `channels`/`delays_ps`/`deadtimes_ps`/`triggers_volt`/`loop_period_s`). Add a new site here once, use it in any link.
- `"links"` — every known pair of sites that's actually been recorded together (e.g. `bratislava_nitra`, `bratislava_trnava`), each just `{"site_a": ..., "site_b": ...}`. This is the full set of possibilities that used to live as commented-out `TT_A`/`TT_B` blocks in the script; add a link here once it's been tried, and it stays selectable by name from then on.
- `"record_dual"` — which single link to record this run (`link`, any name from `"links"`), `stop_mode`/`record_duration_s`/`target_detection_count`, and `output_dir`. Only one link is ever recorded per run; `"links"` just makes every previously-known pair reachable by name instead of by editing the file.

Recording a different link, or a one-off duration, doesn't require editing the file — every `record_dual` setting has a CLI override:

```bash
python3 tt_record_dual.py                                        # config.json's record_dual.link, as-is
python3 tt_record_dual.py path/to/other-config.json              # a different config file
python3 tt_record_dual.py --link bratislava_nitra                # any named link from "links"
python3 tt_record_dual.py --site-a trencin --site-b trnava       # an ad hoc pair, bypassing "links"
python3 tt_record_dual.py --stop-mode detection_count --target-count 2000000
python3 tt_record_dual.py --help                                 # full flag list
```

This writes 6 files per run into `output_dir`, named `<timestamp>_<ip>_p<port>_*`:

- `*_timestamp_sequence.npy` — int64 picosecond timestamps, one per TT, sorted
- `*_channel_sequence.npy` — int8 physical channel per timestamp (1-4; other values, e.g. negative falling-edge tags, are not measurement clicks)
- `*_meta.json` — per-frame recording metadata (wall-clock time, uptime, drop warnings)

Notes from the field:

- `'duration'` mode is the one to use whenever the network can keep up with the detection rate (i.e. any link except the slow one) — it never drops data. `'detection_count'` mode is for a link that can't keep up: it relies on the TimeTagger's own on-device buffer (~4M detections, drop-newest) so a short capture at the front of the stream is guaranteed complete even if the host can't drain it fast enough.
- A link involving the slow site most likely drops detections in `'duration'` mode (the script warns you: `!! LIKELY LOST DATA` / `!! CAPPED`). If that happens, copy `tt_record_dual.py` + `tt_stream_external.py` to `/tmp` on that TimeTagger's own box and run the recording locally there instead.
- The TT connection occasionally fails for reasons not yet root-caused; it retries 3 times internally (`tt_stream_external.py`) before giving up — just rerun the script if it does.

### 2. Finding the coincidence peak

```bash
python3 coincidence_peak.py <recording directory>
python3 coincidence_peak.py --config config.json <recording directory>   # optional
```

The clock offset between the two recordings could be anywhere across the whole capture — searching that at picosecond precision directly would be far too slow, so this runs in two passes: a coarse FFT cross-correlation over binned counts narrows it down cheaply, then exact timestamp differences refine just that neighborhood to picosecond precision. It also fits the clock *skew* (drift) between the two TimeTaggers from several time segments, since a long enough recording will smear the coincidence window if only a constant offset is corrected for. Writes `<dir>/<dir>_coincidence.json` + a diagnostic plot. `--help` lists tuning knobs (bin sizes, search radius, memory cap); every one of them already has a sane default, but `--config` (see `config.json`'s `"coincidence_peak"` section) lets you save a preferred set of them instead of retyping flags — an explicit flag still overrides whatever the config file says. Needs both `.npy` files for one recording in the same directory; nothing else from this repo.

`example/20260909/` has a real 3-second dual-TT capture (included for testing `cmd/importtt` without a live link) plus its `coincidence_peak.py` output.

### 3. Running the post-processing chain

From the repo root:

```bash
go run ./cmd/importtt -dir acquisition/example/20260909
```

`cmd/importtt` reads the two recordings and the `*_coincidence.json` report from step 2, then:

- maps each channel to a basis/bit: channels 1-2 are one polarization basis (bits 0/1), channels 3-4 are the other — adjust `channelToBasisBit` in `cmd/importtt/main.go` if your detector wiring differs
- shifts the second recording's timestamps onto the first's clock using the report's fitted lag *and* clock skew (not just the constant offset), so drift over a long recording doesn't smear the coincidence window
- calls `qoj_post_processing.Run` with the aligned, labeled clicks and prints the same per-stage report as `cmd/qoj_post_processing`

`-window` overrides the coincidence window (defaults to the report's clock-skew-corrected window); `-matrix` and `-chunk` are the same step 2/3 knobs `cmd/qoj_post_processing` takes. Requires `coincidence_peak.py`'s default `--strict` (peak actually detected) — a report with `"detected": false` is refused, since its clock alignment isn't trustworthy.

### Optional: CHSH self-testing diagnostic

`chsh_diagnostic.py` is a separate standalone acquisition mode, not integrated with the QKD
sifting/key-generation chain above — a different post-processing script over the same raw
recording, for a CHSH/Bell self-testing analysis instead of a distilled key. It answers the
same alignment question `cmd/importtt` does (which clicks on A and B came from the same
photon pair) but reports per-channel-pair coincidence counts instead of sifting a key from
them, since the CHSH statistic needs $N_{++}, N_{+-}, N_{-+}, N_{--}$ per measurement-setting
combination, not just a single peak-vs-noise decision.

Run it the same way as `cmd/importtt`, after `tt_record_dual.py` + `coincidence_peak.py`:

```bash
python3 chsh_diagnostic.py <recording dir> --roi2-offset-ns <e.g. one laser rep period>
```

It shifts B's timestamps onto A's clock with the same affine lag+skew model `cmd/importtt`
uses, takes two equal-width regions of interest on the aligned coincidence histogram — ROI1
centered on the coincidence peak (default: `coincidence_peak.py`'s fitted peak center/width),
ROI2 an accidental/background baseline offset from it by `--roi2-offset-ns` (no safe default;
it depends on the source's repetition rate) — then walks the recording in `--block-s`
integration blocks. Each output row is one block:

- the singles count on each of the 8 detector channels (`A_H`/`A_V`/`A_D`/`A_A`,
  `B_H`/`B_V`/`B_D`/`B_A`, following the same channel 1-4 -> H/V/D/A wiring
  `cmd/importtt`'s `channelToBasisBit` assumes)
- for every cross-tagger channel pair (one A channel, one B channel — 4x4 = 16 pairs; same-side
  pairs aren't needed and aren't computed), the coincidence count in ROI1
- the same 16 counts in ROI2

Writes `<dir>/<dir>_chsh_blocks.csv` (the rows above) plus a `_chsh_blocks.json` sidecar
recording the resolved ROI centers/widths and the exact column order, so a downstream script
doesn't have to guess it. ROI1 minus a (width-scaled, if the two ROIs differ in width) ROI2
gives the accidental-subtracted coincidence count per pair per block, which is what feeds
$N_{++}, N_{+-}, N_{-+}, N_{--}$ per measurement-setting combination and the CHSH statistic and its uncertainty.

### Provenance

`tt_stream_external.py` is Quantum Optics Jena's TimeTagger interface client, included as provided (only the Python-version guard around its custom HMAC challenge is commented out — it works fine on newer Python than the check allowed). `tt_record_dual.py` and `coincidence_peak.py` are QUTE-side tooling built on top of it.
