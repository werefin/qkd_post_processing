package qoj_post_processing

import "math/rand"

// QBERResult holds the outcome of step 4
type QBERResult struct {
	ErrorBits  int
	SampleBits int
	QBER       float64
}

// siftAndSampleQBER sifts, then sacrifices a random public sample to
// measure QBER directly, shared by RunWinnow and RunLDPC since neither
// discards blocks up front the way Run() does
func siftAndSampleQBER(aliceEvents, bobEvents []DetectionEvent, windowPS int64, sampleFraction float64, seed int64) (siftedBits int, aliceRem, bobRem []byte, qber QBERResult) {
	sifted := Sift(aliceEvents, bobEvents, windowPS)
	siftedBits = len(sifted)

	aliceBits := make([]byte, siftedBits)
	bobBits := make([]byte, siftedBits)
	for i, s := range sifted {
		aliceBits[i] = s.AliceBit
		bobBits[i] = s.BobBit
	}

	order := rand.New(rand.NewSource(seed)).Perm(siftedBits)
	sampleN := max(0, min(int(float64(siftedBits)*sampleFraction), siftedBits))

	errBits := 0
	for _, idx := range order[:sampleN] {
		if aliceBits[idx] != bobBits[idx] {
			errBits++
		}
	}
	rate := 0.0
	if sampleN > 0 {
		rate = float64(errBits) / float64(sampleN)
	}
	qber = QBERResult{ErrorBits: errBits, SampleBits: sampleN, QBER: rate}

	remaining := order[sampleN:]
	aliceRem = make([]byte, len(remaining))
	bobRem = make([]byte, len(remaining))
	for i, idx := range remaining {
		aliceRem[i] = aliceBits[idx]
		bobRem[i] = bobBits[idx]
	}
	return siftedBits, aliceRem, bobRem, qber
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
