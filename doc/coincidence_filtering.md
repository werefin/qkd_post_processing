## Coincidence filtering

**Status**: formal procedure, implemented in `coincfilter.go` and `detdelay.go` and run by default in `cmd/importtt` (`-filter`, on unless `-filter=false`). It runs after clock alignment and before sifting, and applies to every reconciliation strategy.

### The problem it solves

A sifted pair is either a **true pair** (both photons from one entangled state, arrival-time offset $\Delta t = t_\text{Bob} - t_\text{Alice}$ in a narrow peak) or an **accidental coincidence** (two unrelated photons, $\Delta t$ uniform over the window, bits uncorrelated, 50% error). No measurement can tell the two apart for an individual pair. What can be controlled is how many accidentals the window admits, which is proportional to its width.

Within a short time slice the peak is as sharp as the detectors' jitter allows, tens of picoseconds, but over a recording of several seconds its position can move by nanoseconds, because a clock-skew fit that is slightly off leaves a residual linear drift. A window wide enough to hold the smeared peak admits accidentals in proportion to its width, and each one adds a 50% error, so the QBER climbs toward one half. Tracking the peak in slices and removing the drift brings the window back to the peak's own width.

### Procedure

All inputs are public: each click's timestamp, basis and multi-photon flag, which sifting already exchanges (`PublicEvent`), plus declared constants. Bit values are never read (except in the separate calibration of Per-detector delays and the timing side channel).

- **Pruning** (`PruneToCoincidenceRange`): Bob records about 50 times as many clicks as Alice, nearly all unpaired. Bob clicks farther than $\max(20\,\text{ns}, 2 \times \text{window})$ from every Alice click can never form a coincidence and are dropped. Results are bit-identical with and without pruning; memory falls from 4.9 GB to 1.6 GB per recording.
- **Candidate pairs**: after the lag-and-skew alignment, list $\Delta t$ for every basis-matched, non-multi-photon Alice-Bob pair with $|\Delta t| \le R = 3$ ns.
- **Drift tracking** (`EstimateDrift`): split the recording into slices of $T = 250$ ms. In each slice, histogram $\Delta t$ in 25 ps bins, estimate the flat background from $|\Delta t| > 2R/3$, locate the maximum of a 5-bin running sum, and take the background-subtracted centroid near it as the slice's peak centre $c_s$. A slice is trusted if its peak-to-background ratio is at least 5.
- **Drift removal, iterated** (`ApplyDrift`, `FilterCoincidences`): let $c(t)$ interpolate the trusted $(t_s, c_s)$ linearly and **extrapolate the end segments linearly**, since residual skew keeps drifting past the last trusted slice. Replace every Bob timestamp $t$ by $t - c(t)$. Repeat steps 3-4 on the corrected data until every slice offset is below two bins (at most 5 passes): a correction brings slices whose peak lay beyond $\pm R$ into range.
- **Window: half width at half maximum of a Gaussian fit** (`fitGaussian`) --> on the drift-corrected histogram (10 ps bins over $\pm R$), fit:
   $$f(\Delta t) = A\, e^{-(\Delta t - \mu)^2/(2\sigma^2)} + B$$
by Poisson-weighted Levenberg-Marquardt: the Gaussian is the true-pair peak, $B$ the flat accidental floor. The window half-width is:
   $$w = \mathrm{HWHM} = \sigma\sqrt{2\ln 2} \approx 1.177\,\sigma,$$
and the peak is moved to $\mu = 0$. The predicted accidental share inside $\pm w$ is $a = 2wB / (2wB + A\sigma\sqrt{2\pi}\,\mathrm{erf}(\sqrt{\ln 2}))$, and $q_\text{pred} = e_0 + (\tfrac12 - e_0)\,a$ with a declared prior $e_0$ (`-filter-e0`, default 0.03), used only to size the QBER sample.
- **Sift** with half-width $w$; QBER, phase error and privacy amplification are computed on exactly the pairs inside it.
- **Pooling** (`importtt -dir a,b,c`). Several recordings are each pruned and drift-corrected on their own (each has its own skew), laid end to end with a 1 s gap, and treated as one privacy-amplification block; the window is refitted on the union. Every recording given is included: choosing recordings by their measured QBER would be post-selection.

### Security

**Claim**: we introduce no bias into the error statistics of the key and reveal nothing beyond what sifting already reveals.

*Argument*: (a) Every quantity in above steps is a function of public timestamps, bases and multi-photon flags, and of declared constants. `TestFilterCoincidencesNeverReadsBits` checks this directly: flipping every one of Bob's bits leaves the drift model and the window unchanged. (b) The window is fixed before any bit is examined, so it cannot select for low-error pairs; every pair inside it is kept, and the sample, the QBER estimate and privacy amplification see the same population as the key. (c) This is what distinguishes the procedure from choosing time buckets by their measured QBER, which `windowreport.go` only reports and never uses. (d) **Timing side channel**: if detectors have different delays, $\Delta t$ depends on which detector clicked, that is, on the bit. Per-detector delays and the timing side channel measures this on the real recordings and gives the two mitigations.

