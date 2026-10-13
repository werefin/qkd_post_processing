package qkd_post_processing

import (
	"math"
	"math/rand"
	"testing"
)

// timingLink simulates the error mechanism timing-aware reconciliation targets: true pairs
// with Gaussian jitter and a small intrinsic error, accidentals uniform over +-window with random bits
func timingLink(pairs int, accidentalFrac, intrinsicErr float64, jitterPS, windowPS int64, seed int64) (alice, bob []DetectionEvent) {
	r := rand.New(rand.NewSource(seed))
	const spacingPS = 1_000_000 // far above any window, so no click is ever ambiguous
	alice = make([]DetectionEvent, pairs)
	bob = make([]DetectionEvent, pairs)
	for i := range pairs {
		t := int64(i) * spacingPS
		basis := Basis(r.Intn(2))
		bit := byte(r.Intn(2))
		var dt int64
		bobBit := bit
		if r.Float64() < accidentalFrac {
			dt = r.Int63n(2*windowPS+1) - windowPS
			bobBit = byte(r.Intn(2))
		} else {
			dt = int64(math.Round(r.NormFloat64() * float64(jitterPS)))
			dt = max(-windowPS, min(windowPS, dt))
			if r.Float64() < intrinsicErr {
				bobBit ^= 1
			}
		}
		alice[i] = DetectionEvent{TimestampPS: t, Basis: basis, Bit: bit}
		bob[i] = DetectionEvent{TimestampPS: t + dt, Basis: basis, Bit: bobBit}
	}
	return alice, bob
}

// TestSlepianWolfBoundsJensen: timing can only lower the disclosure bound, and changes
// nothing when every bit has the same error rate
func TestSlepianWolfBoundsJensen(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for trial := 0; trial < 200; trial++ {
		q := make([]float64, 100)
		for i := range q {
			q[i] = r.Float64() * 0.5
		}
		soft, flat := SlepianWolfBounds(q)
		if soft > flat+1e-12 {
			t.Fatalf("trial %d: soft bound %.6f exceeds flat %.6f, contradicts Jensen", trial, soft, flat)
		}
	}
	soft, flat := SlepianWolfBounds([]float64{0.05, 0.05, 0.05})
	if math.Abs(soft-flat) > 1e-12 {
		t.Fatalf("uniform q: soft %.6f and flat %.6f should be equal", soft, flat)
	}
}

// TestSoftLLRs: sign follows the received bit, magnitude follows reliability, q=0.5 is no information
func TestSoftLLRs(t *testing.T) {
	llr := SoftLLRs([]byte{0, 1, 0, 1}, []float64{0.01, 0.01, 0.5, 0.2})
	if !(llr[0] > 0 && llr[1] < 0 && llr[0] == -llr[1]) {
		t.Fatalf("sign/symmetry wrong: %v", llr)
	}
	if math.Abs(llr[2]) > 1e-9 {
		t.Fatalf("q=0.5 must give LLR 0, got %v", llr[2])
	}
	if !(math.Abs(llr[3]) < math.Abs(llr[1])) {
		t.Fatalf("a less reliable bit must get a smaller |LLR|: %v", llr)
	}
}

// TestEstimateTimingErrorModelRecoversProfile: centre bins see mostly true pairs, edge bins
// mostly accidentals at ~50% error
func TestEstimateTimingErrorModelRecoversProfile(t *testing.T) {
	const window = 2000
	alice, bob := timingLink(200000, 0.2, 0.01, 100, window, 2)
	sifted := Sift(alice, bob, window)
	al, bo, dt := make([]byte, len(sifted)), make([]byte, len(sifted)), make([]int64, len(sifted))
	for i, s := range sifted {
		al[i], bo[i] = s.AliceBit, s.BobBit
		dt[i] = bob[s.BobIdx].TimestampPS - alice[s.AliceIdx].TimestampPS
	}
	m, err := EstimateTimingErrorModel(al, bo, dt, window, 16)
	if err != nil {
		t.Fatal(err)
	}
	centre, edge := m.Rate(0), m.Rate(window-1)
	t.Logf("q(centre)=%.4f q(edge)=%.4f", centre, edge)
	if centre > 0.05 || edge < 0.4 {
		t.Fatalf("expected a low centre and ~0.5 edge rate, got %.4f and %.4f", centre, edge)
	}
}

// TestEstimateTimingErrorModelRejectsBadInput
func TestEstimateTimingErrorModelRejectsBadInput(t *testing.T) {
	if _, err := EstimateTimingErrorModel(nil, nil, nil, 0, 8); err == nil {
		t.Fatal("expected an error for windowPS=0")
	}
	if _, err := EstimateTimingErrorModel([]byte{0}, []byte{0, 1}, []int64{0}, 100, 8); err == nil {
		t.Fatal("expected an error for mismatched slices")
	}
}

