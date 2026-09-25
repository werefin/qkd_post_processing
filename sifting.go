package qoj_post_processing

import (
	"cmp"
	"slices"
)

// SiftedBit is one surviving, basis-matched, unambiguous coincidence
type SiftedBit struct {
	AliceBit byte
	BobBit   byte
	AliceIdx int // index into the original Alice event slice
	BobIdx   int // index into the original Bob event slice
}

// PublicEvent is the classically-exchangeable part of a detection event
// timestamp, basis, and multi-photon flag, never the measured bit
type PublicEvent struct {
	TimestampPS int64
	Basis       Basis
	MultiPhoton bool
}

// StripBit converts DetectionEvents to the PublicEvent view safe to
// publish over the classical channel for coincidence matching
func StripBit(events []DetectionEvent) []PublicEvent {
	out := make([]PublicEvent, len(events))
	for i, e := range events {
		out[i] = PublicEvent{TimestampPS: e.TimestampPS, Basis: e.Basis, MultiPhoton: e.MultiPhoton}
	}
	return out
}

// MatchedPair is one coincidence found by MatchCoincidences: indices into
// the original event slices, never the secret bit values
type MatchedPair struct {
	AliceIdx int
	BobIdx   int
}

// MatchCoincidences finds basis-matched, unambiguous coincidences from
// public metadata alone --> both sides get the same pairs, no bits involved
func MatchCoincidences(alice, bob []PublicEvent, windowPS int64) []MatchedPair {
	aliceOrder := sortedIndices(alice)
	bobOrder := sortedIndices(bob)

	bobLo := 0
	// bPos is a dense index into bobOrder, so a slice beats a map here
	// no hashing, no bucket probe, just an array read
	bobCandidateCount := make([]int32, len(bobOrder))

	type match struct{ aPos, bPos int32 }
	matches := make([]match, 0, len(aliceOrder))

	for aPos, aIdx := range aliceOrder {
		aEv := alice[aIdx]
		if aEv.MultiPhoton {
			continue
		}
		for bobLo < len(bobOrder) && bob[bobOrder[bobLo]].TimestampPS < aEv.TimestampPS-windowPS {
			bobLo++
		}
		count := 0
		lastBPos := int32(-1)
		for bPos := bobLo; bPos < len(bobOrder); bPos++ {
			bIdx := bobOrder[bPos]
			bEv := bob[bIdx]
			if bEv.TimestampPS > aEv.TimestampPS+windowPS {
				break
			}
			if bEv.MultiPhoton || bEv.Basis != aEv.Basis {
				continue
			}
			count++
			lastBPos = int32(bPos)
		}
		if count == 1 {
			matches = append(matches, match{int32(aPos), lastBPos})
			bobCandidateCount[lastBPos]++
		}
	}

	pairs := make([]MatchedPair, 0, len(matches))
	for _, m := range matches {
		if bobCandidateCount[m.bPos] != 1 {
			continue // bob side was ambiguous too, drop
		}
		pairs = append(pairs, MatchedPair{
			AliceIdx: int(aliceOrder[m.aPos]),
			BobIdx:   int(bobOrder[m.bPos]),
		})
	}
	return pairs
}

// Sift is a convenience wrapper for simulation and testing
// use StripBit + MatchCoincidences directly to run each side apart
func Sift(aliceEvents, bobEvents []DetectionEvent, windowPS int64) []SiftedBit {
	pairs := MatchCoincidences(StripBit(aliceEvents), StripBit(bobEvents), windowPS)
	sifted := make([]SiftedBit, len(pairs))
	for i, p := range pairs {
		sifted[i] = SiftedBit{
			AliceBit: aliceEvents[p.AliceIdx].Bit,
			BobBit:   bobEvents[p.BobIdx].Bit,
			AliceIdx: p.AliceIdx,
			BobIdx:   p.BobIdx,
		}
	}
	return sifted
}

type tsIdx struct {
	ts  int64
	idx int32
}

// sortedIndices sorts contiguous {timestamp,idx} pairs directly, not an
// index array through sort.Slice's reflect-based closure comparator
func sortedIndices(events []PublicEvent) []int32 {
	pairs := make([]tsIdx, len(events))
	for i := range events {
		pairs[i] = tsIdx{events[i].TimestampPS, int32(i)}
	}
	slices.SortFunc(pairs, func(a, b tsIdx) int {
		return cmp.Compare(a.ts, b.ts)
	})
	idx := make([]int32, len(pairs))
	for i, p := range pairs {
		idx[i] = p.idx
	}
	return idx
}
