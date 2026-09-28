## qoj_post_processing

BBM92-style classical post-processing chain: coincidence **sifting**, block **error detection** (generator-matrix parity, never bit-flipping), CRC **verification**, **QBER** estimation, **privacy amplification** (Devetak-Winter rate + Toeplitz hashing), and optional **AEAD encryption** with the distilled key.

Library package (`import "qoj_post_processing"`); `cmd/qoj_post_processing` is a thin CLI driver that simulates click streams and calls `qoj_post_processing.Run(...)`. For real detector logs, build `[]qoj_post_processing.DetectionEvent` yourself and call `Run` directly.

### Layout

| file | step |
|---|---|
| `events.go` | shared `DetectionEvent`/`Basis` types |
| `sifting.go` | coincidence-window sifting |
| `blockdetect.go` | generator-matrix block parity detection (parallel, bit-packed) |
| `bch.go` | BCH-code generator matrices, a guaranteed-detection alternative |
| `matrixsearch.go` | `SearchGeneratorMatrix()`: hill-climbs a matrix against actual key rate instead of a fixed construction |
| `winnow.go` | Winnow error correction --> a different tradeoff than steps 2/3 |
| `crcverify.go` | CRC-16 chunk verification (parallel) |
| `qber.go` | QBER from discarded blocks over the whole sifted key |
| `toeplitz.go` | bit-packed, parallel Toeplitz universal hash (portable) |
| `toeplitz_clmul_amd64.{go,s}` | hardware-CLMUL Toeplitz hash (amd64 only) |
| `privacyamp.go` | secure key length + privacy amplification |
| `encryption.go` | AEAD encrypt/decrypt with the distilled key |
| `pipeline.go` | `Run()`: orchestrates steps 1-5 |
| `pipeline_winnow.go` | `RunWinnow()`: `Run()`'s error-correcting sibling, steps 2/3 replaced by Winnow |
| `ldpc.go` | sparse regular LDPC matrix construction, syndrome, min-sum belief-propagation decoder |
| `pipeline_ldpc.go` | `RunLDPC()`: `Run()`'s other error-correcting sibling, steps 2/3 replaced by LDPC |
| `keystore.go` | splits a finished key into 256-bit ETSI GS QKD 014 key records (`key_ID` + base64 `key`), saves/loads them |
| `cmd/tuneblock` | compares the generator matrices for the highest key rate |
| `cmd/importtt` | runs `Run()` (or `-winnow`/`-ldpc` for `RunWinnow()`/`RunLDPC()`) over a real dual-TimeTagger recording (see `acquisition/`) |
| `acquisition/` | Python: records from two TimeTaggers, finds the clock offset between them, and (`chsh_diagnostic.py`) an unrelated standalone CHSH/Bell self-testing diagnostic over the same recordings |
| `Dockerfile` | packages the Go binaries and Python acquisition scripts into one image, no server, see Docker below |

### Quick start

```bash
go build ./...
go run ./cmd/qoj_post_processing -n 300000 -err 0.005 -matrix ex3 -chunk 3000
```

Key flags (`-h` for all): `-n` raw attempts, `-err` intrinsic bit error rate, `-window` coincidence window (ps), `-matrix` step 2 generator matrix (`ex1`/`ex2`/`ex3`), `-chunk` step 3 CRC chunk size.

### Real detector data

`cmd/qoj_post_processing` simulates click streams; to run the chain over an actual dual-TimeTagger recording instead:

```bash
cd acquisition && python3 tt_record_dual.py                    # record from two TTs
python3 coincidence_peak.py <recording dir>                    # find the clock offset/skew
cd .. && go run ./cmd/importtt -dir acquisition/<recording dir> # sift --> ... --> final key
```

See `acquisition/README.md` for the full workflow, network caveats, and how channels map to BBM92 basis/bit.

### Saving the final key

