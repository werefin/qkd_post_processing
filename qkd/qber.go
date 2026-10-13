package qkd

import "math/rand"

// QBERResult holds the outcome of step 4
type QBERResult struct {
	ErrorBits  int
	SampleBits int
	QBER       float64
}

// siftAndSampleQBER sifts, then sacrifices a random sample to measure QBER directly;
// shared by RunWinnow and RunLDPC since neither discards blocks up front like Run()
func siftAndSampleQBER(aliceEvents, bobEvents []DetectionEvent, windowPS int64, sampleFraction float64, seed int64) (siftedBits int, aliceRem, bobRem []byte, qber QBERResult) {
	siftedBits, _, rem, qber := siftSampleWithTiming(aliceEvents, bobEvents, windowPS, sampleFraction, seed)
	return siftedBits, rem.Alice, rem.Bob, qber
}

// siftSampleOrder is the public random permutation choosing which sifted bits are sacrificed
func siftSampleOrder(n int, seed int64) []int {
	return rand.New(rand.NewSource(seed)).Perm(n)
}

// CalculateQBER: bad bits in the discarded blocks over all bits of the
// sifted key, faulty and non-faulty blocks combined
func CalculateQBER(discardedAliceBlocks, discardedBobBlocks []byte, siftedBits int) QBERResult {
	errBits := 0
	n := min(len(discardedAliceBlocks), len(discardedBobBlocks))
	for i := range n {
		if discardedAliceBlocks[i] != discardedBobBlocks[i] {
			errBits++
		}
	}
	var q float64
	if siftedBits > 0 {
		q = float64(errBits) / float64(siftedBits)
	}
	return QBERResult{ErrorBits: errBits, SampleBits: siftedBits, QBER: q}
}
