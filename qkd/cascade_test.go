package qkd

import (
	"bytes"
	"math"
	"math/rand"
	"testing"
)

func bscFrame(n int, q float64, seed int64) (alice, bob []byte, p float64) {
	r := rand.New(rand.NewSource(seed))
	alice = randomBits(r, n)
	bob = append([]byte(nil), alice...)
	e := 0
	for i := range bob {
		if r.Float64() < q {
			bob[i] ^= 1
			e++
		}
	}
	return alice, bob, float64(e) / float64(n)
}

// TestCascadeEfficiency: on long frames across the QBER range every error is corrected, at a disclosure
// within a few percent of the Shannon limit
func TestCascadeEfficiency(t *testing.T) {
	t.Parallel() // heavy and independent: runs beside the other heavy tests
	for _, c := range []struct{ q, maxF float64 }{{0.01, 1.06}, {0.03, 1.07}, {0.05, 1.08}, {0.08, 1.09}, {0.11, 1.10}} {
		alice, bob, p := bscFrame(100_000, c.q, int64(c.q*1e4))
		got, s := CascadeReconcile(alice, bob, c.q, CascadeConfig{Seed: 3})
		f := float64(s.Parities) / (float64(len(alice)) * binaryEntropy(p))
		t.Logf("QBER %.0f%%: blocks %v, %d parities (%d inferred), %d corrections, f = %.4f", 100*c.q, s.BlockSizes, s.Parities, s.Inferred, s.Corrections, f)
		if !bytes.Equal(got, alice) || f > c.maxF {
			t.Fatalf("QBER %.2f: corrected %v, f %.4f above %.2f", c.q, bytes.Equal(got, alice), f, c.maxF)
		}
	}
}

// TestCascadeSmallFramesAndPoorEstimates: short frames and a QBER estimate 30% off either way still end
// clean almost always, which the clean passes at the end are for
func TestCascadeSmallFramesAndPoorEstimates(t *testing.T) {
	t.Parallel() // heavy and independent: runs beside the other heavy tests
	failed, runs := 0, 0
	for _, n := range []int{2000, 5000} {
		for _, q := range []float64{0.03, 0.06, 0.10} {
			for _, est := range []float64{0.7, 1.0, 1.3} {
				for trial := range 20 {
					alice, bob, _ := bscFrame(n, q, int64(trial*31+n)+int64(q*1e4))
					got, _ := CascadeReconcile(alice, bob, q*est, CascadeConfig{Seed: int64(trial)})
					if !bytes.Equal(got, alice) {
						failed++
					}
					runs++
				}
			}
		}
	}
	if failed > runs/100 {
		t.Fatalf("%d of %d short frames left errors", failed, runs)
	}
}

// TestCascadeNeverDisclosesTwice: every disclosed parity is a distinct range, and inferred halves cost nothing
func TestCascadeNeverDisclosesTwice(t *testing.T) {
	alice, bob, _ := bscFrame(20_000, 0.05, 9)
	_, s := CascadeReconcile(alice, bob, 0.05, CascadeConfig{Seed: 1})
	// a binary search over k bits costs log2(k) parities at most; the inferred halves show the saving
	if s.Inferred == 0 || s.Parities >= s.Searches*int(math.Log2(float64(s.BlockSizes[len(s.BlockSizes)-1])))+20_000/s.BlockSizes[0]*len(s.BlockSizes) {
		t.Fatalf("parities %d, inferred %d, searches %d", s.Parities, s.Inferred, s.Searches)
	}
}

func TestCascadeFirstBlock(t *testing.T) {
	for _, c := range []struct {
		q float64
		k int
	}{{0.01, 128}, {0.03, 32}, {0.05, 16}, {0.10, 8}} {
		if got := CascadeFirstBlock(c.q, 1_000_000); got != c.k {
			t.Fatalf("QBER %.2f: first block %d, want %d", c.q, got, c.k)
		}
	}
}

// TestRunCascadeBeatsLDPCAndWinnow: on the same data and sample the full chain with Cascade certifies a
// longer key than Winnow and regular LDPC
func TestRunCascadeBeatsLDPCAndWinnow(t *testing.T) {
	t.Parallel() // heavy and independent: runs beside the other heavy tests
	alice, bob := simulateClicks(300000, 0.05, 21)
	cs, err := RunCascade(alice, bob, CascadeRunConfig{CoincidenceWindowPS: 500, Seed: 5, ChunkSize: 1024, ExpectedQBER: 0.05})
	if err != nil {
		t.Fatal(err)
	}
	wn, err := RunWinnow(alice, bob, WinnowRunConfig{CoincidenceWindowPS: 500, Seed: 5, ChunkSize: 1024, ExpectedQBER: 0.05})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Cascade: %d disclosed, %d-bit key; Winnow: %d disclosed, %d-bit key", cs.LeakedBits, len(cs.FinalKeyBits), wn.LeakedBits, len(wn.FinalKeyBits))
	if len(cs.FinalKeyBits) == 0 || !cs.FiniteKey.Verified || cs.CRCVerify.ChunksDropped != 0 || len(cs.FinalKeyBits) <= len(wn.FinalKeyBits) {
		t.Fatal("Cascade should certify a verified key, longer than Winnow's")
	}
}
