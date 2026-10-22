package qkd

// timedBits is a set of sifted bit pairs with their public offsets and bases, index-aligned
type timedBits struct {
	Alice, Bob []byte
	Dt         []int64
	Basis      []Basis
}

// siftSample sifts, then splits the sifted bits by a public permutation into a sacrificed sample and the rest;
// given expectedQBER > 0 the sample share comes from OptimalSampleFraction, before any sample bit is looked at
func siftSample(aliceEvents, bobEvents []DetectionEvent, windowPS int64, sampleFraction, expectedQBER float64, fk FiniteKeyConfig, seed int64) (siftedBits int, sample, rem timedBits, qber QBERResult) {
	sifted := Sift(aliceEvents, bobEvents, windowPS)
	if expectedQBER > 0 {
		sampleFraction = OptimalSampleFraction(len(sifted), expectedQBER, fk)
	}
	order := siftSampleOrder(len(sifted), seed)
	sampleN := max(0, min(int(float64(len(sifted))*sampleFraction), len(sifted)))

	pick := func(idxs []int) timedBits {
		t := timedBits{Alice: make([]byte, len(idxs)), Bob: make([]byte, len(idxs)), Dt: make([]int64, len(idxs)), Basis: make([]Basis, len(idxs))}
		for i, idx := range idxs {
			s := sifted[idx]
			t.Alice[i], t.Bob[i] = s.AliceBit, s.BobBit
			t.Basis[i] = aliceEvents[s.AliceIdx].Basis
			t.Dt[i] = bobEvents[s.BobIdx].TimestampPS - aliceEvents[s.AliceIdx].TimestampPS
		}
		return t
	}
	sample, rem = pick(order[:sampleN]), pick(order[sampleN:])

	errBits := 0
	for i := range sample.Alice {
		if sample.Alice[i] != sample.Bob[i] {
			errBits++
		}
	}
	rate := 0.0
	if sampleN > 0 {
		rate = float64(errBits) / float64(sampleN)
	}
	return len(sifted), sample, rem, QBERResult{ErrorBits: errBits, SampleBits: sampleN, QBER: rate}
}
