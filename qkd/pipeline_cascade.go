package qkd

// CascadeRunConfig configures RunCascade; zero Cascade values take the measured defaults
type CascadeRunConfig struct {
	CoincidenceWindowPS int64
	SampleFraction      float64         // fraction of sifted bits sacrificed to measure QBER directly
	Cascade             CascadeConfig   // passes, block sizes; Seed is taken from Seed below
	Seed                int64           // derives the sample and every pass's shuffle; must be agreed publicly
	ChunkSize           int             // CRC chunk size (bits), applied after Cascade
	KeyAlignBits        int             // round the final key down to a multiple of this many bits, 0 = don't
	Asymptotic          bool            // asymptotic n(1-h2(QBER)) instead of the default finite-key bound
	FiniteKey           FiniteKeyConfig // finite-key security target, zero values default to 1e-10 / 1e-15
	ExpectedQBER        float64         // >0: choose SampleFraction by OptimalSampleFraction from this declared prior
}

// CascadeRunResult mirrors WinnowRunResult with Cascade's statistics
type CascadeRunResult struct {
	SiftedBits int
	QBER       QBERResult
	FiniteKey  *FiniteKeyReport // nil in asymptotic mode
	Cascade    CascadeStats
	CRCVerify  CRCVerifyResult
	LeakedBits int // Cascade's disclosed parities plus the CRCs

	FinalKeyBits  []byte
	FinalKeyBytes []byte
	Seed          []byte
}

// RunCascade sifts, sacrifices a public sample for the QBER, corrects Bob's remaining bits with Cascade
// planned from that QBER, CRC-verifies and privacy-amplifies
func RunCascade(aliceEvents, bobEvents []DetectionEvent, cfg CascadeRunConfig) (CascadeRunResult, error) {
	var res CascadeRunResult
	siftedBits, sample, rem, qberResult := siftSample(aliceEvents, bobEvents, cfg.CoincidenceWindowPS, cfg.SampleFraction, cfg.ExpectedQBER, cfg.FiniteKey, cfg.Seed)
	res.SiftedBits, res.QBER = siftedBits, qberResult

	cc := cfg.Cascade
	cc.Seed = cfg.Seed
	correctedBob, stats := CascadeReconcile(rem.Alice, rem.Bob, qberResult.QBER, cc)
	res.Cascade = stats

	res.CRCVerify = CRCVerify(rem.Alice, correctedBob, cfg.ChunkSize)
	res.LeakedBits = stats.Parities + res.CRCVerify.LeakedBits

	key, seed, fkr, err := distill(res.CRCVerify, sample, rem, res.LeakedBits, res.QBER.QBER, cfg.Asymptotic, cfg.FiniteKey, cfg.KeyAlignBits)
	res.FiniteKey = fkr
	if err != nil {
		return res, err
	}
	res.FinalKeyBits, res.FinalKeyBytes, res.Seed = key, packToBytes(key), seed
	return res, nil
}
