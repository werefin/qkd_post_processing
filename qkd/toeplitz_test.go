package qkd

import (
	"bytes"
	"math/big"
	"math/rand"
	"testing"
)

// toeplitzImpls covers every implementation this build can reach, so
// non-default ones (e.g. the portable fallback on amd64) stay verified
var toeplitzImpls = map[string]func(x, seed []byte, l int) []byte{
	"Fast":     toeplitzHashFast,
	"Popcount": toeplitzHashPopcount,
	"NTT":      toeplitzHashNTT,
}

// TestNTTFieldConstants: p is a prime below 2^30 with 2^26 dividing p-1, 3 generates its multiplicative group
// (no power (p-1)/q is 1 for the prime factors q of p-1, 2 and 7), and the lazy arithmetic is right
func TestNTTFieldConstants(t *testing.T) {
	if !big.NewInt(nttP).ProbablyPrime(40) || nttP >= 1<<30 || (nttP-1)%(1<<nttMaxLog) != 0 || (nttP-1)>>nttMaxLog != 7 {
		t.Fatal("p is not a prime below 2^30 with p-1 = 7*2^26")
	}
	if nttPow(nttG, (nttP-1)/2) == 1 || nttPow(nttG, (nttP-1)/7) == 1 {
		t.Fatal("3 does not generate the group mod p")
	}
	if uint32(nttP)*nttPInv != 1 {
		t.Fatal("p^-1 mod 2^32 is wrong")
	}
	rInv := uint64(nttPow(uint32((1<<32)%nttP), nttP-2))
	r := rand.New(rand.NewSource(7))
	for range 200000 {
		a, b, y := uint64(r.Int63n(2*nttP)), uint64(r.Int63n(2*nttP)), r.Uint32()
		if got, want := uint64(nttRedc(a*b)), a*b%nttP*rInv%nttP; got != want {
			t.Fatalf("redc(%d*%d) = %d, want %d", a, b, got, want)
		}
		w := uint32(r.Int63n(nttP))
		if got := nttShoup(y, w, uint32((uint64(w)<<32)/nttP)); got >= 2*nttP || uint64(got)%nttP != uint64(w)*uint64(y)%nttP {
			t.Fatalf("Shoup %d*%d = %d", w, y, got)
		}
		if x := uint32(r.Int63n(4 * nttP)); nttLow2(x) != x%(2*nttP) || nttLow(x%(2*nttP)) != x%nttP {
			t.Fatalf("reductions wrong at %d", x)
		}
	}
}

// TestToeplitzNTTMatchesCLMULAtScale: the transform against the bit-level product on blocks the naive oracle
// is too slow for, across transform lengths, with output both shorter and longer than the input
func TestToeplitzNTTMatchesCLMULAtScale(t *testing.T) {
	r := rand.New(rand.NewSource(31))
	for _, sh := range []struct{ n, l int }{{1 << 12, 1 << 11}, {5000, 3001}, {65537, 1}, {40000, 90000}, {200000, 100000}, {300001, 524288 - 300001}, {1, 1}, {2, 1}, {1, 3}} {
		x, seed := randomBits(r, sh.n), randomBits(r, sh.l+sh.n-1)
		if got, want := toeplitzHashNTT(x, seed, sh.l), toeplitzImpl(x, seed, sh.l); !bytes.Equal(got, want) {
			t.Fatalf("n=%d l=%d: NTT and bit-level product differ", sh.n, sh.l)
		}
	}
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
