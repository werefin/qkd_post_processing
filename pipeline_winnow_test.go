package qoj_post_processing

import (
	"math/rand"
	"testing"
)

// simulateClicks builds correlated Alice/Bob click streams at the given intrinsic bit error rate
func simulateClicks(n int, errRate float64, seed int64) (alice, bob []DetectionEvent) {
	r := rand.New(rand.NewSource(seed))
	tcur := int64(0)
	for range n {
		tcur += 1000
		aBasis := Basis(r.Intn(2))
		bBasis := Basis(r.Intn(2))
		aBit := byte(r.Intn(2))
		bBit := aBit
		if aBasis == bBasis && r.Float64() < errRate {
			bBit ^= 1
		} else if aBasis != bBasis {
			bBit = byte(r.Intn(2))
		}
		alice = append(alice, DetectionEvent{TimestampPS: tcur, Basis: aBasis, Bit: aBit})
		bob = append(bob, DetectionEvent{TimestampPS: tcur, Basis: bBasis, Bit: bBit})
	}
	return alice, bob
}

// TestRunWinnowEndToEnd checks the same invariants as TestRunEndToEnd
func TestRunWinnowEndToEnd(t *testing.T) {
	alice, bob := simulateClicks(50000, 0.02, 55)
	cfg := WinnowRunConfig{
		CoincidenceWindowPS: 500,
		SampleFraction:      0.05,
		WinnowBlockSizes:    []int{25, 50, 100},
		Seed:                1,
		ChunkSize:           128,
	}
	res, err := RunWinnow(alice, bob, cfg)
	if err != nil {
		t.Fatalf("RunWinnow returned error: %v", err)
	}
	if res.SiftedBits == 0 {
		t.Fatal("expected nonzero sifted bits")
	}
	for i := range res.CRCVerify.SurvivingAlice {
		if res.CRCVerify.SurvivingAlice[i] != res.CRCVerify.SurvivingBob[i] {
			t.Fatalf("error-free stream has a residual mismatch at %d", i)
		}
	}
	if len(res.FinalKeyBits) > len(res.CRCVerify.SurvivingAlice) {
		t.Fatalf("final key (%d bits) longer than error-free input (%d bits)",
			len(res.FinalKeyBits), len(res.CRCVerify.SurvivingAlice))
	}
	if len(res.FinalKeyBits) == 0 {
		t.Fatal("expected a nonzero final key at 2% QBER over 50000 attempts")
	}
}

// TestRunWinnowZeroSampleFraction: with nothing sacrificed for QBER, the estimate is 0 regardless of the real error rate
func TestRunWinnowZeroSampleFraction(t *testing.T) {
	alice, bob := simulateClicks(5000, 0.02, 9)
	cfg := WinnowRunConfig{
		CoincidenceWindowPS: 500,
		SampleFraction:      0,
		WinnowBlockSizes:    []int{8, 16},
		Seed:                1,
		ChunkSize:           512,
	}
	res, err := RunWinnow(alice, bob, cfg)
	if err != nil {
		t.Fatalf("RunWinnow returned error: %v", err)
	}
	if res.QBER.SampleBits != 0 || res.QBER.QBER != 0 {
		t.Fatalf("expected a zero-bit, zero QBER sample, got %+v", res.QBER)
	}
}

func TestRunWinnowVsBlockDiscard(t *testing.T) {
	alice, bob := simulateClicks(50000, 0.02, 55)

	discardRes, err := Run(alice, bob, Config{
		CoincidenceWindowPS: 500,
		GeneratorMatrix:     testMatrixC,
		ChunkSize:           2048,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	winnowRes, err := RunWinnow(alice, bob, WinnowRunConfig{
		CoincidenceWindowPS: 500,
		SampleFraction:      0.05,
		WinnowBlockSizes:    []int{25, 50, 100},
		Seed:                1,
		ChunkSize:           128,
	})
	if err != nil {
		t.Fatalf("RunWinnow returned error: %v", err)
	}
	t.Logf("discard(baseline): sifted=%d leaked=%d final=%d bits | winnow: sifted=%d leaked=%d final=%d bits",
		discardRes.SiftedBits, discardRes.LeakedBits, len(discardRes.FinalKeyBits),
		winnowRes.SiftedBits, winnowRes.LeakedBits, len(winnowRes.FinalKeyBits))
}
