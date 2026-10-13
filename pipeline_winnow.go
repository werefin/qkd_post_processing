package qkd_post_processing

// WinnowRunConfig configures RunWinnow, the error-correcting alternative to Run(): step 2/3's discard-on-disagreement is replaced by Winnow
// QBER can no longer be read off discarded blocks (nothing is discarded up front), so it comes from directly comparing a random public sample of the sifted key
type WinnowRunConfig struct {
	CoincidenceWindowPS int64
	SampleFraction      float64         // fraction of sifted bits sacrificed to measure QBER directly
	WinnowBlockSizes    []int           // one Hamming block size per correction pass
	Seed                int64           // derives the sample selection and per-pass permutations; must be agreed publicly
	ChunkSize           int             // step 3 CRC chunk size (bits), applied after Winnow
	KeyAlignBits        int             // step 5: round the final key down to a multiple of this many bits, 0 = don't
	Asymptotic          bool            // asymptotic n(1-h2(QBER)) instead of the default finite-key bound
	FiniteKey           FiniteKeyConfig // finite-key security target, zero values default to 1e-10 / 1e-15
	ExpectedQBER        float64         // >0: choose SampleFraction by OptimalSampleFraction from this declared prior
}

// WinnowRunResult mirrors Result, with Winnow's correction stats standing in for BlockDetectResult
type WinnowRunResult struct {
	SiftedBits int

	QBER              QBERResult
	FiniteKey         *FiniteKeyReport // nil in asymptotic mode
	WinnowCorrections int
	WinnowLeakedBits  int
	CRCVerify         CRCVerifyResult

	LeakedBits int // total classical-channel bits: Winnow syndromes + CRCs

	FinalKeyBits  []byte
	FinalKeyBytes []byte
	Seed          []byte
}

// RunWinnow is Run()'s error-correcting sibling: sift, sacrifice a random sample to measure QBER, Winnow-correct the rest of Bob's bits onto Alice's
// CRC-verify the result (Winnow's one-error-per-block model can still miss a block that had two or more), then privacy-amplify
func RunWinnow(aliceEvents, bobEvents []DetectionEvent, cfg WinnowRunConfig) (WinnowRunResult, error) {
	var res WinnowRunResult

	// Step 4 moved ahead of correction: a random public sample gives a direct, unbiased QBER
	// estimate, unlike the post-Winnow residual rate, which Winnow just tried to zero out
	siftedBits, sample, rem, qberResult := siftSample(aliceEvents, bobEvents, cfg.CoincidenceWindowPS, cfg.SampleFraction, cfg.ExpectedQBER, cfg.FiniteKey, cfg.Seed)
	aliceRem, bobRem := rem.Alice, rem.Bob
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

	key, seed, fkr, err := distill(res.CRCVerify, sample, rem, res.LeakedBits, res.QBER.QBER, cfg.Asymptotic, cfg.FiniteKey, cfg.KeyAlignBits)
	res.FiniteKey = fkr
	if err != nil {
		return res, err
	}
	res.FinalKeyBits = key
	res.FinalKeyBytes = packToBytes(key)
	res.Seed = seed
	return res, nil
}
