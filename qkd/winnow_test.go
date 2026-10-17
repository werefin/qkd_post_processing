package qkd

import (
	"bytes"
	"math"
	"math/rand"
	"testing"
)

// TestWinnowPassFixesOneErrorAnywhere puts one error at every position of a block, the parity-only
// position 0 included, and checks the parity and syndrome find it
func TestWinnowPassFixesOneErrorAnywhere(t *testing.T) {
	r := rand.New(rand.NewSource(5))
	for _, size := range []int{4, 8, 16, 32} {
		alice := randomBits(r, size)
		for j := range size {
			bob := append([]byte(nil), alice...)
			bob[j] ^= 1
			res := WinnowPass(alice, bob, size)
			if !bytes.Equal(res.CorrectedBob, alice) || res.Corrections != 1 || res.Mismatched != 1 {
				t.Fatalf("block %d, error at %d: corrected=%v corrections=%d", size, j, bytes.Equal(res.CorrectedBob, alice), res.Corrections)
			}
			// one parity and log2(size) syndrome bits
			if want := 1 + int(math.Log2(float64(size))); res.LeakedBits != want {
				t.Fatalf("block %d: leaked %d bits, want %d", size, res.LeakedBits, want)
			}
		}
	}
}

// TestWinnowPassLeavesEvenErrors: two errors keep the parities equal, so the block is left for a later
// pass instead of being miscorrected into three errors, and only its parity is disclosed
func TestWinnowPassLeavesEvenErrors(t *testing.T) {
	r := rand.New(rand.NewSource(6))
	alice := randomBits(r, 16)
	bob := append([]byte(nil), alice...)
	bob[3] ^= 1
	bob[11] ^= 1
	res := WinnowPass(alice, bob, 16)
	if !bytes.Equal(res.CorrectedBob, bob) || res.Corrections != 0 || res.LeakedBits != 1 {
		t.Fatalf("two errors: corrections=%d leaked=%d, want an untouched block and one parity bit", res.Corrections, res.LeakedBits)
	}
}

// TestWinnowPassShortFinalBlock: a trailing block shorter than the rest is corrected with a shorter syndrome
func TestWinnowPassShortFinalBlock(t *testing.T) {
	r := rand.New(rand.NewSource(8))
	alice := randomBits(r, 21) // blocks of 8, 8 and 5
	bob := append([]byte(nil), alice...)
	bob[20] ^= 1
	res := WinnowPass(alice, bob, 8)
	if !bytes.Equal(res.CorrectedBob, alice) {
		t.Fatal("error in the short final block not corrected")
	}
	if want := 3 + 3; res.LeakedBits != want { // three parities, a 3-bit syndrome for positions 0..4
		t.Fatalf("leaked %d bits, want %d", res.LeakedBits, want)
	}
}

func TestWinnowScheduleFollowsQBER(t *testing.T) {
	for _, c := range []struct {
		qber  float64
		first int
	}{{0.01, 16}, {0.03, 16}, {0.05, 8}, {0.08, 8}, {0.1, 4}} {
		s := WinnowSchedule(c.qber, 100_000)
		if s[0] != c.first || s[1] != 2*c.first {
			t.Fatalf("QBER %.2f: schedule starts %v, want %d then %d", c.qber, s[:2], c.first, 2*c.first)
		}
	}
}

// TestWinnowReconcileEfficiency: on a long block at the QBERs of real links, the planned schedule
// corrects every error, or leaves a handful for the CRC, at a disclosure close to the Shannon limit
func TestWinnowReconcileEfficiency(t *testing.T) {
	r := rand.New(rand.NewSource(13))
	n := 200_000
	for _, q := range []float64{0.02, 0.05, 0.08} {
		alice := randomBits(r, n)
		bob := append([]byte(nil), alice...)
		errors := 0
		for i := range bob {
			if r.Float64() < q {
				bob[i] ^= 1
				errors++
			}
		}
		corrected, leaked, _ := WinnowReconcile(alice, bob, WinnowConfig{BlockSizes: WinnowSchedule(q, n), Seed: 1})
		left := 0
		for i := range corrected {
			if corrected[i] != alice[i] {
				left++
			}
		}
		p := float64(errors) / float64(n)
		f := float64(leaked) / (float64(n) * (-p*math.Log2(p) - (1-p)*math.Log2(1-p)))
		t.Logf("QBER %.0f%%: %d errors, %d left, leaked %d bits, f = %.3f", 100*q, errors, left, leaked, f)
		if left > errors/1000 || f > 1.6 {
			t.Fatalf("QBER %.0f%%: %d errors left of %d, f = %.3f", 100*q, left, errors, f)
		}
	}
}

func TestPermuteUnpermuteRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	data := randomBits(r, 500)
	perm := GeneratePermutation(len(data), 42)
	if got := unpermuteBits(permuteBits(data, perm), perm); !bytes.Equal(got, data) {
		t.Fatal("expected unpermuteBits(permuteBits(x)) == x")
	}
}

func TestGeneratePermutationIsDeterministic(t *testing.T) {
	a, b := GeneratePermutation(1000, 99), GeneratePermutation(1000, 99)
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("expected the same seed to produce the same permutation")
		}
	}
}
