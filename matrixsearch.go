package qoj_post_processing

import "math/rand"

// SearchGeneratorMatrix hill-climbs a p x m matrix to directly maximize
// final key rate on a fixed dataset, unlike the example or BCH matrices
//
// Only call this with proxy data, never a real session's own secret bits,
// or the matrix choice itself leaks what PrivacyAmplify never charges for
func SearchGeneratorMatrix(m, p int, aliceEvents, bobEvents []DetectionEvent, windowPS int64, chunkSize, iterations int, seed int64) (best GeneratorMatrix, bestKeyRate float64) {
	sifted := Sift(aliceEvents, bobEvents, windowPS)
	aliceBits := make([]byte, len(sifted))
	bobBits := make([]byte, len(sifted))
	for i, s := range sifted {
		aliceBits[i] = s.AliceBit
		bobBits[i] = s.BobBit
	}
	siftedBits := len(sifted)
	if siftedBits == 0 {
		return nil, 0
	}

	rng := rand.New(rand.NewSource(seed))

	randomMatrix := func() GeneratorMatrix {
		g := make(GeneratorMatrix, p)
		for i := range g {
			row := make([]byte, m)
			for j := range row {
				row[j] = byte(rng.Intn(2))
			}
			g[i] = row
		}
		return g
	}

	// evaluate reuses the exact same stages Run() calls after sifting, just
	// against the one fixed sifted batch instead of re-sifting every call
	evaluate := func(g GeneratorMatrix) float64 {
		bd := BlockErrorDetect(aliceBits, bobBits, g)
		qber := CalculateQBER(bd.DiscardedAlice, bd.DiscardedBob, siftedBits)
		cv := CRCVerify(bd.SurvivingAlice, bd.SurvivingBob, chunkSize)
		key, _, err := PrivacyAmplify(cv.SurvivingAlice, qber.QBER, bd.LeakedBits+cv.LeakedBits, 0)
		if err != nil {
			return 0
		}
		return float64(len(key)) / float64(siftedBits)
	}

	current := randomMatrix()
	currentFitness := evaluate(current)
	best = cloneMatrix(current)
	bestFitness := currentFitness

	for iter := range iterations {
		candidate := cloneMatrix(current)
		flips := 1 + rng.Intn(2)
		for range flips {
			candidate[rng.Intn(p)][rng.Intn(m)] ^= 1
		}
		candidateFitness := evaluate(candidate)

		accept := candidateFitness >= currentFitness
		if !accept {
			temperature := 1 - float64(iter)/float64(iterations)
			accept = temperature > 0 && rng.Float64() < temperature*0.1
		}
		if accept {
			current, currentFitness = candidate, candidateFitness
			if candidateFitness > bestFitness {
				best, bestFitness = cloneMatrix(candidate), candidateFitness
			}
		}
	}

	return best, bestFitness
}

func cloneMatrix(g GeneratorMatrix) GeneratorMatrix {
	out := make(GeneratorMatrix, len(g))
	for i, row := range g {
		out[i] = append([]byte(nil), row...)
	}
	return out
}
