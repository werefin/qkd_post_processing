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
		t.Logf("QBER %.0f%%: blocks %v, %d parities (%d inferred), %d corrections, %d round trips, f = %.4f", 100*c.q, s.BlockSizes, s.Parities, s.Inferred, s.Corrections, s.Rounds, f)
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

// TestRunCascadeCertifiesVerifiedKey: the full chain with Cascade certifies a verified key, every chunk kept
func TestRunCascadeCertifiesVerifiedKey(t *testing.T) {
	t.Parallel() // heavy and independent: runs beside the other heavy tests
	alice, bob := simulateClicks(300000, 0.05, 21)
	cs, err := RunCascade(alice, bob, CascadeRunConfig{CoincidenceWindowPS: 500, Seed: 5, ChunkSize: 1024, ExpectedQBER: 0.05})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Cascade: %d disclosed, %d-bit key", cs.LeakedBits, len(cs.FinalKeyBits))
	if len(cs.FinalKeyBits) == 0 || !cs.FiniteKey.Verified || cs.CRCVerify.ChunksDropped != 0 {
		t.Fatal("Cascade should certify a verified key with every chunk kept")
	}
}

// TestRunCascadeCountsErrorsFromCorrection: with nothing sacrificed, the errors correction found bound the
// phase errors; the key is longer than with a sacrificed sample, and the bound never falls below the count
func TestRunCascadeCountsErrorsFromCorrection(t *testing.T) {
	t.Parallel() // heavy and independent: runs beside the other heavy tests
	alice, bob := simulateClicks(300000, 0.05, 23)
	cfg := CascadeRunConfig{CoincidenceWindowPS: 500, Seed: 5, ChunkSize: 1024, ExpectedQBER: 0.05}
	free, err := RunCascade(alice, bob, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.SacrificeSample = true
	spent, err := RunCascade(alice, bob, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("counted from correction: %d errors in %d bits, phi %.4f, %d-bit key; sacrificed sample: %d bits, phi %.4f, %d-bit key",
		free.QBER.ErrorBits, free.QBER.SampleBits, free.FiniteKey.Phi, len(free.FinalKeyBits), spent.QBER.SampleBits, spent.FiniteKey.Phi, len(spent.FinalKeyBits))
	if !free.FiniteKey.Verified || len(free.FinalKeyBits) <= len(spent.FinalKeyBits) || free.FiniteKey.Phi < free.QBER.QBER {
		t.Fatal("counting errors from correction should give a verified, longer key with a bound above the count")
	}
	// the chain rule joining the two bases is charged only when the bits double as estimation data
	if free.FiniteKey.ChainBits < 60 || spent.FiniteKey.ChainBits != 0 {
		t.Fatalf("chain rule charged %d and %d bits", free.FiniteKey.ChainBits, spent.FiniteKey.ChainBits)
	}
}

// TestCorrectionSampleCountsDroppedChunksAsErrors: a chunk the CRC drops has unknown errors, so every bit counts
func TestCorrectionSampleCountsDroppedChunksAsErrors(t *testing.T) {
	r := rand.New(rand.NewSource(4))
	n := 4096
	rem := timedBits{Alice: randomBits(r, n), Bob: make([]byte, n), Dt: make([]int64, n), Basis: make([]Basis, n)}
	copy(rem.Bob, rem.Alice)
	rem.Bob[10] ^= 1 // one real error in the first chunk, which correction fixes
	corrected := append([]byte(nil), rem.Alice...)
	corrected[3000] ^= 1 // a residual error leaves the third chunk unverified
	s := correctionSample(rem, corrected, 1024)
	errs := 0
	for i := range s.Alice {
		if s.Alice[i] != s.Bob[i] {
			errs++
		}
	}
	if errs != 1+1024 {
		t.Fatalf("%d errors counted, want the real one plus the whole dropped chunk, 1025", errs)
	}
}

// TestCorrectionBeatsSampleAcrossSizes: with the chain rule charged in full, counting errors by correction still
// certifies a longer key than sacrificing a sample, at every block size and QBER tried
func TestCorrectionBeatsSampleAcrossSizes(t *testing.T) {
	t.Parallel() // heavy and independent: runs beside the other heavy tests
	for _, n := range []int{60_000, 200_000, 600_000} {
		for _, q := range []float64{0.02, 0.05, 0.08} {
			alice, bob := simulateClicks(n, q, int64(n)+int64(q*1000))
			cfg := CascadeRunConfig{CoincidenceWindowPS: 500, Seed: 5, ChunkSize: 1024, ExpectedQBER: q}
			free, err := RunCascade(alice, bob, cfg)
			if err != nil {
				t.Fatal(err)
			}
			cfg.SacrificeSample = true
			spent, err := RunCascade(alice, bob, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%7d sifted, QBER %.0f%%: by correction %6d bits (phi %.4f, chain rule %d bits), sacrificed sample %6d bits (phi %.4f), x%.2f",
				free.SiftedBits, 100*q, len(free.FinalKeyBits), free.FiniteKey.Phi, free.FiniteKey.ChainBits,
				len(spent.FinalKeyBits), spent.FiniteKey.Phi, float64(len(free.FinalKeyBits))/max(1, float64(len(spent.FinalKeyBits))))
			if len(free.FinalKeyBits) <= len(spent.FinalKeyBits) {
				t.Fatalf("%d clicks, QBER %.2f: correction %d bits, sample %d bits", n, q, len(free.FinalKeyBits), len(spent.FinalKeyBits))
			}
		}
	}
}
