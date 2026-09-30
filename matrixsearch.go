package qoj_post_processing

import "math/rand"

// SearchGeneratorMatrix hill-climbs a p x m matrix to directly maximize
// final key rate on a fixed dataset, unlike the example or BCH matrices
// only call this with proxy data, never a real session's own secret bits,
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

	current := randomMatrix()

	// table (or masks, if m/p are too large for one) for current's matrix,
	// maintained incrementally across iterations via patchBit instead of a
	// fresh buildParityTable on every evaluate --> a flipped bit only
	// touches the half of the table it actually affects, not all 2^m
	// entries, which dominated this loop's cost before this change
	table, useTable := buildParityTable(current)
	var masks parityMasks
	if !useTable {
		masks = buildParityMasks(current)
	}

	// evaluate mirrors BlockErrorDetect + the stages Run() calls after
	// sifting, just against the one fixed sifted batch and the
	// incrementally-maintained table/masks above instead of rebuilding
	// them, and skipping BlockErrorDetect's own (now-redundant) build
	evaluate := func() float64 {
		aliceParity := computeBlockParity(aliceBits, m, p, table, useTable, masks)
		bobParity := computeBlockParity(bobBits, m, p, table, useTable, masks)
		aliceRes := ReconcileBlocks(aliceBits, aliceParity, bobParity, m)
		bobRes := ReconcileBlocks(bobBits, bobParity, aliceParity, m)
		qber := CalculateQBER(aliceRes.Discarded, bobRes.Discarded, siftedBits)
		cv := CRCVerify(aliceRes.Surviving, bobRes.Surviving, chunkSize)
		l := SecureKeyLength(len(cv.SurvivingAlice), qber.QBER, aliceRes.LeakedBits+cv.LeakedBits)
		return float64(l) / float64(siftedBits)
	}

	currentFitness := evaluate()
	best = cloneMatrix(current)
	bestFitness := currentFitness

	flipRows := make([]int, 2)
	flipCols := make([]int, 2)
	for iter := range iterations {
		flips := 1 + rng.Intn(2)
		for k := range flips {
			r, c := rng.Intn(p), rng.Intn(m)
			flipRows[k], flipCols[k] = r, c
			current[r][c] ^= 1
			if useTable {
				table.patchBit(r, c)
			}
		}
		if !useTable {
			masks = buildParityMasks(current)
		}

		candidateFitness := evaluate()

		accept := candidateFitness >= currentFitness
		if !accept {
			temperature := 1 - float64(iter)/float64(iterations)
			accept = temperature > 0 && rng.Float64() < temperature*0.1
		}
		if accept {
			currentFitness = candidateFitness
			if candidateFitness > bestFitness {
				best, bestFitness = cloneMatrix(current), candidateFitness
			}
		} else {
			// revert: flipping the same bits again is a no-op on both the
			// matrix and the table (see parityTable.patchBit), undoing
			// exactly what the block above did
			for k := range flips {
				r, c := flipRows[k], flipCols[k]
				current[r][c] ^= 1
				if useTable {
					table.patchBit(r, c)
				}
			}
			if !useTable {
				masks = buildParityMasks(current)
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
