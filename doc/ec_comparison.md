### Error-correction comparison

**Status**: measured with `scripts/ec_compare.py`: every method on the same simulated binary symmetric channel, $2^{20}$ sifted bits per point, all eight cores of one machine. The efficiency is $f = \mathrm{leak} / (n\,h_2(\mathrm{QBER}))$, the bits each method discloses over the Shannon limit, counting every syndrome or frozen bit, every hash tag and one bit for each of Bob's verdicts; $f = 1$ is the theoretical optimum. The secret fraction is the asymptotic key left per sifted bit, $(1 - \mathrm{fail})(1 - h_2) - f\,h_2$, so a method pays both for what it discloses and for the blocks it fails to correct.

### Methods

| Method | What it is | Where |
|---|---|---|
| Cascade, optimized | Brassard-Salvail Cascade as optimized by Martinez-Mateo et al. (QIC 15, 2015): first blocks of $2^{\lceil \log_2(0.75/\mathrm{QBER}) \rceil}$ bits growing x4 over 3 passes, then half-frame passes until 8 in a row find nothing; no parity disclosed twice, the other half of every split inferred, each correction reopening earlier blocks shortest first; the default wherever both strings are on one machine | `qkd/cascade.go` |
| Winnow | Buttler et al. (2003): parity per block, a Hamming syndrome only where parities differ, blocks from the QBER doubling each pass | `qkd/winnow.go` |
| Polar SC, SCL8, SCL32 | rate-adaptive polar codes, successive cancellation and list decoding | `qkd/polar.go`, `qkd/softrecon.go` |
| LDPC regular | random (3, w) Gallager codes at a fixed rate 1.25 times the Shannon limit, no rate adaptation | `qkd/ldpc.go` |
| 5G NR LDPC | the 3GPP TS 38.212 base graph 1 lifted by Z = 384 (matrices from the `py3gpp` library), decoded by the `ldpc` library's belief propagation (exact sum-product, parallel schedule, 60 iterations per attempt and 250 on the last, which matches the serial schedule's disclosure at a third of the time); blind reconciliation: the code's rows are chosen from the QBER, punctured positions start unknown to Bob and are revealed 1/40 at a time while a frame fails, and a 32-bit tag per frame confirms each decoded frame | `ec/ldpc5g.py` |

### Efficiency

| Method | 1% | 2% | 3% | 5% | 7% | 9% | 11% |
|---|---|---|---|---|---|---|---|
| **Cascade, optimized, $n = 2^{14}$ to $2^{17}$** | **1.025** | **1.033** | **1.045** | **1.055** | **1.058** | **1.064** | **1.077** |
| Winnow, $n = 2^{17}$ | 2.059 | 1.505 | 1.387 | 1.484 | 1.427 | 1.624 | 1.537 |
| Polar SC $N = 2^{14}$ | 2.235 | 1.805 | 1.616 | 1.446 | 1.342 | 1.267 | 1.240 |
| Polar SC $N = 2^{16}$ | 2.348 | 1.815 | 1.567 | 1.398 | 1.315 | 1.305 | 1.282 |
| Polar SCL8 $N = 2^{16}$ | 2.015 | 1.616 | 1.448 | 1.277 | 1.221 | 1.232 | 1.218 |
| Polar SCL32 $N = 2^{16}$ | 1.920 | 1.598 | 1.422 | 1.269 | 1.207 | 1.214 | 1.197 |
| LDPC regular, $n \approx 2^{14}$ | 1.257, 98% of blocks fail | 1.266, 98% fail | 1.306, 89% fail | 1.309, 31% fail | 1.189, 98% fail | 1.396 | 1.219, 38% fail |
| 5G NR LDPC, sum-product | 1.599 | 1.281 | 1.225 | 1.164 | 1.134 | 1.118 | 1.114 |
| 5G NR LDPC, min-sum (measured with a looser plan) | 1.657 | 1.330, 3% fail | 1.264 | 1.242 | 1.224 | 1.216 | 1.224 |

No method except regular LDPC and polar at $N = 2^{20}$ left any block uncorrected. Polar SCL8 at $N = 2^{20}$ failed whole blocks at 1%, 7%, 9% and 11% QBER; its Gaussian-approximation construction degrades that far, so the default block length is now capped at $2^{16}$ (`PolarMaxDefaultBlock`).

### Secret fraction per sifted bit

