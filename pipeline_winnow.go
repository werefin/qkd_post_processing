package qoj_post_processing

// WinnowRunConfig configures RunWinnow, the error-correcting alternative to Run(): step 2/3's discard-on-disagreement is replaced by Winnow
// QBER can no longer be read off discarded blocks (nothing is discarded up front), so it comes from directly comparing a random public sample of the sifted key
type WinnowRunConfig struct {
	CoincidenceWindowPS int64
	SampleFraction      float64 // fraction of sifted bits sacrificed to measure QBER directly
	WinnowBlockSizes    []int   // one Hamming block size per correction pass
	Seed                int64   // derives the sample selection and per-pass permutations; must be agreed publicly
	ChunkSize           int     // step 3 CRC chunk size (bits), applied after Winnow
}

// WinnowRunResult mirrors Result, with Winnow's correction stats standing in for BlockDetectResult
type WinnowRunResult struct {
	SiftedBits int

	QBER              QBERResult
	WinnowCorrections int
	WinnowLeakedBits  int
	CRCVerify         CRCVerifyResult

	LeakedBits int // total classical-channel bits: QBER sample + Winnow syndromes + CRCs

	FinalKeyBits  []byte
	FinalKeyBytes []byte
	Seed          []byte
}

// RunWinnow is Run()'s error-correcting sibling: sift, sacrifice a random sample to measure QBER, Winnow-correct the rest of Bob's bits onto Alice's
// CRC-verify the result (Winnow's one-error-per-block model can still miss a block that had two or more), then privacy-amplify
func RunWinnow(aliceEvents, bobEvents []DetectionEvent, cfg WinnowRunConfig) (WinnowRunResult, error) {
	var res WinnowRunResult

	// step 4 moved ahead of correction: a random public sample gives a
	// direct, unbiased QBER estimate, unlike the residual error rate after
	// Winnow, which is exactly what Winnow just tried to drive to zero
	siftedBits, aliceRem, bobRem, qberResult := siftAndSampleQBER(aliceEvents, bobEvents, cfg.CoincidenceWindowPS, cfg.SampleFraction, cfg.Seed)
	res.SiftedBits = siftedBits
	res.QBER = qberResult

	permutations := make([][]int, len(cfg.WinnowBlockSizes))
	for i := range permutations {
		permutations[i] = GeneratePermutation(len(aliceRem), cfg.Seed+1+int64(i))
	}
	correctedBob, winnowLeaked, corrections := WinnowReconcile(aliceRem, bobRem, WinnowConfig{
		BlockSizes:   cfg.WinnowBlockSizes,
		Permutations: permutations,
	})
	res.WinnowCorrections = corrections
	res.WinnowLeakedBits = winnowLeaked

	res.CRCVerify = CRCVerify(aliceRem, correctedBob, cfg.ChunkSize)
	res.LeakedBits = winnowLeaked + res.CRCVerify.LeakedBits

	key, seed, err := PrivacyAmplify(res.CRCVerify.SurvivingAlice, res.QBER.QBER, res.LeakedBits)
	if err != nil {
		return res, err
	}
	res.FinalKeyBits = key
	res.FinalKeyBytes = packToBytes(key)
	res.Seed = seed
	return res, nil
}