Both CLIs take `-keystore <dir>`: the distilled key is sliced into 256-bit chunks (`ETSI014KeySizeBytes`), each written as its own `<dir>/<key_ID>.json` file `{"key_ID": "<uuid>", "key": "<base64>"}` -- the key record shape and size ETSI GS QKD 014's Key Delivery API returns from `GET .../enc_keys` and `.../dec_keys`. `key_ID` is a fresh random UUIDv4 per key, and any trailing remainder under 256 bits is discarded rather than padded or shrunk into an under-length key. This is local storage only, not the REST API itself: no network server, no SAE ID routing --> just `qkd.SaveKeys256(dir, res.FinalKeyBytes)` / `qkd.LoadKey(dir, keyID)` in `keystore.go`, for a KMS layer built on top to read from.

```bash
go run ./cmd/qoj_post_processing -n 300000 -err 0.005 -keystore ./keys
go run ./cmd/importtt -dir acquisition/<recording dir> -keystore ./keys
```

### Library usage

```go
import qkd "qoj_post_processing"

cfg := qkd.Config{CoincidenceWindowPS: 500, GeneratorMatrix: qkd.GeneratorMatrixEx3, ChunkSize: 2048}
res, err := qkd.Run(aliceEvents, bobEvents, cfg)
res.FinalKeyBytes // ready to feed into your KMS, or into EncryptWithKey below

ciphertext, err := qkd.EncryptWithKey(res.FinalKeyBytes, plaintext) // AES-256-GCM, HKDF-derived key
plaintext, err = qkd.DecryptWithKey(res.FinalKeyBytes, ciphertext)
```

### Step 2: generator-matrix error detection

The sifted key is split into fixed `m`-bit blocks; at each side, `p` parity bits are computed independently as $P = M \cdot G^{T}$ (mod 2) from a shared `p x m` generator matrix `G`. Only the parity bits ever need to cross the public channel; if they disagree the whole block is discarded, never flipped. `GeneratorMatrixEx1`/`Ex2`/`Ex3` are three example matrices with different code-rate/detection-rate tradeoffs; pass any `p x m` `[][]byte` of your own via `Config.GeneratorMatrix`. `ComputeBlockParity` and `ReconcileBlocks` expose this as two one-sided calls, one side never needs the other's raw bits, only its parity, so two independently deployed nodes can run step 2 without a shared process; `BlockErrorDetect(alice, bob, g)` is a convenience wrapper over both for simulation and testing. QBER (step 4) is erroneous bits in the discarded blocks over *all* bits of the sifted key, not just the discarded portion.

Any single matrix has blind spots: two different blocks can land on the same parity (a collision), so an error between them goes undetected. `CascadeGeneratorMatrices(g1, g2)` stacks a second matrix's rows onto the first, so a block only survives if it matches under *both* --> catching errors invisible to `g1` alone, at essentially no extra cost since it reuses the exact same word-packed pass instead of a second scan. Needs `g1`/`g2` to share a block size and `p1+p2 < m`, or the combined leak would exceed the block and leave no secret.

`BCHGeneratorMatrix(gfBits, t, blockSize)` builds a matrix a different way: instead of an empirically tabulated code, it's a shortened binary BCH parity-check matrix over $GF(2^{\text{gfBits}})$, which comes with a proven guarantee (the BCH bound) that every error pattern of Hamming weight $\le t$ is caught, not just most of them, the same kind of guarantee the block-detection step made before it was rebuilt around this patent's tunable matrices, just derived from coding theory instead of an ad hoc extended-Hamming construction. `GeneratorMatrixBCHt2` is a ready-to-use instance (32-bit block, catches every 1-4 bit error, rate 0.44) wired into `-matrix bch` and `make tune` alongside `ex1`/`ex2`/`ex3`. It doesn't automatically win: a stronger per-block guarantee over a larger block means more blocks contain at least one error in the first place, so at a given QBER it can lose on overall key rate to a smaller, weaker-but-tighter matrix, `make tune` shows this trade honestly rather than picking a side. Plugs into every existing step-2 function unchanged, since it's just another `GeneratorMatrix`.

### Winnow: error correction instead of discard

