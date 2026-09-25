package qoj_post_processing

import (
	"math"
	"testing"
)

// TestSearchGeneratorMatrixBeatsEx3OutOfSample checks a searched matrix
// beats ex3 on data it never saw during search, not just training noise
func TestSearchGeneratorMatrixBeatsEx3OutOfSample(t *testing.T) {
	const window = 500
	const chunk = 2048

	trainAlice, trainBob := simulateClicks(300000, 0.03, 1)
	best, _ := SearchGeneratorMatrix(32, 6, trainAlice, trainBob, window, chunk, 1500, 99)

	minRatio := math.Inf(1)
	for _, seed := range []int64{2, 3, 4} {
		testAlice, testBob := simulateClicks(300000, 0.03, seed)

		res, err := Run(testAlice, testBob, Config{CoincidenceWindowPS: window, GeneratorMatrix: best, ChunkSize: chunk})
		if err != nil {
			t.Fatalf("seed=%d: searched matrix Run: %v", seed, err)
		}
		ex3Res, err := Run(testAlice, testBob, Config{CoincidenceWindowPS: window, GeneratorMatrix: GeneratorMatrixEx3, ChunkSize: chunk})
		if err != nil {
			t.Fatalf("seed=%d: ex3 Run: %v", seed, err)
		}
		if ex3Res.SiftedBits == 0 || res.SiftedBits == 0 {
			t.Fatalf("seed=%d: expected nonzero sifted bits", seed)
		}
		searchRate := float64(len(res.FinalKeyBits)) / float64(res.SiftedBits)
		ex3Rate := float64(len(ex3Res.FinalKeyBits)) / float64(ex3Res.SiftedBits)
		ratio := searchRate / ex3Rate
		t.Logf("seed=%d: searched=%.5f bits/sifted (%d bits), ex3=%.5f bits/sifted (%d bits), ratio=%.2fx",
			seed, searchRate, len(res.FinalKeyBits), ex3Rate, len(ex3Res.FinalKeyBits), ratio)
		minRatio = min(minRatio, ratio)
	}
	if minRatio < 1.15 {
		t.Fatalf("expected the searched matrix to beat ex3 by at least 15%% on every held-out seed, worst ratio was %.2fx", minRatio)
	}
}

// TestSearchGeneratorMatrixDeterministic checks the same seed reproduces
// the same search result, both sides must derive it independently
func TestSearchGeneratorMatrixDeterministic(t *testing.T) {
	alice, bob := simulateClicks(20000, 0.03, 1)
	a, fitnessA := SearchGeneratorMatrix(16, 4, alice, bob, 500, 512, 200, 7)
	b, fitnessB := SearchGeneratorMatrix(16, 4, alice, bob, 500, 512, 200, 7)
	if fitnessA != fitnessB {
		t.Fatalf("expected the same seed to reach the same fitness, got %v and %v", fitnessA, fitnessB)
	}
	for i := range a {
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				t.Fatalf("expected the same seed to reproduce the same matrix, differs at [%d][%d]", i, j)
			}
		}
	}
}
