package qoj_post_processing

import "fmt"

// LDPCRunConfig configures RunLDPC, replacing step 2/3's block discard or
// Winnow's multi-pass Hamming correction with one LDPC syndrome reveal per
// block, decoded by belief propagation. QBER comes from a sacrificed
// public sample, same as Winnow, since nothing is discarded up front
type LDPCRunConfig struct {
	CoincidenceWindowPS int64
	SampleFraction      float64 // fraction of sifted bits sacrificed to measure QBER directly
	BlockLength         int     // LDPC codeword length n, 0 defaults to the whole remaining key
	ColWeight           int     // Wc, tune rate 1-Wc/Wr comfortably below 1-h2(QBER), a regular code's threshold runs below the Shannon bound
	RowWeight           int     // Wr
	MaxIterations       int     // belief propagation iteration cap per block, 0 defaults to 50
	Seed                int64   // derives sample selection and matrix construction; must be agreed publicly
	ChunkSize           int     // step 3 CRC chunk size (bits), applied after LDPC
}

// LDPCRunResult mirrors Result, with LDPC's correction stats standing in
// for BlockDetectResult
type LDPCRunResult struct {
	SiftedBits int

	QBER           QBERResult
	LDPCBlocks     int
	LDPCConverged  int // blocks whose decode satisfied the syndrome before the iteration cap
	LDPCLeakedBits int
	CRCVerify      CRCVerifyResult

	LeakedBits int // total classical-channel bits: QBER sample + LDPC syndromes + CRCs

	FinalKeyBits  []byte
	FinalKeyBytes []byte
	Seed          []byte
}

// RunLDPC is Run()'s other error-correcting sibling: sift, sacrifice a
// random sample to measure QBER, LDPC-correct the rest of Bob's bits onto
// Alice's, CRC-verify (catches any block that didn't converge or converged
// to the wrong codeword), then privacy-amplify
func RunLDPC(aliceEvents, bobEvents []DetectionEvent, cfg LDPCRunConfig) (LDPCRunResult, error) {
	var res LDPCRunResult

	siftedBits, aliceRem, bobRem, qberResult := siftAndSampleQBER(aliceEvents, bobEvents, cfg.CoincidenceWindowPS, cfg.SampleFraction, cfg.Seed)
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

	correctedBob, convergedBlocks, totalBlocks, leaked := LDPCReconcile(h, aliceRem, bobRem, res.QBER.QBER, LDPCDecodeConfig{MaxIterations: cfg.MaxIterations})
	res.LDPCBlocks = totalBlocks
	res.LDPCConverged = convergedBlocks
	res.LDPCLeakedBits = leaked

	res.CRCVerify = CRCVerify(aliceRem, correctedBob, cfg.ChunkSize)
	res.LeakedBits = leaked + res.CRCVerify.LeakedBits

	key, seed, err := PrivacyAmplify(res.CRCVerify.SurvivingAlice, res.QBER.QBER, res.LeakedBits)
	if err != nil {
		return res, err
	}
	res.FinalKeyBits = key
	res.FinalKeyBytes = packToBytes(key)
	res.Seed = seed
	return res, nil
}