Everything above never flips a bit, that's the patent's core requirement and this project's central security property. `winnow.go` is a deliberate exception, kept fully separate: the Winnow protocol (Buttler et al., 2003) *corrects* errors instead of discarding blocks, using the same parity math but a different matrix and a different trick. `HammingGeneratorMatrix(blockSize)` builds the minimal matrix where column `j` is `binary(j+1)`, so `own syndrome XOR peer syndrome` decodes directly to the differing bit's position, no bisection, one exchange per pass, `WinnowCorrect` flips that bit toward the peer's value. This only reliably fixes a block with *exactly one* real error; two errors in a block can decode to the wrong position and make things worse, which is why real deployments always follow it with a verification pass, `CRCVerify` already is one. `WinnowReconcile` runs several passes with a fresh `GeneratePermutation` between each, so errors that shared a block in one pass land in different blocks the next; both sides derive the same permutation from the same publicly-agreed seed, so nothing but syndromes crosses the channel. Built entirely on `ComputeBlockParity`, so it gets the lookup-table fast path for free, the cost is proportional to the number of passes (5 passes ≈ 8x one `BlockErrorDetect` call on a 1M-bit key, `BenchmarkWinnowReconcile_1M`), which is the honest price of doing several full reconciliation rounds, not overhead to trim.

