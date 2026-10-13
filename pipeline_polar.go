package qkd_post_processing

import "fmt"

// PolarRunConfig configures RunPolar: polar SC/SCL reconciliation in place of discard/Winnow/LDPC,
// QBER from a sacrificed sample as in Winnow/LDPC (siftAndSampleQBER)
type PolarRunConfig struct {
	CoincidenceWindowPS int64
	SampleFraction      float64         // fraction of sifted bits sacrificed to measure QBER directly
	BlockLength         int             // polar codeword length N=2^n, 0 defaults to the largest power of two <= the whole remaining key
	RateMargin          float64         // how far below 1-h2(QBER) to target the code rate, comfortably not marginally, mirroring LDPC's wc/wr guidance
	ListSize            int             // SCL list size L, 0 defaults to 1 (plain successive cancellation)
	Seed                int64           // derives sample selection; must be agreed publicly (the code itself needs no secret: N, K, and QBER fix it deterministically)
	ChunkSize           int             // step 3 CRC chunk size (bits), applied after polar reconciliation
	KeyAlignBits        int             // step 5: round the final key down to a multiple of this many bits, 0 = don't
	TimingBins          int             // >0: per-block codes and per-bit LLRs from public timing (softrecon.go), 0: flat
	Asymptotic          bool            // asymptotic n(1-h2(QBER)) instead of the default finite-key bound
	FiniteKey           FiniteKeyConfig // finite-key security target, zero values default to 1e-10 / 1e-15
	ExpectedQBER        float64         // >0: choose SampleFraction by OptimalSampleFraction from this declared prior
}

// PolarRunResult mirrors LDPCRunResult; SC/SCL is single-pass, so correction stats replace
// LDPC's convergence counters (a failed block is caught by the CRC step instead)
type PolarRunResult struct {
	SiftedBits int

	QBER               QBERResult
	Timing             *TimingReport    // nil unless TimingBins > 0
	FiniteKey          *FiniteKeyReport // nil in asymptotic mode
	PolarBlocks        int
	PolarBitsCorrected int // bits that differed between Bob's original and corrected stream
	PolarLeakedBits    int
	CRCVerify          CRCVerifyResult

	LeakedBits int // total classical-channel bits: QBER sample + polar frozen-value reveals + CRCs

	FinalKeyBits  []byte
	FinalKeyBytes []byte
	Seed          []byte
}

// largestPowerOfTwoAtMost returns the largest power of two <= n, or 0 if n<1
func largestPowerOfTwoAtMost(n int) int {
	if n < 1 {
		return 0
	}
	p := 1
	for p*2 <= n {
		p *= 2
	}
	return p
}

// RunPolar is Run()'s other error-correcting sibling: sift, sacrifice a sample for QBER,
// polar-correct Bob's bits onto Alice's, CRC-verify, then privacy-amplify
func RunPolar(aliceEvents, bobEvents []DetectionEvent, cfg PolarRunConfig) (PolarRunResult, error) {
	var res PolarRunResult

	siftedBits, sample, rem, qberResult := siftSample(aliceEvents, bobEvents, cfg.CoincidenceWindowPS, cfg.SampleFraction, cfg.ExpectedQBER, cfg.FiniteKey, cfg.Seed)
	aliceRem, bobRem := rem.Alice, rem.Bob
	res.SiftedBits = siftedBits
	res.QBER = qberResult

	blockLength := cfg.BlockLength
	if blockLength <= 0 {
		blockLength = largestPowerOfTwoAtMost(len(aliceRem))
	}
	if blockLength < 2 {
		return res, fmt.Errorf("polar: nothing left to reconcile after the QBER sample")
	}
	if blockLength&(blockLength-1) != 0 {
		return res, fmt.Errorf("polar: BlockLength must be a power of two, got %d", blockLength)
	}

	// per-bit error rates: from public timing if asked, otherwise the sampled QBER for every bit;
	// either way the per-block construction treats a short last block's padding as known
	q := make([]float64, len(aliceRem))
	if cfg.TimingBins > 0 {
		var report *TimingReport
		var err error
		if q, report, err = timingRates(sample, rem, cfg.CoincidenceWindowPS, cfg.TimingBins); err != nil {
			return res, err
		}
		res.Timing = report
	} else {
		for i := range q {
			q[i] = qberResult.QBER
		}
	}
	correctedBob, totalBlocks, bitsCorrected, leaked, err := PolarReconcileSoft(aliceRem, bobRem, q, blockLength, cfg.RateMargin, PolarDecodeConfig{ListSize: cfg.ListSize})
	if err != nil {
		return res, err
	}
	return finishPolarRun(res, cfg, sample, rem, correctedBob, totalBlocks, bitsCorrected, leaked)
}

// finishPolarRun is steps 3 and 5, shared by the flat and timing-aware paths
func finishPolarRun(res PolarRunResult, cfg PolarRunConfig, sample, rem timedBits, correctedBob []byte, totalBlocks, bitsCorrected, leaked int) (PolarRunResult, error) {
	res.PolarBlocks = totalBlocks
	res.PolarBitsCorrected = bitsCorrected
	res.PolarLeakedBits = leaked

	res.CRCVerify = CRCVerify(rem.Alice, correctedBob, cfg.ChunkSize)
	res.LeakedBits = leaked + res.CRCVerify.LeakedBits

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
