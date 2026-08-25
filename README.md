## qkdpostproc

BBM92-style classical post-processing chain: coincidence **sifting**, block **error detection** (generator-matrix parity, never bit-flipping), CRC **verification**, **QBER** estimation, **privacy amplification** (Devetak-Winter rate + Toeplitz hashing), and optional **AEAD encryption** with the distilled key.

Library package (`import "qkdpostproc"`); `cmd/qkdpostproc` is a thin CLI driver that simulates click streams and calls `qkdpostproc.Run(...)`. For real detector logs, build `[]qkdpostproc.DetectionEvent` yourself and call `Run` directly.

### Layout

| file | step |
|---|---|
| `events.go` | shared `DetectionEvent`/`Basis` types |
| `sifting.go` | coincidence-window sifting |
| `blockdetect.go` | generator-matrix block parity detection (parallel, bit-packed) |
| `bch.go` | BCH-code generator matrices, a guaranteed-detection alternative |
| `crcverify.go` | CRC-32 chunk verification (parallel) |
| `qber.go` | QBER from discarded blocks over the whole sifted key |
| `toeplitz.go` | bit-packed, parallel Toeplitz universal hash (portable) |
| `toeplitz_clmul_amd64.{go,s}` | hardware-CLMUL Toeplitz hash (amd64 only) |
| `privacyamp.go` | secure key length + privacy amplification |
| `encryption.go` | AEAD encrypt/decrypt with the distilled key |
| `pipeline.go` | `Run()`: orchestrates steps 1-5 |
| `cmd/tuneblock` | compares the generator matrices for the highest key rate |

### Quick start

```bash
go build ./...
go run ./cmd/qkdpostproc -n 300000 -err 0.005 -matrix ex3 -chunk 3000
```

Key flags (`-h` for all): `-n` raw attempts, `-err` intrinsic bit error rate, `-window` coincidence window (ps), `-matrix` step 2 generator matrix (`ex1`/`ex2`/`ex3`), `-chunk` step 3 CRC chunk size.

### Library usage

```go
import qkd "qkdpostproc"

cfg := qkd.Config{CoincidenceWindowPS: 500, GeneratorMatrix: qkd.GeneratorMatrixEx3, ChunkSize: 2048}
res, err := qkd.Run(aliceEvents, bobEvents, cfg)
res.FinalKeyBytes // ready to feed into your KMS, or into EncryptWithKey below

ciphertext, err := qkd.EncryptWithKey(res.FinalKeyBytes, plaintext) // AES-256-GCM, HKDF-derived key
plaintext, err = qkd.DecryptWithKey(res.FinalKeyBytes, ciphertext)
```

### Step 2: generator-matrix error detection

The sifted key is split into fixed `m`-bit blocks; at each side, `p` parity bits are computed independently as $P = M \cdot G^{T}$ (mod 2) from a shared `p x m` generator matrix `G`. Only the parity bits ever need to cross the public channel; if they disagree the whole block is discarded, never flipped. `GeneratorMatrixEx1`/`Ex2`/`Ex3` are three example matrices with different code-rate/detection-rate tradeoffs; pass any `p x m` `[][]byte` of your own via `Config.GeneratorMatrix`. `ComputeBlockParity` and `ReconcileBlocks` expose this as two one-sided calls - one side never needs the other's raw bits, only its parity - so two independently deployed nodes can run step 2 without a shared process; `BlockErrorDetect(alice, bob, g)` is a convenience wrapper over both for simulation and testing. QBER (step 4) is erroneous bits in the discarded blocks over *all* bits of the sifted key, not just the discarded portion.

Any single matrix has blind spots: two different blocks can land on the same parity (a collision), so an error between them goes undetected. `CascadeGeneratorMatrices(g1, g2)` stacks a second matrix's rows onto the first, so a block only survives if it matches under *both* --> catching errors invisible to `g1` alone, at essentially no extra cost since it reuses the exact same word-packed pass instead of a second scan. Needs `g1`/`g2` to share a block size and `p1+p2 < m`, or the combined leak would exceed the block and leave no secret.