### Per-detector delays and the timing side channel

After drift removal, the four correct-bit detector classes sit at different and stable offsets: across four recordings, A00-B00 at $+12$ to $+16$ ps, A01-B01 at $-10$ to $-13$ ps, A10-B10 at $+15$ to $+21$ ps, A11-B11 at $-5$ to $+3$ ps, against peaks of $\sigma \approx 32$ ps. Two consequences: announced $\Delta t$ carries information about the bit, and a window of $\pm 40$ ps accepts off-centre classes about 6% less often than centred ones.

**Calibration** (`CalibrateDetectorDelays`, `importtt -calibrate-delays`): each class gives one equation $c_{xy} = d^\mathrm{B}_y - d^\mathrm{A}_x$; the four bit classes of a basis are solved for the four delays by weighted least squares (weights $n/\mathrm{rms}^2$, gauge $\sum d = 0$). Each party then subtracts its own detector's delay from its own clicks (`-detector-delays`), before timestamps are used. Calibration reads detector identities, which are bit values, so it must run on a dedicated calibration recording that is never used for key. On run_trigger02 it lowers the estimated timing leak $I(\text{bit};\Delta t)$ (`TimingLeakBits`) from 0.072 to 0.012 bits per sifted bit. The residual comes from the error classes, which an additive per-detector model cannot centre ($c_{00} + c_{11} \ne c_{01} + c_{10}$ by about 60 ps), pointing to a different physical origin such as cross-talk or afterpulsing.

**Protocol rule**: calibration reduces but does not remove the leak, and a mutual-information estimate is not a rigorous min-entropy charge. Deployments must therefore use **one-way timestamp announcement**: Alice announces her timestamps, Bob finds coincidences, tracks drift and fits the window locally, and announces only the matched indices with their bases, the drift model and the window. $\Delta t$ is then never public. All finite-key results below are certified only under this rule.

### Measured behaviour

**Each recording on its own** (`cmd/importtt -polar`, defaults; QBER from detector visibility over all sifted pairs):

| Recording | Unfiltered window | Unfiltered QBER | Drift found, passes | Fit $\sigma$ | Window (HWHM) | Sifted pairs | Filtered QBER |
|---|---|---|---|---|---|---|---|
| variance 084244 | ±2 ns | 27.8% | 1.8 ns, 2 | 32.0 ps | ±38 ps | 3,350 | **5.2%** |
| variance 084056 | ±5 ns | 37.3% | 4.8 ns, 2 | 32.9 ps | ±39 ps | 3,239 | **5.3%** |
| run_delayfix | ±10 ns | 43.0% | 5.8 ns, 3 | 34.3 ps | ±40 ps | 3,493 | **6.0%** |
| variance 084151 | ±10 ns | 42.3% | 5.7 ns, 3 | 34.4 ps | ±41 ps | 3,351 | **6.2%** |
| run_trigger02 | ±1 ns | 20.5% | 1.4 ns, 2 | 34.3 ps | ±40 ps | 3,280 | **6.4%** |
| variance 084119 | ±2 ns | 27.8% | 2.0 ns, 2 | 32.9 ps | ±39 ps | 3,263 | **6.6%** |
| run_trigger03 | ±10 ns | 43.0% | 5.8 ns, 3 | 35.2 ps | ±41 ps | 3,296 | **9.4%** |

All seven recordings move from far above to below the roughly 11% QBER beyond which no secret key is possible, with predicted accidentals of 2.0-2.4%.

**Simulated link** (`coincfilter_test.go`: 20,000 true pairs, 40 ps jitter, 2% intrinsic error, 1.6 ns linear drift, singles rates as on the real link): drift tracked to within 1.1 ps; QBER 18.1% at $\pm 1$ ns unfiltered, 3.5% after filtering. The Gaussian fit recovers a known $\sigma = 40$ ps peak as 39.8 ps, HWHM 46.9 ps against 47.1 ps (`TestFitGaussianRecoversPeak`).

### Limitations

- Drift faster than the 250 ms slices would leave residual smearing.
- The root cause is the skew fit in `acquisition/coincidence_peak.py`; this procedure corrects its effect downstream. A shared clock between the sites would remove it at the source.
- One ten-second recording yields about 3,300 filtered pairs, too few for a finite-key claim on its own; see Block size in `doc/finite_key_procedure.md` for pooled and longer acquisitions.
