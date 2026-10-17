package qkd

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"sort"
)

// Coincidence filtering from public timing only: track the coincidence peak's drift over
// the recording, undo it, then size the window to the sharpened peak; bit values are never read

// DriftConfig controls peak tracking; zero values pick the defaults below
type DriftConfig struct {
	SliceDurationPS int64   // time slice per peak estimate, default 250 ms
	SearchRadiusPS  int64   // +-dt range searched for the peak, default 3 ns
	BinPS           int64   // histogram bin, default 25 ps
	MinScore        float64 // slice peak/background ratio to trust a slice, default 5
}

const (
	defaultDriftSlicePS  = 250_000_000_000
	defaultDriftRadiusPS = 3000
	defaultDriftBinPS    = 25
	defaultDriftMinScore = 5
)

func (c DriftConfig) withDefaults() DriftConfig {
	if c.SliceDurationPS <= 0 {
		c.SliceDurationPS = defaultDriftSlicePS
	}
	if c.SearchRadiusPS <= 0 {
		c.SearchRadiusPS = defaultDriftRadiusPS
	}
	if c.BinPS <= 0 {
		c.BinPS = defaultDriftBinPS
	}
	if c.MinScore <= 0 {
		c.MinScore = defaultDriftMinScore
	}
	return c
}

// DriftModel is the peak offset over time, piecewise linear between trusted slice centres
type DriftModel struct {
	TimesPS   []int64   // slice midpoints on Alice's clock, ascending
	OffsetsPS []float64 // peak centre (t_bob - t_alice) at each midpoint
	Slices    int       // slices examined
}

// Offset interpolates the peak centre at time t, extrapolating the end segments linearly,
// since residual clock skew keeps drifting past the last trusted slice
func (m DriftModel) Offset(t int64) float64 {
	n := len(m.TimesPS)
	line := func(i, j int) float64 {
		slope := (m.OffsetsPS[j] - m.OffsetsPS[i]) / float64(m.TimesPS[j]-m.TimesPS[i])
		return m.OffsetsPS[i] + slope*float64(t-m.TimesPS[i])
	}
	switch {
	case n == 0:
		return 0
	case n == 1:
		return m.OffsetsPS[0]
	case t <= m.TimesPS[0]:
		return line(0, 1)
	case t >= m.TimesPS[n-1]:
		return line(n-2, n-1)
	}
	i := sort.Search(n, func(i int) bool { return m.TimesPS[i] > t })
	t0, t1 := m.TimesPS[i-1], m.TimesPS[i]
	f := float64(t-t0) / float64(t1-t0)
	return m.OffsetsPS[i-1] + f*(m.OffsetsPS[i]-m.OffsetsPS[i-1])
}

// basisPairDts lists dt=t_bob-t_alice for every basis-matched, non-multi-photon pair within
// +-radius, with each pair's Alice time; public metadata only
func basisPairDts(alice, bob []PublicEvent, radius int64, shift func(int64) float64) (ta, dt []int64) {
	ai, bi := sortedIndices(alice), sortedIndices(bob)
	lo := 0
	for _, a := range ai {
		ea := alice[a]
		if ea.MultiPhoton {
			continue
		}
		s := int64(0)
		if shift != nil {
			s = int64(math.Round(shift(ea.TimestampPS)))
		}
		for lo < len(bi) && bob[bi[lo]].TimestampPS-s < ea.TimestampPS-radius {
			lo++
		}
		for j := lo; j < len(bi); j++ {
			eb := bob[bi[j]]
			d := eb.TimestampPS - s - ea.TimestampPS
			if d > radius {
				break
			}
			if eb.MultiPhoton || eb.Basis != ea.Basis {
				continue
			}
			ta = append(ta, ea.TimestampPS)
			dt = append(dt, d)
		}
	}
	return ta, dt
}

// PeakProfile summarizes a coincidence histogram: centre, rms width and background, all
// from timing alone, with the signal count left after subtracting the flat background
type PeakProfile struct {
	CentrePS        float64
	SigmaPS         float64
	BackgroundPerPS float64 // accidental coincidences per ps of window
	Signal          float64 // background-subtracted counts in the peak
	Score           float64 // smoothed peak bin over mean background bin
}