`BCHGeneratorMatrix(gfBits, t, blockSize)` builds a matrix a different way: instead of an empirically tabulated code, it's a shortened binary BCH parity-check matrix over `GF(2^gfBits)`, which comes with a proven guarantee (the BCH bound) that every error pattern of Hamming weight `<= t` is caught, not just most of them, the same kind of guarantee the block-detection step made before it was rebuilt around this patent's tunable matrices, just derived from coding theory instead of an ad hoc extended-Hamming construction. `GeneratorMatrixBCHt2` is a ready-to-use instance (32-bit block, catches every 1-4 bit error, rate 0.44) wired into `-matrix bch` and `make tune` alongside `ex1`/`ex2`/`ex3`. It doesn't automatically win: a stronger per-block guarantee over a larger block means more blocks contain at least one error in the first place, so at a given QBER it can lose on overall key rate to a smaller, weaker-but-tighter matrix, `make tune` shows this trade honestly rather than picking a side. Plugs into every existing step-2 function unchanged, since it's just another `GeneratorMatrix`.

### Running as two independent nodes

`Sift`, `BlockErrorDetect`, and `CRCVerify` are convenience wrappers that run both sides in one call, for simulation and testing. Each has a split form that never needs the peer's secret bits, only public metadata or already-computed check values, so Alice and Bob can run as two separate processes exchanging only what the protocol requires:

- **Sifting**: timestamps and bases aren't secret. `StripBit(events)` gives the public `[]PublicEvent` view; exchange it, then both sides call `MatchCoincidences(alicePublic, bobPublic, windowPS)` locally and get identical `[]MatchedPair` back - no reconciliation step needed, each side just reads its own bit at its own index.
- **Block detection**: `ComputeBlockParity(ownBits, g)` locally, exchange parity, `ReconcileBlocks(ownBits, ownParity, peerParity, blockSize)` locally.
- **CRC verification**: `ComputeChunkCRC(ownBits, chunkSize)` locally, exchange CRCs, `ReconcileChunks(ownBits, ownCRC, peerCRC, chunkSize)` locally.

`Run()` still uses the combined wrappers end-to-end; wire the split calls together yourself over your own authenticated channel if you need Alice and Bob as separate processes.

### Performance

Steps 2 and 3 split per-block/per-chunk work across a `runtime.GOMAXPROCS(0)` worker pool. Step 2's parity is the hot path in practice, since the patent's own matrices use tiny blocks (`m=4`/`6`) against real sifted keys, meaning huge block counts: for `m<=16`, `ComputeBlockParity` precomputes every possible block's parity once into a `2^m`-entry lookup table (`<=64KB`, L1/L2-resident), so per-block work becomes a single array lookup instead of `p` AND+POPCNT passes --> exact, since a block code has finitely many inputs, verified bit-for-bit against the POPCNT path for every possible block under every shipped matrix. `ReconcileBlocks` and `ComputeChunkCRC`/`ComputeBlockParity` write into one flat pre-sized backing array per call (offsets computed by a cheap serial prefix-sum pass) instead of one small heap allocation per block, and the copy into `Surviving`/`Discarded` is itself parallelized. Net effect on a 1M-bit key: `BlockErrorDetect` roughly halves in wall time and drops from ~330K allocations to about 100 (`make bench` / `BenchmarkBlockErrorDetect_1M` on your machine). Step 5's Toeplitz hash is the quadratic-shaped bottleneck (`O(ln)`): on amd64 with `PCLMULQDQ` (checked at runtime), the whole output is one hardware carry-less-multiply polynomial product instead of a popcount per output bit --> `2-10x` over the portable `POPCNT` fallback used everywhere else. All of the above parallelize only past a measured work-size threshold --> below it, goroutine overhead costs more than it saves, so small batches just run single-threaded. `toeplitzHashNaive` is kept only as a correctness oracle for tests, never use it in production. Run `make bench` for numbers on your machine.

### Tuning

This is detect-and-discard, never-flip, so step 2's generator matrix trades survival rate against leaked parity bits: `Ex1`/`Ex2` (`m=4, p=3`) survive more often per-block but leak proportionally more than `Ex3` (`m=6, p=3`, code rate 2/3). At high QBER the final key can hit zero: that's expected, not a bug.

`make tune` (or `./scripts/tune-blocksize.sh -h`) runs all three matrices over one fixed simulated batch and reports which gives the highest key rate (final key bits per raw attempt), plus how many fixed-size keys (`-keybits`, default 256) that batch yields at each.

### Makefile targets

```
make build     # go build ./...
make test      # go test ./... -v
make test-all  # build+vet+fmt+race tests+cross-arch build, see scripts/test.sh
make bench     # go test -run '^$' -bench . -benchmem ./...
make tune      # sweep step 2 block size for the highest key rate
make run       # go run ./cmd/qkdpostproc with sane demo defaults
make vet       # go vet ./...
make fmt       # gofmt -l . (fails if anything is unformatted)
make clean     # remove build artifacts
```
