package qkd_post_processing

import (
	"math"
	"math/rand"
	"testing"
)

// driftingLink models what coincfilter fixes: true pairs with 40 ps jitter whose offset drifts
// linearly by driftPS, plus unpaired singles on both sides that fall into the window by chance
func driftingLink(pairs int, driftPS, aliceSinglesPerPair, bobSinglesPerNs float64, seed int64) (alice, bob []DetectionEvent) {
	r := rand.New(rand.NewSource(seed))
	const spacing = 2_000_000 // 2 us between true pairs
	durationPS := float64(pairs) * spacing
	for i := range pairs {
		t := int64(i) * spacing
		basis := Basis(r.Intn(2))
		bit := byte(r.Intn(2))
		bobBit := bit
		if r.Float64() < 0.02 {
			bobBit ^= 1
		}
		off := driftPS*float64(t)/durationPS - driftPS/2 + r.NormFloat64()*40
		alice = append(alice, DetectionEvent{TimestampPS: t, Basis: basis, Bit: bit})
		bob = append(bob, DetectionEvent{TimestampPS: t + int64(math.Round(off)), Basis: basis, Bit: bobBit})
	}
	single := func(n int) []DetectionEvent {
		out := make([]DetectionEvent, n)
		for i := range out {
			out[i] = DetectionEvent{TimestampPS: int64(r.Float64() * durationPS), Basis: Basis(r.Intn(2)), Bit: byte(r.Intn(2))}
		}
		return out
	}
	alice = append(alice, single(int(float64(pairs)*aliceSinglesPerPair))...)
	bob = append(bob, single(int(bobSinglesPerNs*durationPS/1000))...)
	return alice, bob
}

func siftedQBER(alice, bob []DetectionEvent, window int64) (int, float64) {
	s := Sift(alice, bob, window)
	e := 0
	for _, x := range s {
		if x.AliceBit != x.BobBit {
			e++
		}
	}
	return len(s), float64(e) / float64(max(1, len(s)))
}

// TestEstimateDriftRecoversLinearDrift: the tracked offset must follow the injected drift
// to within a fraction of the jitter at every trusted slice
func TestEstimateDriftRecoversLinearDrift(t *testing.T) {
	alice, bob := driftingLink(500000, 1600, 0, 0, 1)
	m, err := EstimateDrift(StripBit(alice), StripBit(bob), DriftConfig{SliceDurationPS: 50_000_000_000})
	if err != nil {
		t.Fatal(err)
	}
	duration := float64(500000) * 2_000_000
	worst := 0.0
	for i, tm := range m.TimesPS {
		want := 1600*float64(tm)/duration - 800
		worst = math.Max(worst, math.Abs(m.OffsetsPS[i]-want))
	}
	t.Logf("%d/%d slices trusted, worst tracking error %.1f ps", len(m.TimesPS), m.Slices, worst)
	if worst > 25 {
		t.Fatalf("drift tracking error %.1f ps exceeds 25 ps", worst)
	}
}

// TestFilterCoincidencesRestoresLowQBER: drift smears the peak so a wide window is needed
// and admits accidentals; after filtering, QBER must fall close to the 2% intrinsic rate
func TestFilterCoincidencesRestoresLowQBER(t *testing.T) {
	alice, bob := driftingLink(20000, 1600, 140, 0.0035, 2) // singles rates as in the real recordings
	nBefore, qBefore := siftedQBER(alice, bob, 1000)
	corrected, f, err := FilterCoincidences(alice, bob, DriftConfig{SliceDurationPS: 4_000_000_000}, WindowConfig{})
	if err != nil {
		t.Fatal(err)
	}
	nAfter, qAfter := siftedQBER(alice, corrected, f.WindowPS)
	t.Logf("before: +-1000 ps, sifted=%d QBER=%.4f | after: +-%d ps, sifted=%d QBER=%.4f, predicted accidentals %.3f",
		nBefore, qBefore, f.WindowPS, nAfter, qAfter, f.AccidentalFraction)
	if qAfter > 0.05 || qAfter >= qBefore {
		t.Fatalf("expected QBER near 2%% after filtering and below the unfiltered %.4f, got %.4f", qBefore, qAfter)
	}
}