| Method | 1% | 2% | 3% | 5% | 7% | 9% | 11% |
|---|---|---|---|---|---|---|---|
| Polar SCL8 $N = 2^{16}$ | 0.756 | 0.630 | 0.524 | 0.348 | 0.187 | 0.026 | 0 |
| Polar SCL32 $N = 2^{16}$ | 0.764 | 0.633 | 0.529 | 0.350 | 0.193 | 0.034 | 0 |
| LDPC regular | 0 | 0 | 0 | 0.116 | 0 | 0 | 0 |
| 5G NR LDPC, sum-product | 0.790 | 0.677 | 0.568 | 0.380 | 0.219 | 0.075 | 0 |
| **Cascade, optimized** | **0.836** | **0.712** | **0.603** | **0.411** | **0.247** | **0.099** | 0 |
| Winnow | 0.753 | 0.646 | 0.536 | 0.289 | 0.112 | 0 | 0 |

Cascade leaves the most key at every QBER: 8% more than 5G NR LDPC at 5% QBER, 13% more at 7% and 32% more at 9%, and its disclosure is within 2.5% to 8% of the Shannon limit. It is interactive, a few thousand rounds of parity questions per block, which costs nothing when both strings are on one machine, as in single-site extraction and continuous QKD; between two nodes the one-way 5G NR LDPC stays the method. At 7% QBER 5G NR LDPC leaves 13% more key than the best polar setting, and at 9% more than twice as much. At 11% the asymptotic fraction is zero for every method, since $1 - 2h_2(0.11) < 0$ even before any disclosure.

### Throughput, Mbit/s on eight cores

| Method | 1% | 2% | 3% | 5% | 7% | 9% | 11% |
|---|---|---|---|---|---|---|---|
| Cascade, optimized, $n = 2^{17}$ | 15.4 | 21.9 | 22.5 | 22.1 | 18.2 | 19.0 | 15.1 |
| Winnow, $n = 2^{17}$ | 12.2 | 17.3 | 15.8 | 15.2 | 13.0 | 13.3 | 16.0 |
| Polar SC $N = 2^{14}$ | 1.98 | 2.03 | 2.02 | 1.92 | 2.03 | 2.12 | 2.06 |
| Polar SCL8 $N = 2^{16}$ | 0.92 | 0.86 | 0.83 | 0.88 | 0.92 | 0.74 | 0.73 |
| Polar SCL32 $N = 2^{16}$ | 0.20 | 0.18 | 0.18 | 0.21 | 0.22 | 0.16 | 0.16 |
| 5G NR LDPC, sum-product | 0.016 | 0.028 | 0.026 | 0.028 | 0.031 | 0.032 | 0.035 |
| 5G NR LDPC, min-sum | 0.059 | 0.086 | 0.073 | 0.065 | 0.059 | 0.057 | 0.054 |

5G NR LDPC is about thirty times slower than polar SCL8, because the library decodes every attempt from scratch and a frame typically needs ten attempts. Thirty kilobits per second is still about a thousand times what the example recording produces (424 sifted bits in ten seconds), so speed is not the constraint.

### Findings

- **Most efficient**: 5G NR LDPC with exact sum-product at every QBER measured, $f = 1.11$ to $1.28$ from 2% to 11% and 1.60 at 1%, with no failed frame. Min-sum is faster but costs 0.05 to 0.11 in $f$; it is not worth it on this link.
- **Polar SCL32** is the best polar setting, 0.08 to 0.32 behind 5G NR LDPC; SCL8 is within 0.02 of it at a quarter of the time.
- **Regular LDPC at a fixed rate** fails most blocks unless its rate is far from the limit; rate adaptation is what makes LDPC work in QKD.
- **Low QBER is hard for every method**: at 1% the 5G frame is at most 9,984 key bits at rate 0.92, and such short high-rate codes are known to sit far from capacity. Longer, QKD-specific codes (frames of $10^5$ to $10^6$ bits with optimized irregular degree distributions) reach $f \approx 1.05$ in the literature, but no maintained library provides them.

### Status in the pipeline

5G NR LDPC is the default error correction of the dashboard, single-site and two-party. The protocol and its accounting are in Go (`qkd/ldpc5g.go`, and the `ldpc5g` messages of `qkd/twoparty.go`); the matrix work runs in `ec/ldpc5g.py serve`, a pool of Python processes the Go side drives through `pyec`. The frame layout is computed in Go, with Go's seeded generator, so both sites derive the same one whatever numpy they run, and it fits the lifting size to the data: as few frames as the largest lifting allows, each with the smallest lifting that holds its share, so a short run is not charged for an empty frame. `scripts/ec_compare.py` measures the library module on its own and is what the tables above come from.
