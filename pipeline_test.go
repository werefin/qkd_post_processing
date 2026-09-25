package qoj_post_processing

import (
	"math/rand"
	"testing"
)

// TestCalculateQBER: QBER is erroneous bits in the faulty blocks over
// ALL bits of the sifted key, not just the discarded portion
func TestCalculateQBER(t *testing.T) {
	a := []byte{0, 1, 1, 0, 1, 1}
	b := []byte{0, 0, 1, 1, 1, 0} // 3 mismatches, but the sifted key was 20 bits
	res := CalculateQBER(a, b, 20)
	if res.SampleBits != 20 {
		t.Fatalf("expected 20 sample bits, got %d", res.SampleBits)
	}
	if res.ErrorBits != 3 {
		t.Fatalf("expected 3 error bits, got %d", res.ErrorBits)
	}
	want := 3.0 / 20.0
	if res.QBER != want {
		t.Fatalf("expected QBER %.4f, got %.4f", want, res.QBER)
	}
}

// TestCalculateQBERKnownValue checks a known reference value: a 12-bit
// sifted key with 1 erroneous bit gives QBER=8.33%
func TestCalculateQBERKnownValue(t *testing.T) {
	res := CalculateQBER([]byte{1}, []byte{0}, 12)
	want := 1.0 / 12.0
	if res.QBER != want {
		t.Fatalf("expected QBER %.4f (8.33%%), got %.4f", want, res.QBER)
	}
}

func TestCalculateQBEREmptySample(t *testing.T) {
	res := CalculateQBER(nil, nil, 0)
	if res.QBER != 0 || res.SampleBits != 0 {
		t.Fatalf("expected zero QBER/sample on empty input, got %+v", res)
	}
}

// TestRunEndToEnd checks two invariants: no bit is ever flipped
// (surviving streams match), and the key never exceeds the input length
func TestRunEndToEnd(t *testing.T) {
	r := rand.New(rand.NewSource(55))
	n := 50000
	var alice, bob []DetectionEvent
	tcur := int64(0)
	for range n {
		tcur += 1000
		aBasis := Basis(r.Intn(2))
		bBasis := Basis(r.Intn(2))
		aBit := byte(r.Intn(2))
		bBit := aBit
		if aBasis == bBasis && r.Float64() < 0.01 {
			bBit ^= 1
		} else if aBasis != bBasis {
			bBit = byte(r.Intn(2))
		}
		alice = append(alice, DetectionEvent{TimestampPS: tcur, Basis: aBasis, Bit: aBit})
		bob = append(bob, DetectionEvent{TimestampPS: tcur, Basis: bBasis, Bit: bBit})
	}

	cfg := Config{CoincidenceWindowPS: 500, GeneratorMatrix: GeneratorMatrixEx3, ChunkSize: 2048}
	res, err := Run(alice, bob, cfg)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.SiftedBits == 0 {
		t.Fatal("expected nonzero sifted bits")
	}
	for i := range res.CRCVerify.SurvivingAlice {
		if res.CRCVerify.SurvivingAlice[i] != res.CRCVerify.SurvivingBob[i] {
			t.Fatalf("error-free stream has a residual mismatch at %d", i)
		}
	}
	if len(res.FinalKeyBits) > len(res.CRCVerify.SurvivingAlice) {
		t.Fatalf("final key (%d bits) longer than error-free input (%d bits)",
			len(res.FinalKeyBits), len(res.CRCVerify.SurvivingAlice))
	}
	if len(res.FinalKeyBits) == 0 {
		t.Fatal("expected a nonzero final key at 1% QBER over 50000 attempts")
	}
}