// measurePeak fits a PeakProfile to dt values in [-radius, radius]: background from the
// outer third, centre and width from the background-subtracted region around the maximum
func measurePeak(dt []int64, radius, bin int64) PeakProfile {
	nb := int(2 * radius / bin)
	if nb < 9 || len(dt) == 0 {
		return PeakProfile{}
	}
	h := make([]float64, nb)
	for _, d := range dt {
		if k := int((d + radius) / bin); k >= 0 && k < nb {
			h[k]++
		}
	}
	x := func(k int) float64 { return float64(int64(k)*bin-radius) + float64(bin)/2 }

	var bgSum float64
	bgN := 0
	for k := range nb {
		if math.Abs(x(k)) > 2*float64(radius)/3 {
			bgSum += h[k]
			bgN++
		}
	}
	bg := bgSum / float64(max(bgN, 1))

	best, bk := -1.0, 0
	for k := 2; k < nb-2; k++ {
		if v := h[k-2] + h[k-1] + h[k] + h[k+1] + h[k+2]; v > best {
			best, bk = v, k
		}
	}
	p := PeakProfile{BackgroundPerPS: bg / float64(bin), Score: best / 5 / math.Max(bg, 0.5)}

	// centroid and rms inside +-radius/3 of the maximum, after background subtraction
	var w, m float64
	half := float64(radius) / 3
	for k := range nb {
		if e := h[k] - bg; math.Abs(x(k)-x(bk)) <= half && e > 0 {
			w += e
			m += e * x(k)
		}
	}
	if w <= 0 {
		return p
	}
	p.CentrePS = m / w
	var v float64
	for k := range nb {
		if e := h[k] - bg; math.Abs(x(k)-p.CentrePS) <= half && e > 0 {
			v += e * (x(k) - p.CentrePS) * (x(k) - p.CentrePS)
		}
	}
	p.SigmaPS = math.Sqrt(v / w)
	p.Signal = w
	return p
}

// PruneToCoincidenceRange keeps only bob clicks within +-radiusPS of some alice click: the rest
// can never coincide, so dropping them changes no result and saves most of the memory
func PruneToCoincidenceRange(alice, bob []DetectionEvent, radiusPS int64) []DetectionEvent {
	ts := make([]int64, len(alice))
	for i, e := range alice {
		ts[i] = e.TimestampPS
	}
	if !slices.IsSorted(ts) {
		slices.Sort(ts)
	}
	inOrder := slices.IsSortedFunc(bob, func(a, b DetectionEvent) int { return cmp.Compare(a.TimestampPS, b.TimestampPS) })
	out := make([]DetectionEvent, 0, min(len(bob), len(alice)+len(bob)/64))
	j := 0
	for _, e := range bob {
		var i int
		if inOrder { // one pass over both, the usual case: recordings come time-ordered
			for j < len(ts) && ts[j] < e.TimestampPS-radiusPS {
				j++
			}
			i = j
		} else {
			i = sort.Search(len(ts), func(i int) bool { return ts[i] >= e.TimestampPS-radiusPS })
		}
		if i < len(ts) && ts[i] <= e.TimestampPS+radiusPS {
			out = append(out, e)
		}
	}
	return slices.Clip(out)
}

