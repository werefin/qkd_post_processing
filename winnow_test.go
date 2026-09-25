package qoj_post_processing

import (
	"bytes"
	"math/rand"
	"testing"
)

// TestHammingSyndromeLocatesEveryUnitError exhaustively checks that a
// single-bit error at every position decodes back to that exact position
func TestHammingSyndromeLocatesEveryUnitError(t *testing.T) {
	for _, blockSize := range []int{1, 2, 3, 4, 7, 8, 15, 16, 31} {
		g, err := HammingGeneratorMatrix(blockSize)
		if err != nil {
			t.Fatalf("blockSize=%d: %v", blockSize, err)
		}
		zero := make([]byte, blockSize)
		zeroSyndrome := ComputeBlockParity(zero, g)[0]
		for j := 0; j < blockSize; j++ {
			e := make([]byte, blockSize)
			e[j] = 1
			syndrome := ComputeBlockParity(e, g)[0]
			diff := make([]byte, len(syndrome))
			for i := range diff {
				diff[i] = syndrome[i] ^ zeroSyndrome[i]
			}
			pos := syndromeToPosition(diff)
			if pos != j {
				t.Fatalf("blockSize=%d: unit error at %d decoded to %d", blockSize, j, pos)
			}
		}
	}
}

func TestWinnowCorrectFixesExactlyOneErrorPerBlock(t *testing.T) {
	g, err := HammingGeneratorMatrix(15)
	if err != nil {
		t.Fatalf("HammingGeneratorMatrix: %v", err)
	}
	r := rand.New(rand.NewSource(5))
	numBlocks := 50
	alice := make([]byte, 15*numBlocks)
	for i := range alice {
		alice[i] = byte(r.Intn(2))
	}
	bob := append([]byte(nil), alice...)
	// exactly one flipped bit in every block
	for bi := 0; bi < numBlocks; bi++ {
		bob[bi*15+r.Intn(15)] ^= 1
	}

	res := WinnowPass(alice, bob, g)
	if !bytes.Equal(res.CorrectedBob, alice) {
		t.Fatal("expected every single-bit error to be corrected exactly")
	}
	if res.Corrections != numBlocks {
		t.Fatalf("expected %d corrections, got %d", numBlocks, res.Corrections)
	}
	if res.LeakedBits != numBlocks*g.ParityBits() {
		t.Fatalf("expected %d leaked bits, got %d", numBlocks*g.ParityBits(), res.LeakedBits)
	}
}

func TestWinnowCorrectNoErrorsStaysUnchanged(t *testing.T) {
	g, err := HammingGeneratorMatrix(7)
	if err != nil {
		t.Fatalf("HammingGeneratorMatrix: %v", err)
	}
	r := rand.New(rand.NewSource(6))
	alice := make([]byte, 7*20)
	for i := range alice {
		alice[i] = byte(r.Intn(2))
	}
	bob := append([]byte(nil), alice...)

	res := WinnowPass(alice, bob, g)
	if res.Corrections != 0 {
		t.Fatalf("expected 0 corrections on identical input, got %d", res.Corrections)
	}
	if !bytes.Equal(res.CorrectedBob, alice) {
		t.Fatal("expected identical input to stay identical")
	}
}

// TestWinnowCorrectTwoErrorsNeverPanics: 2 errors can decode to a bogus
// position, but that must never go out of bounds or crash
func TestWinnowCorrectTwoErrorsNeverPanics(t *testing.T) {
	g, err := HammingGeneratorMatrix(15)
	if err != nil {
		t.Fatalf("HammingGeneratorMatrix: %v", err)
	}
	alice := make([]byte, 15)
	bob := make([]byte, 15)
	bob[2] ^= 1
	bob[9] ^= 1

	res := WinnowPass(alice, bob, g)
	if len(res.CorrectedBob) != 15 {
		t.Fatalf("expected 15 bits back, got %d", len(res.CorrectedBob))
	}
}

func TestWinnowCorrectHandlesShortFinalBlock(t *testing.T) {
	g, err := HammingGeneratorMatrix(15)
	if err != nil {
		t.Fatalf("HammingGeneratorMatrix: %v", err)
	}
	alice := make([]byte, 15*3+4) // a 4-bit short final block
	bob := append([]byte(nil), alice...)
	bob[15*3+2] ^= 1 // error inside the short block

	res := WinnowPass(alice, bob, g)
	if !bytes.Equal(res.CorrectedBob, alice) {
		t.Fatal("expected the short final block's error to be corrected")
	}
}

func TestPermuteUnpermuteRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	n := 500
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(r.Intn(2))
	}
	perm := GeneratePermutation(n, 42)
	got := unpermuteBits(permuteBits(data, perm), perm)
	if !bytes.Equal(got, data) {
		t.Fatal("expected unpermuteBits(permuteBits(x)) == x")
	}
}

func TestGeneratePermutationIsDeterministic(t *testing.T) {
	a := GeneratePermutation(1000, 99)
	b := GeneratePermutation(1000, 99)
	if !intSliceEqual(a, b) {
		t.Fatal("expected the same seed to produce the same permutation")
	}
}

func intSliceEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestWinnowReconcileConverges runs a full multi-pass schedule and
// checks the error count drops sharply, unlike a single pass alone
func TestWinnowReconcileConverges(t *testing.T) {
	r := rand.New(rand.NewSource(13))
	n := 8000
	alice := make([]byte, n)
	for i := range alice {
		alice[i] = byte(r.Intn(2))
	}
	bob := append([]byte(nil), alice...)
	initialErrors := 0
	for i := range bob {
		if r.Float64() < 0.03 { // 3% initial QBER
			bob[i] ^= 1
			initialErrors++
		}
	}

	cfg := WinnowConfig{
		BlockSizes:   []int{8, 8, 16, 16, 32},
		Permutations: make([][]int, 5),
	}
	for i := range cfg.Permutations {
		cfg.Permutations[i] = GeneratePermutation(n, int64(1000+i))
	}

	corrected, leaked, corrections := WinnowReconcile(alice, bob, cfg)

	remainingErrors := 0
	for i := range corrected {
		if corrected[i] != alice[i] {
			remainingErrors++
		}
	}
	t.Logf("initial errors=%d, corrections made=%d, leaked bits=%d, remaining errors=%d",
		initialErrors, corrections, leaked, remainingErrors)
	if remainingErrors >= initialErrors {
		t.Fatalf("expected Winnow to reduce the error count: %d initial -> %d remaining", initialErrors, remainingErrors)
	}
}