// TestFilterCoincidencesNeverReadsBits is the security property: flipping every one of
// Bob's bits must leave the drift model and the chosen window exactly unchanged
func TestFilterCoincidencesNeverReadsBits(t *testing.T) {
	alice, bob := driftingLink(20000, 1200, 20, 0.0035, 3)
	flipped := make([]DetectionEvent, len(bob))
	for i, e := range bob {
		e.Bit ^= 1
		flipped[i] = e
	}
	cfg := DriftConfig{SliceDurationPS: 4_000_000_000}
	_, f1, err := FilterCoincidences(alice, bob, cfg, WindowConfig{})
	if err != nil {
		t.Fatal(err)
	}
	_, f2, err := FilterCoincidences(alice, flipped, cfg, WindowConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if f1.WindowPS != f2.WindowPS || len(f1.Drift.OffsetsPS) != len(f2.Drift.OffsetsPS) {
		t.Fatalf("filter changed with bit values: window %d vs %d", f1.WindowPS, f2.WindowPS)
	}
	for i := range f1.Drift.OffsetsPS {
		if f1.Drift.OffsetsPS[i] != f2.Drift.OffsetsPS[i] {
			t.Fatalf("drift slice %d changed with bit values: %v vs %v", i, f1.Drift.OffsetsPS[i], f2.Drift.OffsetsPS[i])
		}
	}
}

// TestFilterCoincidencesErrorsWithoutPeak: pure accidentals have no peak to track
func TestFilterCoincidencesErrorsWithoutPeak(t *testing.T) {
	r := rand.New(rand.NewSource(4))
	var alice, bob []DetectionEvent
	for i := range 50000 {
		alice = append(alice, DetectionEvent{TimestampPS: int64(i) * 2_000_000, Basis: Basis(r.Intn(2))})
		bob = append(bob, DetectionEvent{TimestampPS: r.Int63n(int64(50000) * 2_000_000), Basis: Basis(r.Intn(2))})
	}
	if _, _, err := FilterCoincidences(alice, bob, DriftConfig{}, WindowConfig{}); err == nil {
		t.Fatal("expected an error when there is no coincidence peak to track")
	}
}

// TestDriftModelOffsetInterpolates checks interpolation and linear extrapolation past the knots
func TestDriftModelOffsetInterpolates(t *testing.T) {
	m := DriftModel{TimesPS: []int64{100, 200}, OffsetsPS: []float64{10, 30}}
	for _, c := range []struct {
		t    int64
		want float64
	}{{0, -10}, {100, 10}, {150, 20}, {200, 30}, {500, 90}} {
		if got := m.Offset(c.t); math.Abs(got-c.want) > 1e-9 {
			t.Fatalf("Offset(%d) = %v, want %v", c.t, got, c.want)
		}
	}
}

// TestFitGaussianRecoversPeak: a 40 ps Gaussian on a flat accidental floor must be recovered,
// and the window must be its half width at half maximum
func TestFitGaussianRecoversPeak(t *testing.T) {
	r := rand.New(rand.NewSource(9))
	var dt []int64
	for range 20000 {
		dt = append(dt, int64(math.Round(15+r.NormFloat64()*40)))
	}
	for range 30000 {
		dt = append(dt, r.Int63n(6001)-3000)
	}
	p := measurePeak(dt, 3000, 25)
	g, err := fitGaussian(dt, 3000, p)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fit: centre %.1f ps, sigma %.1f ps, HWHM %.1f ps (rms estimate was %.1f ps)", g.CentrePS, g.SigmaPS, g.HWHM(), p.SigmaPS)
	if math.Abs(g.SigmaPS-40) > 2 || math.Abs(g.CentrePS-15) > 2 {
		t.Fatalf("fit off: centre %.1f (want 15), sigma %.1f (want 40)", g.CentrePS, g.SigmaPS)
	}
	if want := 40 * math.Sqrt(2*math.Ln2); math.Abs(g.HWHM()-want) > 2.5 {
		t.Fatalf("HWHM %.1f, want %.1f", g.HWHM(), want)
	}
}

// TestPruneToCoincidenceRangeKeepsExactlyTheReachable: every bob click within the radius of
// some alice click survives, every other one is dropped, so no possible coincidence is lost
func TestPruneToCoincidenceRangeKeepsExactlyTheReachable(t *testing.T) {
	r := rand.New(rand.NewSource(41))
	var alice, bob []DetectionEvent
	for range 2000 {
		alice = append(alice, DetectionEvent{TimestampPS: r.Int63n(1_000_000_000)})
	}
	for range 50000 {
		bob = append(bob, DetectionEvent{TimestampPS: r.Int63n(1_000_000_000)})
	}
	const radius = 20000
	kept := PruneToCoincidenceRange(alice, bob, radius)
	want := 0
	for _, b := range bob {
		for _, a := range alice {
			if d := b.TimestampPS - a.TimestampPS; d >= -radius && d <= radius {
				want++
				break
			}
		}
	}
	if len(kept) != want {
		t.Fatalf("kept %d bob clicks, brute force says %d are in range", len(kept), want)
	}
}
