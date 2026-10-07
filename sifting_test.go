package qoj_post_processing

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

// Bob-side ambiguity: two Alice clicks each see exactly one Bob candidate,
// but it's the SAME Bob click both times --> neither should survive
func TestSiftDropsBobSideAmbiguous(t *testing.T) {
	alice := []DetectionEvent{
		{TimestampPS: 980, Basis: 0, Bit: 1},
		{TimestampPS: 1020, Basis: 0, Bit: 0},
	}
	bob := []DetectionEvent{
		{TimestampPS: 1000, Basis: 0, Bit: 1},
	}
	got := Sift(alice, bob, 50)
	if len(got) != 0 {
		t.Fatalf("expected Bob-side ambiguous pairing to be dropped, got %d sifted bits: %+v", len(got), got)
	}
}

// window boundary: exactly at +/- windowPS should still count as a coincidence
func TestSiftWindowBoundaryInclusive(t *testing.T) {
	alice := []DetectionEvent{{TimestampPS: 1000, Basis: 0, Bit: 1}}
	bob := []DetectionEvent{{TimestampPS: 1050, Basis: 0, Bit: 1}} // exactly +window
	got := Sift(alice, bob, 50)
	if len(got) != 1 {
		t.Fatalf("expected event exactly at window boundary to be included, got %d", len(got))
	}
}

func TestSiftWindowJustOutside(t *testing.T) {
	alice := []DetectionEvent{{TimestampPS: 1000, Basis: 0, Bit: 1}}
	bob := []DetectionEvent{{TimestampPS: 1051, Basis: 0, Bit: 1}} // window+1
	got := Sift(alice, bob, 50)
	if len(got) != 0 {
		t.Fatalf("expected event just outside window to be dropped, got %d", len(got))
	}
}

// basis-mismatched candidate in the window must not count toward ambiguity
func TestSiftBasisMismatchDoesNotCauseAmbiguity(t *testing.T) {
	alice := []DetectionEvent{{TimestampPS: 1000, Basis: 0, Bit: 1}}
	bob := []DetectionEvent{
		{TimestampPS: 995, Basis: 1, Bit: 0},  // wrong basis, should be ignored entirely
		{TimestampPS: 1005, Basis: 0, Bit: 1}, // right basis, the real match
	}
	got := Sift(alice, bob, 50)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 sifted bit (basis-mismatched candidate should not create ambiguity), got %d", len(got))
	}
}

// bruteForceMatch is a deliberately naive O(N*M) reference: for every Alice event, scan ALL Bob events,
// collect same-basis non-multiphoton candidates within the window, keep only unambiguous (exactly 1) matches,
// then drop any Bob event claimed by more than one Alice event
func bruteForceMatch(alice, bob []PublicEvent, windowPS int64) []MatchedPair {
	type cand struct{ aIdx, bIdx int }
	var raw []cand
	bobClaims := make(map[int]int)

	for aIdx, aEv := range alice {
		if aEv.MultiPhoton {
			continue
		}
		matchIdx := -1
		count := 0
		for bIdx, bEv := range bob {
			if bEv.MultiPhoton || bEv.Basis != aEv.Basis {
				continue
			}
			diff := aEv.TimestampPS - bEv.TimestampPS
			if diff < 0 {
				diff = -diff
			}
			if diff <= windowPS {
				count++
				matchIdx = bIdx
			}
		}
		if count == 1 {
			raw = append(raw, cand{aIdx, matchIdx})
			bobClaims[matchIdx]++
		}
	}

	var out []MatchedPair
	for _, c := range raw {
		if bobClaims[c.bIdx] != 1 {
			continue
		}
		out = append(out, MatchedPair{AliceIdx: c.aIdx, BobIdx: c.bIdx})
	}
	return out
}

func pairSetEqual(t *testing.T, got, want []MatchedPair) bool {
	t.Helper()
	if len(got) != len(want) {
		return false
	}
	// order can differ (brute force isn't in Alice-time order for ties etc);
	// compare as sets of (AliceIdx,BobIdx)
	seen := make(map[[2]int]bool, len(want))
	for _, p := range want {
		seen[[2]int{p.AliceIdx, p.BobIdx}] = true
	}
	for _, p := range got {
		if !seen[[2]int{p.AliceIdx, p.BobIdx}] {
			return false
		}
		delete(seen, [2]int{p.AliceIdx, p.BobIdx})
	}
	return len(seen) == 0
}

