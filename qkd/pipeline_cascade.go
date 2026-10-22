package qkd

// CascadeRunConfig configures RunCascade; zero Cascade values take the measured defaults
type CascadeRunConfig struct {
	CoincidenceWindowPS int64
	SampleFraction      float64         // with SacrificeSample: fraction of sifted bits sacrificed for the QBER
	Cascade             CascadeConfig   // passes, block sizes; Seed is taken from Seed below
	Seed                int64           // derives the sample and every pass's shuffle; must be agreed publicly
	ChunkSize           int             // CRC chunk size (bits), applied after Cascade
	KeyAlignBits        int             // round the final key down to a multiple of this many bits, 0 = don't
	Asymptotic          bool            // asymptotic n(1-h2(QBER)) instead of the default finite-key bound
	FiniteKey           FiniteKeyConfig // finite-key security target, zero values default to 1e-10 / 1e-15
	ExpectedQBER        float64         // the QBER Cascade plans with, from public timing; 0 takes 0.05
	// SacrificeSample spends a random share of the sifted bits on the QBER; by default
	// nothing is spent and each basis's errors, counted exactly after correction, bound the other's phase errors
	SacrificeSample bool
}

// CascadeRunResult is one block: the sifted count, the QBER, Cascade's statistics, verification and the key
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

// RunCascade sifts, corrects Bob's bits with Cascade, CRC-verifies and privacy-amplifies; the QBER and phase
// errors come from the errors correction found, or with SacrificeSample from a sacrificed sample
func RunCascade(aliceEvents, bobEvents []DetectionEvent, cfg CascadeRunConfig) (CascadeRunResult, error) {
	var res CascadeRunResult
	fraction, prior := cfg.SampleFraction, cfg.ExpectedQBER
	if !cfg.SacrificeSample {
		fraction, prior = 0, 0
	}
	siftedBits, sample, rem, qberResult := siftSample(aliceEvents, bobEvents, cfg.CoincidenceWindowPS, fraction, prior, cfg.FiniteKey, cfg.Seed)
	res.SiftedBits, res.QBER = siftedBits, qberResult

	plan := qberResult.QBER
	if !cfg.SacrificeSample {
		plan = cfg.ExpectedQBER
		if plan <= 0 {
			plan = 0.05
		}
	}
	cc := cfg.Cascade
	cc.Seed = cfg.Seed
	correctedBob, stats := CascadeReconcile(rem.Alice, rem.Bob, plan, cc)
	res.Cascade = stats

	res.CRCVerify = CRCVerify(rem.Alice, correctedBob, cfg.ChunkSize)
	res.LeakedBits = stats.Parities + res.CRCVerify.LeakedBits
	if !cfg.SacrificeSample && len(rem.Alice) > 0 {
		// every key bit is in the sample: its error is known once correction has verified its chunk
		sample = correctionSample(rem, correctedBob, cfg.ChunkSize)
		errs := 0
		for i := range sample.Alice {
			if sample.Alice[i] != sample.Bob[i] {
				errs++
			}
		}
		res.QBER = QBERResult{ErrorBits: errs, SampleBits: len(sample.Alice), QBER: float64(errs) / float64(len(sample.Alice))}
	}

	fk := cfg.FiniteKey
	fk.byCorrection = !cfg.SacrificeSample
	key, seed, fkr, err := distill(res.CRCVerify, sample, rem, res.LeakedBits, res.QBER.QBER, cfg.Asymptotic, fk, cfg.KeyAlignBits)
	res.FiniteKey = fkr
	if err != nil {
		return res, err
	}
	res.FinalKeyBits, res.FinalKeyBytes, res.Seed = key, packToBytes(key), seed
	return res, nil
}
