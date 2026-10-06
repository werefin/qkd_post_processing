package qoj_post_processing

import (
	"math/rand"
	"testing"
)

// buildEpochStream builds a stream alternating good/bad quality by time,
// the shape of a real link whose polarization alignment drifts in and out
func buildEpochStream(r *rand.Rand, epochPS int64, numEpochs int) (alice, bob []DetectionEvent) {
	tcur := int64(0)
	totalElapsed := epochPS * int64(numEpochs)
	for tcur < totalElapsed {
		tcur += 1000
		goodEpoch := (tcur/epochPS)%2 == 0
		errRate := 0.01
		if !goodEpoch {
			errRate = 0.45
		}
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

// TestRunEpochedRecoversKeyFromMixedQualityLink checks that epoching a
// pooled-zero-key recording into short, independently-run sessions recovers
// a non-zero key from its genuinely-good epochs
func TestRunEpochedRecoversKeyFromMixedQualityLink(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	const epochPS = 5_000_000
	alice, bob := buildEpochStream(r, epochPS, 10)

	cfg := Config{CoincidenceWindowPS: 500, GeneratorMatrix: testMatrixC, ChunkSize: 256}

	pooled, err := Run(alice, bob, cfg)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(pooled.FinalKeyBits) != 0 {
		t.Fatalf("expected the pooled, unepoched run over this mixed-quality link to fail to produce a key, got %d bits", len(pooled.FinalKeyBits))
	}

	epoched, err := RunEpoched(alice, bob, cfg, epochPS)
	if err != nil {
		t.Fatalf("RunEpoched returned error: %v", err)
	}
	if len(epoched.FinalKeyBits) == 0 {
		t.Fatal("expected epoching to recover a non-zero key from the good epochs")
	}

	goodEpochsWithKey, badEpochsWithKey := 0, 0
	for _, e := range epoched.Epochs {
		isGood := (e.StartPS/epochPS)%2 == 0
		if len(e.FinalKeyBits) > 0 {
			if isGood {
				goodEpochsWithKey++
			} else {
				badEpochsWithKey++
			}
		}
	}
	if goodEpochsWithKey == 0 {
		t.Fatal("expected at least one good epoch to yield key bits")
	}
	if badEpochsWithKey != 0 {
		t.Fatalf("expected zero bad (QBER about 45%%) epochs to yield key bits, got %d", badEpochsWithKey)
	}
}

// TestRunEpochedUniformlyBadLinkYieldsNoKey checks epoching a link that's
// bad everywhere still correctly yields nothing: epoching recovers key
// material from genuinely good moments, it doesn't manufacture any
func TestRunEpochedUniformlyBadLinkYieldsNoKey(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	const epochPS = 5_000_000

	var alice, bob []DetectionEvent
	tcur := int64(0)
	for tcur < epochPS*10 {
		tcur += 1000
		bit := byte(r.Intn(2))
		otherBit := bit
		if r.Float64() < 0.20 {
			otherBit ^= 1
		}
		alice = append(alice, DetectionEvent{TimestampPS: tcur, Basis: 0, Bit: bit})
		bob = append(bob, DetectionEvent{TimestampPS: tcur, Basis: 0, Bit: otherBit})
	}

	cfg := Config{CoincidenceWindowPS: 500, GeneratorMatrix: testMatrixC, ChunkSize: 256}
	epoched, err := RunEpoched(alice, bob, cfg, epochPS)
	if err != nil {
		t.Fatalf("RunEpoched returned error: %v", err)
	}
	if len(epoched.FinalKeyBits) != 0 {
		t.Fatalf("a uniformly 20%% QBER link is above the security threshold everywhere; expected zero key bits, got %d", len(epoched.FinalKeyBits))
	}
}

// TestRunEpochedKeyAlignAppliesOnceToTheConcatenation checks that
// KeyAlignBits rounds the total across epochs, not each epoch individually
// (which would zero out any epoch shorter than the alignment)
func TestRunEpochedKeyAlignAppliesOnceToTheConcatenation(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	const epochPS = 5_000_000
	alice, bob := buildEpochStream(r, epochPS, 10)

	cfg := Config{CoincidenceWindowPS: 500, GeneratorMatrix: testMatrixC, ChunkSize: 256, KeyAlignBits: 32}
	epoched, err := RunEpoched(alice, bob, cfg, epochPS)
	if err != nil {
		t.Fatalf("RunEpoched returned error: %v", err)
	}
	if len(epoched.FinalKeyBits)%32 != 0 {
		t.Fatalf("expected the concatenated key to be a multiple of KeyAlignBits=32, got %d bits", len(epoched.FinalKeyBits))
	}
	if len(epoched.FinalKeyBits) == 0 {
		t.Fatal("expected a non-zero aligned key; a per-epoch alignment bug would zero every short epoch out")
	}
}
