package qoj_post_processing

import (
	"math/rand"
	"testing"
)

// toeplitzImpls covers every implementation this build can reach, so
// non-default ones (e.g. the portable fallback on amd64) stay verified
var toeplitzImpls = map[string]func(x, seed []byte, l int) []byte{
	"Fast":     toeplitzHashFast,
	"Popcount": toeplitzHashPopcount,
}

// TestToeplitzImplsMatchNaive: each impl must be bit-for-bit identical
// to the O(l*n) reference, across every shape we exercise
func TestToeplitzImplsMatchNaive(t *testing.T) {
	shapes := []struct{ n, l int }{
		{1, 1}, {1, 5}, {5, 1}, {63, 1}, {64, 1}, {65, 1},
		{100, 37}, {128, 128}, {200, 1}, {17, 200}, {513, 260},
	}
	for name, impl := range toeplitzImpls {
		t.Run(name, func(t *testing.T) {
			r := rand.New(rand.NewSource(99))
			for _, sh := range shapes {
				x := randomBits(r, sh.n)
				seed := randomBits(r, sh.l+sh.n-1)
				want := toeplitzHashNaive(x, seed, sh.l)
				got := impl(x, seed, sh.l)
				if len(want) != len(got) {
					t.Fatalf("n=%d l=%d: length mismatch naive=%d got=%d", sh.n, sh.l, len(want), len(got))
				}
				for i := range want {
					if want[i] != got[i] {
						t.Fatalf("n=%d l=%d: mismatch at output bit %d: naive=%d got=%d", sh.n, sh.l, i, want[i], got[i])
					}
				}
			}
		})
	}
}

func TestToeplitzImplsRandomizedAgainstNaive(t *testing.T) {
	for name, impl := range toeplitzImpls {
		t.Run(name, func(t *testing.T) {
			r := rand.New(rand.NewSource(123))
			for trial := range 30 {
				n := 1 + r.Intn(500)
				l := 1 + r.Intn(500)
				x := randomBits(r, n)
				seed := randomBits(r, l+n-1)
				want := toeplitzHashNaive(x, seed, l)
				got := impl(x, seed, l)
				for i := range want {
					if want[i] != got[i] {
						t.Fatalf("trial %d (n=%d l=%d): mismatch at bit %d", trial, n, l, i)
					}
				}
			}
		})
	}
}

func TestSecureKeyLengthMonotonicity(t *testing.T) {
	// higher QBER or higher leakage should never yield a longer key
	base := SecureKeyLength(10000, 0.02, 100)
	higherQBER := SecureKeyLength(10000, 0.05, 100)
	higherLeak := SecureKeyLength(10000, 0.02, 5000)
	if higherQBER > base {
		t.Errorf("expected higher QBER to reduce or keep key length, got base=%d higherQBER=%d", base, higherQBER)
	}
	if higherLeak > base {
		t.Errorf("expected higher leakage to reduce or keep key length, got base=%d higherLeak=%d", base, higherLeak)
	}
}

func TestSecureKeyLengthNeverNegativeOrOverN(t *testing.T) {
	l := SecureKeyLength(100, 0.5, 0)
	if l < 0 || l > 100 {
		t.Errorf("key length out of bounds: %d", l)
	}
	l = SecureKeyLength(100, 0.0, 1000000)
	if l != 0 {
		t.Errorf("expected 0 when leakage exceeds budget, got %d", l)
	}
}

func randomBits(r *rand.Rand, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(r.Intn(2))
	}
	return out
}
