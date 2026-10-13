package qkd_post_processing

import "testing"

// TestSearchGeneratorMatrixImprovesInSample: the search must improve its own objective on its
// training data; out-of-sample it is logged, not asserted, because it does not generalize
func TestSearchGeneratorMatrixImprovesInSample(t *testing.T) {
	const window = 500
	const chunk = 2048

	trainAlice, trainBob := simulateClicks(300000, 0.03, 1)
	best, _ := SearchGeneratorMatrix(32, 6, trainAlice, trainBob, window, chunk, 1500, 99)

	rate := func(alice, bob []DetectionEvent, g GeneratorMatrix) float64 {
		res, err := Run(alice, bob, Config{CoincidenceWindowPS: window, GeneratorMatrix: g, ChunkSize: chunk})
		if err != nil || res.SiftedBits == 0 {
			t.Fatalf("Run: %v (sifted=%d)", err, res.SiftedBits)
		}
		return float64(len(res.FinalKeyBits)) / float64(res.SiftedBits)
	}
	inSearched, inBaseline := rate(trainAlice, trainBob, best), rate(trainAlice, trainBob, testMatrixC)
	t.Logf("training data: searched=%.5f baseline=%.5f bits/sifted", inSearched, inBaseline)
	if inSearched < inBaseline {
		t.Fatalf("search did not beat the baseline on its own training data: %.5f < %.5f", inSearched, inBaseline)
	}
	for _, seed := range []int64{2, 3, 4} {
		a, b := simulateClicks(300000, 0.03, seed)
		s, bl := rate(a, b, best), rate(a, b, testMatrixC)
		t.Logf("held-out seed=%d: searched=%.5f baseline=%.5f ratio=%.2fx", seed, s, bl, s/bl)
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
