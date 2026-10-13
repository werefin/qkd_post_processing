package qkd_post_processing

import "fmt"

// LDPCRunConfig configures RunLDPC, replacing block discard or Winnow's Hamming correction
// with one LDPC syndrome reveal per block; QBER comes from a sacrificed sample, as in Winnow
type LDPCRunConfig struct {
	CoincidenceWindowPS int64
	SampleFraction      float64         // fraction of sifted bits sacrificed to measure QBER directly
	BlockLength         int             // LDPC codeword length n, 0 defaults to the whole remaining key
	ColWeight           int             // Wc, tune rate 1-Wc/Wr comfortably below 1-h2(QBER), a regular code's threshold runs below the Shannon bound
	RowWeight           int             // Wr
	MaxIterations       int             // belief propagation iteration cap per block, 0 defaults to 50
	Seed                int64           // derives sample selection and matrix construction; must be agreed publicly
	ChunkSize           int             // step 3 CRC chunk size (bits), applied after LDPC
	KeyAlignBits        int             // step 5: round the final key down to a multiple of this many bits, 0 = don't
	TimingBins          int             // >0: per-bit LLRs from public timing (softrecon.go), 0: one flat QBER LLR
	Asymptotic          bool            // asymptotic n(1-h2(QBER)) instead of the default finite-key bound
	FiniteKey           FiniteKeyConfig // finite-key security target, zero values default to 1e-10 / 1e-15
	ExpectedQBER        float64         // >0: choose SampleFraction by OptimalSampleFraction from this declared prior
}

// LDPCRunResult mirrors Result, with LDPC's correction stats standing in
// for BlockDetectResult
type LDPCRunResult struct {
	SiftedBits int

	QBER           QBERResult
	Timing         *TimingReport    // nil unless TimingBins > 0
	FiniteKey      *FiniteKeyReport // nil in asymptotic mode
	LDPCBlocks     int
	LDPCConverged  int // blocks whose decode satisfied the syndrome before the iteration cap
	LDPCLeakedBits int
	CRCVerify      CRCVerifyResult

	LeakedBits int // total classical-channel bits: QBER sample + LDPC syndromes + CRCs

	FinalKeyBits  []byte
	FinalKeyBytes []byte
	Seed          []byte
}

// RunLDPC is Run()'s other error-correcting sibling: sift, sacrifice a sample for QBER,
// LDPC-correct Bob's bits onto Alice's, CRC-verify, then privacy-amplify
func RunLDPC(aliceEvents, bobEvents []DetectionEvent, cfg LDPCRunConfig) (LDPCRunResult, error) {
	var res LDPCRunResult

	siftedBits, sample, rem, qberResult := siftSample(aliceEvents, bobEvents, cfg.CoincidenceWindowPS, cfg.SampleFraction, cfg.ExpectedQBER, cfg.FiniteKey, cfg.Seed)
	aliceRem, bobRem := rem.Alice, rem.Bob
	res.SiftedBits = siftedBits
	res.QBER = qberResult

	blockLength := cfg.BlockLength
	if blockLength <= 0 {
		blockLength = len(aliceRem) // recommended default: one block covers the whole remaining key
	}
	if blockLength <= 0 {
		return res, fmt.Errorf("ldpc: nothing left to reconcile after the QBER sample")
	}

	h, err := BuildRegularLDPC(blockLength, cfg.ColWeight, cfg.RowWeight, cfg.Seed+1)
	if err != nil {
		return res, err
	}

	decodeCfg := LDPCDecodeConfig{MaxIterations: cfg.MaxIterations}
	var correctedBob []byte
	var convergedBlocks, totalBlocks, leaked int
	if cfg.TimingBins > 0 {
		q, report, terr := timingRates(sample, rem, cfg.CoincidenceWindowPS, cfg.TimingBins)
		if terr != nil {
			return res, terr
		}
		res.Timing = report
		correctedBob, convergedBlocks, totalBlocks, leaked = LDPCReconcileSoft(h, aliceRem, bobRem, q, decodeCfg)
	} else {
		correctedBob, convergedBlocks, totalBlocks, leaked = LDPCReconcile(h, aliceRem, bobRem, res.QBER.QBER, decodeCfg)
	}
	res.LDPCBlocks = totalBlocks
	res.LDPCConverged = convergedBlocks
	res.LDPCLeakedBits = leaked

	res.CRCVerify = CRCVerify(aliceRem, correctedBob, cfg.ChunkSize)
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
