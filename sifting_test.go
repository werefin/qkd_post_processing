package qkdpostproc

import (
	"math/rand"
	"testing"
)

func TestSiftBasic(t *testing.T) {
	alice := []DetectionEvent{
		{TimestampPS: 1000, Basis: 0, Bit: 1},
		{TimestampPS: 2000, Basis: 1, Bit: 0},
		{TimestampPS: 3000, Basis: 0, Bit: 1},
	}
	bob := []DetectionEvent{
		{TimestampPS: 1010, Basis: 0, Bit: 1}, // coincides with alice[0], same basis
		{TimestampPS: 2010, Basis: 0, Bit: 0}, // basis mismatch with alice[1] --> dropped
		{TimestampPS: 3005, Basis: 0, Bit: 0}, // coincides with alice[2], same basis, different bit (that's fine, errors are expected)
	}
	got := Sift(alice, bob, 50)
	if len(got) != 2 {
		t.Fatalf("expected 2 sifted bits, got %d", len(got))
	}
	if got[0].AliceBit != 1 || got[0].BobBit != 1 {
		t.Errorf("unexpected first sifted pair: %+v", got[0])
	}
	if got[1].AliceBit != 1 || got[1].BobBit != 0 {
		t.Errorf("unexpected second sifted pair: %+v", got[1])
	}
}

func TestSiftDropsMultiPhoton(t *testing.T) {
	alice := []DetectionEvent{{TimestampPS: 1000, Basis: 0, Bit: 1, MultiPhoton: true}}
	bob := []DetectionEvent{{TimestampPS: 1000, Basis: 0, Bit: 1}}
	got := Sift(alice, bob, 50)
	if len(got) != 0 {
		t.Fatalf("expected multi-photon event to be dropped, got %d sifted bits", len(got))
	}
}

func TestSiftDropsAmbiguousMultipleCandidates(t *testing.T) {
	alice := []DetectionEvent{{TimestampPS: 1000, Basis: 0, Bit: 1}}
	bob := []DetectionEvent{
		{TimestampPS: 990, Basis: 0, Bit: 1},
		{TimestampPS: 1010, Basis: 0, Bit: 0},
	}
	got := Sift(alice, bob, 50)
	if len(got) != 0 {
		t.Fatalf("expected ambiguous pairing to be dropped, got %d sifted bits", len(got))
	}
}

func TestSiftOutsideWindow(t *testing.T) {
	alice := []DetectionEvent{{TimestampPS: 1000, Basis: 0, Bit: 1}}
	bob := []DetectionEvent{{TimestampPS: 1200, Basis: 0, Bit: 1}}
	got := Sift(alice, bob, 50)
	if len(got) != 0 {
		t.Fatalf("expected event outside coincidence window to be dropped, got %d", len(got))
	}
}

// TestMatchCoincidencesEquivalentToSift: MatchCoincidences on the public
// (bit-stripped) view finds the same pairs Sift finds on the full events
func TestMatchCoincidencesEquivalentToSift(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	n := 2000
	var alice, bob []DetectionEvent
	tcur := int64(0)
	for range n {
		tcur += 1000
		aBasis := Basis(r.Intn(2))
		bBasis := Basis(r.Intn(2))
		aBit := byte(r.Intn(2))
		bBit := aBit
		if aBasis == bBasis && r.Float64() < 0.02 {
			bBit ^= 1
		} else if aBasis != bBasis {
			bBit = byte(r.Intn(2))
		}
		aMP := r.Float64() < 0.01
		bMP := r.Float64() < 0.01
		alice = append(alice, DetectionEvent{TimestampPS: tcur, Basis: aBasis, Bit: aBit, MultiPhoton: aMP})
		bob = append(bob, DetectionEvent{TimestampPS: tcur, Basis: bBasis, Bit: bBit, MultiPhoton: bMP})
	}

	want := Sift(alice, bob, 500)
	pairs := MatchCoincidences(StripBit(alice), StripBit(bob), 500)
	if len(pairs) != len(want) {
		t.Fatalf("expected %d pairs, got %d", len(want), len(pairs))
	}
	for i, p := range pairs {
		if p.AliceIdx != want[i].AliceIdx || p.BobIdx != want[i].BobIdx {
			t.Fatalf("pair %d: expected (%d,%d), got (%d,%d)", i, want[i].AliceIdx, want[i].BobIdx, p.AliceIdx, p.BobIdx)
		}
		// neither side needs the other's bit to read its own via the pair
		if alice[p.AliceIdx].Bit != want[i].AliceBit || bob[p.BobIdx].Bit != want[i].BobBit {
			t.Fatalf("pair %d: bits read via MatchedPair don't match Sift's", i)
		}
	}
}
