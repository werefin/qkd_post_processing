package qkd

import (
	"math/rand"
	"testing"
)

// TestUniversalHashTagBitsMatchesReviewTarget: epsCor=1e-15 needs ceil(log2(1e15)) = 50 bits, within the
// review's "64- to 128-bit" suggestion as a safe, round superset
func TestUniversalHashTagBitsMatchesReviewTarget(t *testing.T) {
	got := UniversalHashTagBits(1e-15)
	if got != 50 {
		t.Fatalf("UniversalHashTagBits(1e-15) = %d, want 50", got)
	}
}

// TestUniversalHashTagBitsRejectsInvalidEpsilon
func TestUniversalHashTagBitsRejectsInvalidEpsilon(t *testing.T) {
	for _, eps := range []float64{0, -1, 1, 2} {
		if got := UniversalHashTagBits(eps); got != 0 {
			t.Fatalf("UniversalHashTagBits(%v) = %d, want 0", eps, got)
		}
	}
}

// TestVerifyUniversalHashAcceptsEqualStrings: identical bits under the same
// seed must always verify, regardless of seed
func TestVerifyUniversalHashAcceptsEqualStrings(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for trial := 0; trial < 50; trial++ {
		n := 100 + r.Intn(5000)
		bits := make([]byte, n)
		for i := range bits {
			bits[i] = byte(r.Intn(2))
		}
		tagBits := 50
		seed, err := UniversalHashSeed(tagBits, n)
		if err != nil {
			t.Fatal(err)
		}
		tag := ComputeUniversalHashTag(bits, seed, tagBits)
		if !VerifyUniversalHash(bits, tag, seed, tagBits) {
			t.Fatalf("trial %d: identical bits failed to verify", trial)
		}
	}
}

// TestVerifyUniversalHashRejectsDifferingStringsWithHighProbability: flipping even a single bit must be
// caught the overwhelming majority of the time across independent random seeds
func TestVerifyUniversalHashRejectsDifferingStringsWithHighProbability(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	const tagBits = 16 // small on purpose: makes the rare-collision case reachable in a feasible number of trials
	const trials = 20000
	n := 200

	falseAccepts := 0
	for trial := 0; trial < trials; trial++ {
		bitsA := make([]byte, n)
		for i := range bitsA {
			bitsA[i] = byte(r.Intn(2))
		}
		bitsB := append([]byte(nil), bitsA...)
		flip := r.Intn(n)
		bitsB[flip] ^= 1 // exactly one bit different

		seed, err := UniversalHashSeed(tagBits, n)
		if err != nil {
			t.Fatal(err)
		}
		tagB := ComputeUniversalHashTag(bitsB, seed, tagBits)
		if VerifyUniversalHash(bitsA, tagB, seed, tagBits) {
			falseAccepts++
		}
	}

	rate := float64(falseAccepts) / float64(trials)
	target := 1.0 / float64(int(1)<<tagBits)
	// allow a wide statistical margin (this is a Monte Carlo check of a
	// 2^-16 event over 20000 trials, not a tight confidence interval)
	if rate > target*5 {
		t.Fatalf("false-accept rate %v far exceeds the 2^-%d bound (%v) -- collision resistance may be broken", rate, tagBits, target)
	}
	t.Logf("false-accept rate over %d trials: %v (bound: %v)", trials, rate, target)
}

// TestVerifyUniversalHashRejectsWrongLengthTag
func TestVerifyUniversalHashRejectsWrongLengthTag(t *testing.T) {
	bits := []byte{0, 1, 1, 0, 1}
	seed, err := UniversalHashSeed(10, len(bits))
	if err != nil {
		t.Fatal(err)
	}
	if VerifyUniversalHash(bits, []byte{0, 1, 0}, seed, 10) {
		t.Fatal("expected a length mismatch to fail verification, not panic or false-accept")
	}
}
