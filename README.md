## qoj_post_processing

BBM92-style classical post-processing chain: coincidence **sifting**, block **error detection/correction**, CRC **verification**, **QBER** estimation, **privacy amplification** (Devetak-Winter rate + Toeplitz hashing), and optional **AEAD encryption** with the distilled key.

Library package (`import "qoj_post_processing"`); `cmd/qoj_post_processing` is a thin CLI driver that simulates click streams and calls `qoj_post_processing.Run(...)`. For real detector logs, build `[]qoj_post_processing.DetectionEvent` yourself and call `Run` directly.

### Layout

| file | step |
|---|---|
| `events.go` / `sifting.go` | `DetectionEvent`/`Basis` types, coincidence-window sifting |
| `blockdetect.go` | generator-matrix block parity detection (parallel, bit-packed), never flips a bit |
| `bch.go` | BCH-code generator matrix, a guaranteed-detection alternative |
| `matrixsearch.go` | `SearchGeneratorMatrix()`: hill-climbs a matrix against actual key rate |
| `winnow.go` / `pipeline_winnow.go` | Winnow error *correction* (`RunWinnow()`), a different tradeoff than block discard |
| `ldpc.go` / `pipeline_ldpc.go` | LDPC belief-propagation error correction (`RunLDPC()`) |
| `crcverify.go` / `qber.go` | CRC-16 chunk verification, QBER estimation |
| `toeplitz*.go` / `privacyamp.go` | Toeplitz hash (hardware-CLMUL on amd64) + privacy amplification |
| `encryption.go` | AEAD encrypt/decrypt with the distilled key |
| `keystore.go` | splits a key into 256-bit ETSI GS QKD 014 records, saves/loads them |
| `pipeline.go` | `Run()`: orchestrates steps 1-5 |
| `cmd/tuneblock` | compares generator matrices for the highest key rate |
| `cmd/importtt` | runs the chain over a real dual-TimeTagger recording (see `acquisition/`) |
| `acquisition/` | Python: records from two TimeTaggers, finds their clock offset, plus a standalone CHSH/Bell diagnostic |
| `ui/` | a small dashboard: run the pipeline, watch metrics, check live alignment on recordings |

### Quick start

```bash
go build ./...
go run ./cmd/qoj_post_processing -n 300000 -err 0.005 -chunk 3000
```

Key flags (`-h` for all): `-n` raw attempts, `-err` intrinsic bit error rate, `-window` coincidence window (ps), `-matrix bch` (default) or `-search-matrix`, `-winnow`/`-ldpc` to correct errors instead of discarding blocks, `-chunk` step 3 CRC chunk size.

### Real detector data

```bash
cd acquisition && python3 tt_record_dual.py                    # record from two TTs
python3 coincidence_peak.py <recording dir>                    # find the clock offset/skew
cd .. && go run ./cmd/importtt -dir acquisition/<recording dir> # sift --> ... --> final key
```

See `acquisition/README.md` for the full workflow and network caveats.

### Saving the final key

Both CLIs take `-keystore <dir>`: the distilled key is sliced into 256-bit chunks, each written as its own `<dir>/<key_ID>.json` -- the record shape ETSI GS QKD 014's Key Delivery API returns. Local storage only, no REST API or SAE routing -- `qkd.SaveKeys256(dir, res.FinalKeyBytes)` / `qkd.LoadKey(dir, keyID)` in `keystore.go`, for a KMS layer built on top.

### Library usage

```go
import qkd "qoj_post_processing"

cfg := qkd.Config{CoincidenceWindowPS: 500, GeneratorMatrix: qkd.GeneratorMatrixBCHt2, ChunkSize: 2048}
res, err := qkd.Run(aliceEvents, bobEvents, cfg)
res.FinalKeyBytes // ready to feed into your KMS, or into EncryptWithKey below

ciphertext, err := qkd.EncryptWithKey(res.FinalKeyBytes, plaintext) // AES-256-GCM, HKDF-derived key
plaintext, err = qkd.DecryptWithKey(res.FinalKeyBytes, ciphertext)
```

### Step 2: error detection vs. correction

The sifted key splits into `m`-bit blocks; each side computes `p` parity bits as $P = M \cdot G^{T}$ (mod 2) from a shared generator matrix `G`. Only parity crosses the channel. **Block discard** (the default) drops a block on any disagreement, never flipping a bit -- the simplest, most conservative option. `BCHGeneratorMatrix`/`GeneratorMatrixBCHt2` gives a proven Hamming-weight detection guarantee instead of an empirically tuned one; `CascadeGeneratorMatrices(g1, g2)` stacks two matrices to catch collisions invisible to either alone.

**Winnow** (`winnow.go`) and **LDPC belief propagation** (`ldpc.go`) *correct* errors instead of discarding, at the cost of leaking more per block and (Winnow) needing several passes. Neither is a strict upgrade over discard --> measure on your own data. Both need a much smaller CRC chunk size than discard's default, or step 3 discards nearly everything; `cmd/importtt -winnow`/`-ldpc` (and `cmd/qoj_post_processing` the same way) set this automatically.

`Sift`, `BlockErrorDetect`, and `CRCVerify` are combined convenience wrappers; each has a split, one-sided form (`ComputeBlockParity`/`ReconcileBlocks`, `ComputeChunkCRC`/`ReconcileChunks`) for running Alice and Bob as two independent processes that never share raw bits.

### Searching for a better matrix than any fixed construction

`SearchGeneratorMatrix` hill-climbs a matrix directly against final key rate instead of using a fixed construction, and reliably beats BCH by 20-50%+ depending on QBER (`make tune` / `matrixsearch_test.go` show current numbers on your machine).

**Never search directly on the real bits you intend to key from**: matrix it picks depends on the exact data it's scored against; if that's a real session's own secret bits, the matrix choice itself leaks information `PrivacyAmplify` never accounts for. Both CLIs enforce the safe order automatically: `-search-matrix` designs the matrix on independently-simulated proxy data first, then applies that fixed matrix to the real run --> exactly like designing a BCH matrix offline before ever deploying it.

### Makefile targets

```
make build     # go build ./...
make test      # go test ./... -v
make test-all  # build+vet+fmt+race tests+cross-arch build, see scripts/test.sh
make bench     # go test -run '^$' -bench . -benchmem ./...
make tune      # sweep step 2 block size for the highest key rate
make run       # go run ./cmd/qoj_post_processing with sane demo defaults
make check     # fmt+vet+build+test+run in one shot
make vet       # go vet ./...
make fmt       # gofmt -l . (fails if anything is unformatted)
make clean     # remove build artifacts
```

### Docker

```bash
docker build -t qoj-post-processing .
docker run --rm qoj-post-processing qoj_post_processing -n 300000 -err 0.005 -chunk 3000
docker run --rm -v "$(pwd)/acquisition:/app/acquisition" qoj-post-processing importtt -dir acquisition/example/20260909
```

One image, no server: Python 3.12 + Go 1.24, with `qoj_post_processing`/`importtt`/`tuneblock` prebuilt to `/usr/local/bin`. Mount `acquisition/` whenever a command needs to read or write a recording. For the dashboard instead, see `ui/` (separate Docker + Caddy deployment).