// TestRunPolarTimingBeatsFlat: same link, seed and margin; timing must disclose fewer
// bits, fail no more blocks, and keep strictly more final key
func TestRunPolarTimingBeatsFlat(t *testing.T) {
	const window = 2000
	alice, bob := timingLink(120000, 0.06, 0.01, 100, window, 3)
	base := PolarRunConfig{CoincidenceWindowPS: window, SampleFraction: 0.1, BlockLength: 4096, RateMargin: 0.1, ListSize: 8, Seed: 4, ChunkSize: 256}
	flat, err := RunPolar(alice, bob, base)
	if err != nil {
		t.Fatal(err)
	}
	soft := base
	soft.TimingBins = 16
	timed, err := RunPolar(alice, bob, soft)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("QBER=%.4f bounds: flat=%.4f soft=%.4f bits/bit", flat.QBER.QBER, timed.Timing.FlatBound, timed.Timing.SoftBound)
	t.Logf("flat: leaked=%d crc_dropped=%d final=%d bits", flat.PolarLeakedBits, flat.CRCVerify.ChunksDropped, len(flat.FinalKeyBits))
	t.Logf("soft: leaked=%d crc_dropped=%d final=%d bits", timed.PolarLeakedBits, timed.CRCVerify.ChunksDropped, len(timed.FinalKeyBits))
	if timed.PolarLeakedBits >= flat.PolarLeakedBits {
		t.Fatalf("timing should disclose fewer bits: soft %d vs flat %d", timed.PolarLeakedBits, flat.PolarLeakedBits)
	}
	if timed.CRCVerify.ChunksDropped > flat.CRCVerify.ChunksDropped {
		t.Fatalf("timing should not fail more chunks: soft %d vs flat %d", timed.CRCVerify.ChunksDropped, flat.CRCVerify.ChunksDropped)
	}
	if len(timed.FinalKeyBits) <= len(flat.FinalKeyBits) {
		t.Fatalf("timing should keep strictly more final key: soft %d vs flat %d", len(timed.FinalKeyBits), len(flat.FinalKeyBits))
	}
}

// TestRunLDPCTimingConvergesMore: at rate 0.7, above flat decoding's reach on this link but
// below the timing-aware bound, per-bit LLRs must converge every block on the same syndromes
func TestRunLDPCTimingConvergesMore(t *testing.T) {
	const window = 2000
	alice, bob := timingLink(120000, 0.06, 0.01, 100, window, 3)
	base := LDPCRunConfig{CoincidenceWindowPS: window, SampleFraction: 0.1, BlockLength: 4000, ColWeight: 3, RowWeight: 10, Seed: 6, ChunkSize: 256}
	flat, err := RunLDPC(alice, bob, base)
	if err != nil {
		t.Fatal(err)
	}
	soft := base
	soft.TimingBins = 16
	timed, err := RunLDPC(alice, bob, soft)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("QBER=%.4f rate=0.7: flat converged %d/%d final=%d, soft converged %d/%d final=%d (same %d leaked bits)",
		flat.QBER.QBER, flat.LDPCConverged, flat.LDPCBlocks, len(flat.FinalKeyBits), timed.LDPCConverged, timed.LDPCBlocks, len(timed.FinalKeyBits), timed.LDPCLeakedBits)
	if timed.LDPCLeakedBits != flat.LDPCLeakedBits {
		t.Fatalf("LDPC disclosure must not change with timing: %d vs %d", timed.LDPCLeakedBits, flat.LDPCLeakedBits)
	}
	if timed.LDPCConverged != timed.LDPCBlocks || timed.LDPCConverged <= flat.LDPCConverged {
		t.Fatalf("expected soft to converge all blocks and beat flat: soft %d/%d, flat %d", timed.LDPCConverged, timed.LDPCBlocks, flat.LDPCConverged)
	}
}

// TestRunPolarTimingNeedsSample: a timing model can't be fitted without a QBER sample
func TestRunPolarTimingNeedsSample(t *testing.T) {
	alice, bob := timingLink(5000, 0.1, 0.01, 100, 2000, 7)
	_, err := RunPolar(alice, bob, PolarRunConfig{CoincidenceWindowPS: 2000, SampleFraction: 0, BlockLength: 1024, RateMargin: 0.1, Seed: 1, ChunkSize: 256, TimingBins: 8})
	if err == nil {
		t.Fatal("expected an error when TimingBins>0 but SampleFraction=0")
	}
}
