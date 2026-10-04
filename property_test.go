package qoj_post_processing

import (
	"bytes"
	"math/rand"
	"testing"
)

// simulateAt generates one seeded, correlated Alice/Bob click stream at a
// target intrinsic error rate, shared by TestPropertySweep below
func simulateAt(n int, errRate float64, seed int64) (alice, bob []DetectionEvent) {
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

// TestPropertySweep checks the two invariants that must hold everywhere in
// the (QBER, generator matrix, chunk size) space, not just at the one fixed
// point each individual unit test happens to use
func TestPropertySweep(t *testing.T) {
	errRates := []float64{0.0, 0.005, 0.01, 0.02, 0.05, 0.08, 0.11}
	matrices := []GeneratorMatrix{testMatrixA, testMatrixC}
	chunkSizes := []int{256, 2048}
	seeds := []int64{1, 2, 3}

	const n = 20000
	trials := 0
	nonEmptyKeys := 0

	for _, errRate := range errRates {
		for _, g := range matrices {
			for _, chunkSize := range chunkSizes {
				for _, seed := range seeds {
					trials++
					alice, bob := simulateAt(n, errRate, seed)
					cfg := Config{CoincidenceWindowPS: 500, GeneratorMatrix: g, ChunkSize: chunkSize}
					res, err := Run(alice, bob, cfg)
					if err != nil {
						t.Fatalf("err=%.3f g=block%d chunk=%d seed=%d: Run returned error: %v",
							errRate, g.BlockSize(), chunkSize, seed, err)
					}
					if !bytes.Equal(res.CRCVerify.SurvivingAlice, res.CRCVerify.SurvivingBob) {
						t.Fatalf("err=%.3f g=block%d chunk=%d seed=%d: error-free streams differ",
							errRate, g.BlockSize(), chunkSize, seed)
					}
					if len(res.FinalKeyBits) > len(res.CRCVerify.SurvivingAlice) {
						t.Fatalf("err=%.3f g=block%d chunk=%d seed=%d: final key (%d bits) longer than input (%d bits)",
							errRate, g.BlockSize(), chunkSize, seed, len(res.FinalKeyBits), len(res.CRCVerify.SurvivingAlice))
					}
					if len(res.FinalKeyBytes) != (len(res.FinalKeyBits)+7)/8 {
						t.Fatalf("err=%.3f g=block%d chunk=%d seed=%d: packed key length %d doesn't match bit length %d",
							errRate, g.BlockSize(), chunkSize, seed, len(res.FinalKeyBytes), len(res.FinalKeyBits))
					}
					if len(res.FinalKeyBits) > 0 {
						nonEmptyKeys++
					}
				}
			}
		}
	}

	if trials == 0 {
		t.Fatal("swept zero parameter combinations")
	}
	// Low QBER should reliably produce a key somewhere in the grid; this
	// isn't a strict per-point guarantee (small-n variance exists), just a
	// sanity check that the sweep isn't silently producing nothing but empty keys
	if nonEmptyKeys == 0 {
		t.Fatal("every single combination in the sweep produced an empty key --> suspicious")
	}
	t.Logf("swept %d combinations, %d produced a nonzero key", trials, nonEmptyKeys)
}