// TestMatchCoincidencesAgainstBruteForce cross-checks the production sliding-window matcher against an independent, obviously-correct
// O(N*M) implementation across many randomized, adversarial scenarios
func TestMatchCoincidencesAgainstBruteForce(t *testing.T) {
	for seed := int64(0); seed < 200; seed++ {
		r := rand.New(rand.NewSource(seed))
		// deliberately cram timestamps into a small range so many events land within or near each other's windows
		// this is what stresses the ambiguity-detection logic, not sparse well-separated clicks
		spanPS := int64(50 + r.Intn(500))
		windowPS := int64(1 + r.Intn(60))

		var alice, bob []PublicEvent
		na := 3 + r.Intn(30)
		nb := 3 + r.Intn(30)
		for range na {
			alice = append(alice, PublicEvent{
				TimestampPS: r.Int63n(spanPS),
				Basis:       Basis(r.Intn(2)),
				MultiPhoton: r.Float64() < 0.15,
			})
		}
		for range nb {
			bob = append(bob, PublicEvent{
				TimestampPS: r.Int63n(spanPS),
				Basis:       Basis(r.Intn(2)),
				MultiPhoton: r.Float64() < 0.15,
			})
		}

		got := MatchCoincidences(alice, bob, windowPS)
		want := bruteForceMatch(alice, bob, windowPS)

		if !pairSetEqual(t, got, want) {
			t.Fatalf("seed=%d window=%d: MatchCoincidences disagrees with brute force\n"+
				"alice=%+v\nbob=%+v\ngot=%+v\nwant=%+v",
				seed, windowPS, alice, bob, got, want)
		}
	}
}

// TestMatchCoincidencesAgainstBruteForceWithTiesAndLargeTimestamps stresses two things the first fuzz pass doesn't
// many exactly-tied timestamps (sortedIndices' sort isn't guaranteed stable, so tie order is arbitrary this checks that doesn't change the result)
// and absolute timestamp values at the real scale tt_record_dual.py actually produces
func TestMatchCoincidencesAgainstBruteForceWithTiesAndLargeTimestamps(t *testing.T) {
	const largeBase = int64(2_239_056_212_082_688_000) // real recorded scale
	for seed := int64(0); seed < 100; seed++ {
		r := rand.New(rand.NewSource(1000 + seed))
		windowPS := int64(1 + r.Intn(60))
		tieValues := int64(3 + r.Intn(5)) // force lots of exact ties

		var alice, bob []PublicEvent
		na := 5 + r.Intn(25)
		nb := 5 + r.Intn(25)
		for range na {
			alice = append(alice, PublicEvent{
				TimestampPS: largeBase + r.Int63n(tieValues)*int64(windowPS/2+1),
				Basis:       Basis(r.Intn(2)),
				MultiPhoton: r.Float64() < 0.15,
			})
		}
		for range nb {
			bob = append(bob, PublicEvent{
				TimestampPS: largeBase + r.Int63n(tieValues)*int64(windowPS/2+1),
				Basis:       Basis(r.Intn(2)),
				MultiPhoton: r.Float64() < 0.15,
			})
		}

		got := MatchCoincidences(alice, bob, windowPS)
		want := bruteForceMatch(alice, bob, windowPS)

		if !pairSetEqual(t, got, want) {
			t.Fatalf("seed=%d window=%d: disagreement with ties/large timestamps\n"+
				"alice=%+v\nbob=%+v\ngot=%+v\nwant=%+v",
				seed, windowPS, alice, bob, got, want)
		}
	}
}

// TestMatchCoincidencesAgainstBruteForceExtremeRateAsymmetry: real recordings have one side clicking 60-100x faster than the other
// bobLo two-pointer sweep is only exercised meaningfully when one side vastly outnumbers the other, so that's what this checks
func TestMatchCoincidencesAgainstBruteForceExtremeRateAsymmetry(t *testing.T) {
	for seed := int64(0); seed < 60; seed++ {
		r := rand.New(rand.NewSource(5000 + seed))
		spanPS := int64(200_000 + r.Intn(800_000))
		windowPS := int64(50 + r.Intn(2000))

		fastIsAlice := r.Intn(2) == 0
		nFast := 400 + r.Intn(1600)
		nSlow := 3 + r.Intn(20)

		mk := func(n int) []PublicEvent {
			out := make([]PublicEvent, n)
			for i := range out {
				out[i] = PublicEvent{
					TimestampPS: r.Int63n(spanPS),
					Basis:       Basis(r.Intn(2)),
					MultiPhoton: r.Float64() < 0.02,
				}
			}
			return out
		}

		var alice, bob []PublicEvent
		if fastIsAlice {
			alice, bob = mk(nFast), mk(nSlow)
		} else {
			alice, bob = mk(nSlow), mk(nFast)
		}

		got := MatchCoincidences(alice, bob, windowPS)
		want := bruteForceMatch(alice, bob, windowPS)

		if !pairSetEqual(t, got, want) {
			t.Fatalf("seed=%d window=%d fastIsAlice=%v: disagreement under extreme rate asymmetry\n"+
				"len(alice)=%d len(bob)=%d\ngot=%+v\nwant=%+v",
				seed, windowPS, fastIsAlice, len(alice), len(bob), got, want)
		}
	}
}
