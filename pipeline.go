package qoj_post_processing

// Config holds the tunable parameters of the post-processing chain
type Config struct {
	CoincidenceWindowPS int64           // step 1: max |dt| between Alice/Bob clicks
	GeneratorMatrix     GeneratorMatrix // step 2: G in the block parity check P = M*G^T
	ChunkSize           int             // step 3: CRC chunk size, in bits
}

// Result bundles the outcome of every stage, so callers can inspect
// intermediate statistics (yields, leakage, QBER) as well as the final key
type Result struct {
	SiftedBits int

	BlockDetect BlockDetectResult
	CRCVerify   CRCVerifyResult
	QBER        QBERResult

	LeakedBits int // total classical-channel bits spent on parity + CRCs

	FinalKeyBits  []byte // 0/1 per byte
	FinalKeyBytes []byte // packed, MSB-first
	Seed          []byte // Toeplitz seed used (must be agreed/published too)
}

// Run executes the full chain: sifting, detection, CRC, QBER, and privacy amplification
func Run(aliceEvents, bobEvents []DetectionEvent, cfg Config) (Result, error) {
	var res Result

	sifted := Sift(aliceEvents, bobEvents, cfg.CoincidenceWindowPS)
	res.SiftedBits = len(sifted)

	aliceBits := make([]byte, len(sifted))
	bobBits := make([]byte, len(sifted))
	for i, s := range sifted {
		aliceBits[i] = s.AliceBit
		bobBits[i] = s.BobBit
	}

	res.BlockDetect = BlockErrorDetect(aliceBits, bobBits, cfg.GeneratorMatrix)
	res.QBER = CalculateQBER(res.BlockDetect.DiscardedAlice, res.BlockDetect.DiscardedBob, res.SiftedBits)
	res.CRCVerify = CRCVerify(res.BlockDetect.SurvivingAlice, res.BlockDetect.SurvivingBob, cfg.ChunkSize)
	res.LeakedBits = res.BlockDetect.LeakedBits + res.CRCVerify.LeakedBits

	key, seed, err := PrivacyAmplify(res.CRCVerify.SurvivingAlice, res.QBER.QBER, res.LeakedBits)
	if err != nil {
		return res, err
	}
	res.FinalKeyBits = key
	res.FinalKeyBytes = packToBytes(key)
	res.Seed = seed
	return res, nil
}