// EstimateDrift tracks the coincidence peak slice by slice; slices whose peak is too weak
// to trust are skipped, and the model interpolates linearly across them
func EstimateDrift(alice, bob []PublicEvent, cfg DriftConfig) (DriftModel, error) {
	cfg = cfg.withDefaults()
	ta, dt := basisPairDts(alice, bob, cfg.SearchRadiusPS, nil)
	if len(ta) == 0 {
		return DriftModel{}, fmt.Errorf("drift: no basis-matched coincidences within +-%d ps", cfg.SearchRadiusPS)
	}
	start := ta[0]
	for _, t := range ta {
		start = min(start, t)
	}
	bySlice := map[int64][]int64{}
	for i, t := range ta {
		s := (t - start) / cfg.SliceDurationPS
		bySlice[s] = append(bySlice[s], dt[i])
	}
	keys := make([]int64, 0, len(bySlice))
	for s := range bySlice {
		keys = append(keys, s)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	m := DriftModel{Slices: len(keys)}
	for _, s := range keys {
		p := measurePeak(bySlice[s], cfg.SearchRadiusPS, cfg.BinPS)
		if p.Score < cfg.MinScore || p.Signal <= 0 {
			continue
		}
		m.TimesPS = append(m.TimesPS, start+s*cfg.SliceDurationPS+cfg.SliceDurationPS/2)
		m.OffsetsPS = append(m.OffsetsPS, p.CentrePS)
	}
	if len(m.TimesPS) == 0 {
		return m, fmt.Errorf("drift: no slice had a coincidence peak scoring >= %.1f", cfg.MinScore)
	}
	return m, nil
}

// ApplyDrift returns a copy of bob with each timestamp moved by -Offset(t), centring the
// peak at dt=0 for the whole recording
func ApplyDrift(bob []DetectionEvent, m DriftModel) []DetectionEvent {
	out := make([]DetectionEvent, len(bob))
	for i, e := range bob {
		e.TimestampPS -= int64(math.Round(m.Offset(e.TimestampPS)))
		out[i] = e
	}
	return out
}

// driftMaxPasses bounds the tracking iterations; real recordings converge in two or three
const driftMaxPasses = 5

// CoincidenceFilter is the outcome of FilterCoincidences: the drift model applied, the
// Gaussian fitted to the sharpened peak, and the window it sets
type CoincidenceFilter struct {
	Drift              DriftModel // first pass: the drift as found in the raw data
	DriftPasses        int        // tracking passes run until the residual drift was below one bin
	DriftTrusted       int        // slices trusted in the last pass
	Before, After      PeakProfile
	Fit                GaussianFit
	WindowPS           int64   // half-width: the fitted peak's HWHM, rounded to the picosecond
	AccidentalFraction float64 // predicted share of sifted pairs that are accidentals
	PredictedQBER      float64 // e0 + (0.5-e0)*AccidentalFraction, from timing and the declared e0
}

// WindowConfig holds the declared intrinsic-error prior e0 used only to predict QBER
type WindowConfig struct {
	IntrinsicQBER float64 // e0, default 0.03
}

// GaussianFit is A*exp(-(dt-mu)^2/(2 sigma^2)) + B fitted to the coincidence histogram:
// the peak of true pairs on the flat floor of accidentals, B and A in counts per bin
type GaussianFit struct {
	CentrePS, SigmaPS, Amplitude, Background float64
	BinPS                                    int64
}

// HWHM is the fitted peak's half width at half maximum, sigma*sqrt(2 ln 2)
func (g GaussianFit) HWHM() float64 { return g.SigmaPS * math.Sqrt(2*math.Ln2) }

// fitBinPS is the histogram bin for the Gaussian fit, fine enough for peaks ~100 ps wide
const fitBinPS = 10

// fitGaussian fits the peak by Poisson-weighted Levenberg-Marquardt over +-radius, started
// from the background-subtracted profile; dt values are public timing only
func fitGaussian(dt []int64, radius int64, p PeakProfile) (GaussianFit, error) {
	nb := int(2 * radius / fitBinPS)
	y := make([]float64, nb)
	for _, d := range dt {
		if k := int((d + radius) / fitBinPS); k >= 0 && k < nb {
			y[k]++
		}
	}
	x := func(k int) float64 { return float64(int64(k)*fitBinPS-radius) + fitBinPS/2.0 }

	b0 := p.BackgroundPerPS * fitBinPS
	peak := 0.0
	for k := range nb {
		if math.Abs(x(k)-p.CentrePS) < 200 {
			peak = math.Max(peak, y[k])
		}
	}
	// start sigma from the half-maximum crossing nearest the centre
	sigma0 := 50.0
	for d := float64(fitBinPS); d < 1000; d += fitBinPS {
		k := int((p.CentrePS + d + float64(radius)) / fitBinPS)
		if k < nb && y[k]-b0 < (peak-b0)/2 {
			sigma0 = math.Max(d/math.Sqrt(2*math.Ln2), fitBinPS)
			break
		}
	}
	par := [4]float64{math.Max(peak-b0, 1), p.CentrePS, sigma0, b0} // A, mu, sigma, B

	model := func(q [4]float64, xv float64) (f float64, grad [4]float64) {
		u := (xv - q[1]) / q[2]
		g := math.Exp(-u * u / 2)
		return q[0]*g + q[3], [4]float64{g, q[0] * g * u / q[2], q[0] * g * u * u / q[2], 1}
	}
	cost := func(q [4]float64) float64 {
		var c float64
		for k := range nb {
			f, _ := model(q, x(k))
			r := y[k] - f
			c += r * r / math.Max(y[k], 1)
		}
		return c
	}
	lambda, cur := 1e-3, cost(par)
	for range 200 {
		var jtj [4][4]float64
		var jtr [4]float64
		for k := range nb {
			f, g := model(par, x(k))
			w := 1 / math.Max(y[k], 1)
			r := y[k] - f
			for i := range 4 {
				jtr[i] += w * g[i] * r
				for j := range 4 {
					jtj[i][j] += w * g[i] * g[j]
				}
			}
		}
		for i := range 4 {
			jtj[i][i] *= 1 + lambda
		}
		step, ok := solve4(jtj, jtr)
		if !ok {
			break
		}
		var trial [4]float64
		for i := range 4 {
			trial[i] = par[i] + step[i]
		}
		if trial[2] <= 0 || trial[0] <= 0 {
			lambda *= 10
			continue
		}
		if c := cost(trial); c < cur {
			done := (cur-c)/cur < 1e-10
			par, cur, lambda = trial, c, lambda/10
			if done {
				break
			}
		} else {
			lambda *= 10
		}
	}
	if !(par[2] > 0 && par[0] > 0) {
		return GaussianFit{}, fmt.Errorf("coincidence filter: Gaussian fit did not converge to a peak")
	}
	return GaussianFit{CentrePS: par[1], SigmaPS: par[2], Amplitude: par[0], Background: par[3], BinPS: fitBinPS}, nil
}

// solve4 solves a 4x4 linear system by Gaussian elimination with partial pivoting
func solve4(a [4][4]float64, b [4]float64) ([4]float64, bool) {
	for c := range 4 {
		piv := c
		for r := c + 1; r < 4; r++ {
			if math.Abs(a[r][c]) > math.Abs(a[piv][c]) {
				piv = r
			}
		}
		if math.Abs(a[piv][c]) < 1e-300 {
			return b, false
		}
		a[c], a[piv] = a[piv], a[c]
		b[c], b[piv] = b[piv], b[c]
		for r := c + 1; r < 4; r++ {
			f := a[r][c] / a[c][c]
			for k := c; k < 4; k++ {
				a[r][k] -= f * a[c][k]
			}
			b[r] -= f * b[c]
		}
	}
	var out [4]float64
	for r := 3; r >= 0; r-- {
		s := b[r]
		for k := r + 1; k < 4; k++ {
			s -= a[r][k] * out[k]
		}
		out[r] = s / a[r][r]
	}
	return out, true
}

// FilterCoincidences tracks and removes drift, fits a Gaussian to the sharpened peak and sets
// the window half-width to its HWHM; every input is public timing, never a bit value
func FilterCoincidences(alice, bob []DetectionEvent, cfg DriftConfig, wc WindowConfig) ([]DetectionEvent, CoincidenceFilter, error) {
	cfg = cfg.withDefaults()
	pa, pb := StripBit(alice), StripBit(bob)
	var f CoincidenceFilter
	_, dtBefore := basisPairDts(pa, pb, cfg.SearchRadiusPS, nil)
	f.Before = measurePeak(dtBefore, cfg.SearchRadiusPS, cfg.BinPS)

	// iterate: a correction brings slices that were out of the search radius into it
	corrected := bob
	for pass := range driftMaxPasses {
		drift, err := EstimateDrift(pa, StripBit(corrected), cfg)
		if err != nil {
			if pass == 0 {
				return nil, f, err
			}
			break
		}
		if pass == 0 {
			f.Drift = drift
		}
		f.DriftPasses, f.DriftTrusted = pass+1, len(drift.TimesPS)
		corrected = ApplyDrift(corrected, drift)
		residual := 0.0
		for _, o := range drift.OffsetsPS {
			residual = math.Max(residual, math.Abs(o))
		}
		if residual < float64(2*cfg.BinPS) {
			break
		}
	}

	_, dtAfter := basisPairDts(pa, StripBit(corrected), cfg.SearchRadiusPS, nil)
	f.After = measurePeak(dtAfter, cfg.SearchRadiusPS, cfg.BinPS)
	if f.After.SigmaPS <= 0 {
		return nil, f, fmt.Errorf("coincidence filter: no peak after drift correction")
	}
	fit, err := fitGaussian(dtAfter, cfg.SearchRadiusPS, f.After)
	if err != nil {
		return nil, f, err
	}
	f.Fit = fit
	f.WindowPS = max(1, int64(math.Round(f.Fit.HWHM())))

	// signal inside +-HWHM is erf(sqrt(ln 2)) ~ 76% of the Gaussian's area; accidentals are flat
	e0 := wc.IntrinsicQBER
	if e0 <= 0 {
		e0 = 0.03
	}
	area := f.Fit.Amplitude * f.Fit.SigmaPS * math.Sqrt(2*math.Pi) / fitBinPS
	signal := area * math.Erf(float64(f.WindowPS)/(f.Fit.SigmaPS*math.Sqrt2))
	accidentals := f.Fit.Background / fitBinPS * float64(2*f.WindowPS)
	if signal+accidentals > 0 {
		f.AccidentalFraction = accidentals / (signal + accidentals)
	}
	f.PredictedQBER = e0 + (0.5-e0)*f.AccidentalFraction

	// centre the fitted peak exactly at dt=0
	if c := int64(math.Round(f.Fit.CentrePS)); c != 0 {
		for i := range corrected {
			corrected[i].TimestampPS -= c
		}
	}
	return corrected, f, nil
}