`pipeline_winnow.go`'s `RunWinnow()` wires this into a full end-to-end alternative to `Run()`: sift, sacrifice a random public sample of the sifted key to measure QBER directly (no discarded blocks left to read it off), Winnow-correct the rest, CRC-verify, privacy-amplify. `cmd/importtt -winnow -winnow-blocks 8,16,32 -winnow-sample 0.05` runs it over a real recording. **It is not a strict upgrade over block discard**: `WinnowReconcile` re-reveals parity for the *entire* array on every pass, where block discard only pays for the blocks that survive, so Winnow's overhead scales with pass count regardless of how few blocks still have errors --> pick a method by measuring on your own data, not by assuming "corrects > discards". Winnow's CRC chunk size also needs to be much smaller than block discard's (its output isn't guaranteed error-free the way a survived block-discard block is), or CRC discards nearly everything.

### LDPC: error correction via belief propagation

`ldpc.go` is a second deliberate exception to the never-flip rule, an alternative to Winnow rather than to block discard. `BuildRegularLDPC(n, wc, wr, seed)` builds a sparse regular parity-check matrix (Gallager's construction, with bounded retries to avoid 4-cycles on graphs small enough for that to matter) once per code shape; `LDPCPass`/`LDPCReconcile` then need only *one* syndrome reveal per block regardless of QBER, where Winnow re-reveals parity for the whole array on every pass. Decoding is min-sum belief propagation with a normalization factor (plain min-sum overstates its own confidence and diverges; this is the standard fix), parallelized per iteration across `runtime.GOMAXPROCS(0)` workers. `pipeline_ldpc.go`'s `RunLDPC()` wires this into a full alternative to `Run()`/`RunWinnow()`: sift, sacrifice a public sample for QBER (same as Winnow), LDPC-correct, CRC-verify (a block that doesn't converge, or converges to the wrong codeword, still gets caught here), privacy-amplify. `cmd/importtt -ldpc -ldpc-wc 3 -ldpc-wr 10` runs it over a real recording; `BlockLength` (`-ldpc-block`) defaults to one block covering the whole remaining key, the realistic efficient case.

**Rate needs a real safety margin, not just $1 - w_c/w_r < 1 - h_2(\text{QBER})$.** A regular code's actual iterative-decoding threshold runs below the Shannon bound, more so for low `wc` — `ldpc_test.go` found a rate that converged reliably at 4,000 bits failed at 1,000,000 bits and the *same* QBER, because small blocks can luck into convergence a large block's true threshold doesn't support. Tune empirically at the block size you'll actually use, not just algebraically.

### Running as two independent nodes

`Sift`, `BlockErrorDetect`, and `CRCVerify` are convenience wrappers that run both sides in one call, for simulation and testing. Each has a split form that never needs the peer's secret bits, only public metadata or already-computed check values, so Alice and Bob can run as two separate processes exchanging only what the protocol requires:

- **Sifting**: timestamps and bases aren't secret. `StripBit(events)` gives the public `[]PublicEvent` view; exchange it, then both sides call `MatchCoincidences(alicePublic, bobPublic, windowPS)` locally and get identical `[]MatchedPair` back - no reconciliation step needed, each side just reads its own bit at its own index.
- **Block detection**: `ComputeBlockParity(ownBits, g)` locally, exchange parity, `ReconcileBlocks(ownBits, ownParity, peerParity, blockSize)` locally.
- **CRC verification**: `ComputeChunkCRC(ownBits, chunkSize)` locally, exchange CRCs, `ReconcileChunks(ownBits, ownCRC, peerCRC, chunkSize)` locally.

`Run()` still uses the combined wrappers end-to-end; wire the split calls together yourself over your own authenticated channel if you need Alice and Bob as separate processes.

### Performance

Steps 2 and 3 split per-block/per-chunk work across a `runtime.GOMAXPROCS(0)` worker pool. Step 2's parity is the hot path in practice, since the patent's own matrices use tiny blocks (`m=4`/`6`) against real sifted keys, meaning huge block counts: for `m<=16`, `ComputeBlockParity` precomputes every possible block's parity once into a `2^m`-entry lookup table (`<=64KB`, L1/L2-resident), so per-block work becomes a single array lookup instead of `p` AND+POPCNT passes --> exact, since a block code has finitely many inputs, verified bit-for-bit against the POPCNT path for every possible block under every shipped matrix. `ReconcileBlocks` and `ComputeChunkCRC`/`ComputeBlockParity` write into one flat pre-sized backing array per call (offsets computed by a cheap serial prefix-sum pass) instead of one small heap allocation per block, and the copy into `Surviving`/`Discarded` is itself parallelized. Net effect on a 1M-bit key: `BlockErrorDetect` roughly halves in wall time and drops from ~330K allocations to about 100 (`make bench` / `BenchmarkBlockErrorDetect_1M` on your machine).

Step 3's own bit-packing (turning 0/1-per-byte chunks into real bytes before CRC-16) used to be a one-bit-at-a-time loop with a data-dependent branch, on essentially random key material that branch mispredicts roughly every other bit. It's now 8 bits per iteration via a single big-endian word load and a fixed shift-mask-or per lane, no branch at all (`TestBitsToBytesIntoMatchesNaive` checks it against the original bit-at-a-time logic across every possible tail length). The checksum itself is CRC-16/CCITT-FALSE, table-driven, byte at a time --> no stdlib package covers a 16-bit CRC, so it's a small self-contained table build plus lookup, verified against the standard test vector. This matches `docs/bbm92_details_post_processing.pdf`'s own step-3 mechanism (a 16-bit CRC over a 256-bit chunk, leaking 1/16 of the surviving stream) --> previously this was a full CRC-32, leaking twice as many bits per chunk than the protocol calls for. Measured on a 1M-bit key: `CRCVerify` runs in about 0.63 ms (`BenchmarkCRCVerify_1M`, `BenchmarkBitsToBytesInto_2048`).

Step 5's Toeplitz hash is the quadratic-shaped bottleneck (`O(ln)`): on amd64 with `PCLMULQDQ` (checked at runtime), the whole output is one hardware carry-less-multiply polynomial product instead of a popcount per output bit --> `2-10x` over the portable `POPCNT` fallback used everywhere else. All of the above parallelize only past a measured work-size threshold --> below it, goroutine overhead costs more than it saves, so small batches just run single-threaded. `toeplitzHashNaive` is kept only as a correctness oracle for tests, never use it in production. Run `make bench` for numbers on your machine.

### Tuning

This is detect-and-discard, never-flip, so step 2's generator matrix trades survival rate against leaked parity bits: `Ex1`/`Ex2` (`m=4, p=3`) survive more often per-block but leak proportionally more than `Ex3` (`m=6, p=3`, code rate 2/3). At high QBER the final key can hit zero: that's expected, not a bug.

`make tune` (or `./scripts/tune-blocksize.sh -h`) runs all four matrices (`ex1`/`ex2`/`ex3`/`bch`) over one fixed simulated batch and reports which gives the highest key rate (final key bits per raw attempt), plus how many fixed-size keys (`-keybits`, default 256) that batch yields at each.

### Searching for a better matrix than any fixed construction

`matrixsearch.go`'s `SearchGeneratorMatrix` doesn't use a fixed construction at all: it hill-climbs (with occasional simulated-annealing-style worse-move acceptance to escape local optima) a random `p x m` matrix directly against the metric that actually matters, final key bits per sifted bit, evaluated on a fixed batch via the same `BlockErrorDetect` to `PrivacyAmplify` stages `Run()` uses. `Ex1`/`Ex2`/`Ex3` are empirical spot checks and BCH optimizes a worst-case guarantee (every error of weight $\le t$ detected); neither directly targets the number that ends up mattering, so at a given real QBER both can leave rate on the table.

`./cmd/tuneblock -search` adds this as a candidate in the same sweep table, `-search-m`/`-search-p` pick its size and `-search-iterations` its search budget. At the default `-err 0.03`, a `search(32,6)` matrix trained on one 300,000-attempt batch beat `Ex3` (the best fixed baseline) by 1.24x-1.98x on *held-out* batches the search never saw (`TestSearchGeneratorMatrixBeatsEx3OutOfSample`), checked out-of-sample specifically because the in-sample number is inflated by overfitting to that one batch's noise, and the fixed matrices don't have that problem to begin with since they were never fit to any data. Mechanistically: at that block size and QBER, both `BCH` (`p=18`) and the searched matrix (`p=6`) end up dropping almost exactly the same blocks, meaning `BCH`'s extra 12 parity bits are buying essentially no additional detection, just leaking more of the surviving key. **Always re-run the search on your own data before trusting a number**, it optimizes for whatever batch you hand it, and a batch from a different link/QBER can favor a different `(m, p)`.

**Never search directly on the real bits you intend to key from**: `SearchGeneratorMatrix` needs both sides' raw bits to score a candidate, so if you point it at an actual session's real sifted key, the matrix it picks is chosen *after seeing* that exact secret data. `PrivacyAmplify`'s leak accounting only charges for the parity/CRC bits explicitly revealed; it has no way to charge for information encoded in *which matrix got chosen*, so a key distilled this way isn't secure even though the pipeline reports one. `Ex1`/`Ex2`/`Ex3`/BCH don't have this problem because they're fixed before ever seeing any session's data. Both CLIs enforce the safe order the same way: they design the matrix by searching on `-search-offline-err`-simulated proxy data (independent of the actual run), then apply that fixed matrix to the real data --> `cmd/importtt -search-matrix` for a real recording, `cmd/qoj_post_processing -search-matrix` for its own simulated click stream (useful for trying the search workflow, or comparing it against `-matrix ex3`/`bch`, without a real recording on hand), exactly like designing a new BCH matrix offline and only then deploying it. On the repo's bundled `acquisition/example/20260909` (605 sifted bits, roughly 6.5-6.8% QBER from a 3-second capture), a matrix designed this legitimate way still lands at 0 final key bits, same as every other method here, that recording is simply too small and noisy for any of them to distill a secure key from, not a bug.

### Makefile targets

```
make build     # go build ./...
make test      # go test ./... -v
make test-all  # build+vet+fmt+race tests+cross-arch build, see scripts/test.sh
make bench     # go test -run '^$' -bench . -benchmem ./...
make tune      # sweep step 2 block size for the highest key rate
make run       # go run ./cmd/qoj_post_processing with sane demo defaults
make vet       # go vet ./...
make fmt       # gofmt -l . (fails if anything is unformatted)
make clean     # remove build artifacts
```

### Docker

```bash
docker build -t qoj-post-processing .
docker run --rm qoj-post-processing qoj_post_processing -n 300000 -err 0.005 -matrix ex3 -chunk 3000
docker run --rm -v "$(pwd)/acquisition:/app/acquisition" qoj-post-processing importtt -dir acquisition/example/20260909
```

One image, no server, no UI: Python 3.12 + a matching Go 1.24 toolchain, `acquisition/requirements.txt` installed, and `qoj_post_processing`/`importtt`/`tuneblock` prebuilt to `/usr/local/bin` at image-build time, so a container starts instantly and neither the Go toolchain nor the source tree is needed at run time to use them. `docker run --rm -v "$(pwd)/acquisition:/app/acquisition" qoj-post-processing python3 acquisition/tt_record_dual.py` (and `coincidence_peak.py`/`chsh_diagnostic.py`) work the same way for the Python side; mount `acquisition/` whenever a command needs to read or write a recording, or the container's writes disappear with it.
