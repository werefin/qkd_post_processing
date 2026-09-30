// Command qoj_post_processing runs the BBM92 post-processing chain end to end
// over simulated click streams, or embed qkd.Run as a library call instead
package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	qkd "qoj_post_processing"
)

// jsonReport is -json's machine-readable stand-in for the human-readable
// report, one object printed to stdout, for driving e.g. a dashboard
type jsonReport struct {
	Method            string  `json:"method"` // discard, winnow, or ldpc
	RawAttempts       int     `json:"raw_attempts"`
	ErrRate           float64 `json:"err_rate"`
	MatrixLabel       string  `json:"matrix_label,omitempty"`
	SearchFitness     float64 `json:"search_fitness,omitempty"`
	SiftedBits        int     `json:"sifted_bits"`
	BlocksTotal       int     `json:"blocks_total,omitempty"`
	BlocksKept        int     `json:"blocks_kept,omitempty"`
	BlocksDropped     int     `json:"blocks_dropped,omitempty"`
	BitsLeakedParity  int     `json:"bits_leaked_parity,omitempty"`
	WinnowCorrections int     `json:"winnow_corrections,omitempty"`
	WinnowLeakedBits  int     `json:"winnow_leaked_bits,omitempty"`
	LDPCBlocks        int     `json:"ldpc_blocks,omitempty"`
	LDPCConverged     int     `json:"ldpc_converged,omitempty"`
	LDPCLeakedBits    int     `json:"ldpc_leaked_bits,omitempty"`
	ChunksTotal       int     `json:"chunks_total"`
	ChunksKept        int     `json:"chunks_kept"`
	ChunksDropped     int     `json:"chunks_dropped"`
	BitsLeakedCRC     int     `json:"bits_leaked_crc"`
	QBER              float64 `json:"qber"`
	LeakedBitsTotal   int     `json:"leaked_bits_total"`
	FinalKeyBits      int     `json:"final_key_bits"`
	CompressionRate   float64 `json:"compression_rate"`
	KeysSaved         int     `json:"keys_saved"`
	ElapsedMS         int64   `json:"elapsed_ms"`
	Error             string  `json:"error,omitempty"`
}

// generatorMatrices maps -matrix flag values to the fixed step 2
// generator matrices
var generatorMatrices = map[string]qkd.GeneratorMatrix{
	"bch": qkd.GeneratorMatrixBCHt2,
}

// ANSI color codes for status tags, arnika-style: raw escapes inlined at the
// call site rather than pulled in via a color library
const (
	colorReset = "\033[0m"
	colorOK    = "\033[32m"
	colorWarn  = "\033[33m"
	colorErr   = "\033[31m"
)

const labelWidth = 32

// section prints a plain title line ahead of a group of fields
func section(title string) {
	fmt.Printf("\n%s\n", title)
}

// field prints one label:value row with the label padded to a fixed column,
// so every section lines up regardless of label length
func field(label, format string, args ...any) {
	fmt.Printf("  %-*s"+format+"\n", append([]any{labelWidth, label + ":"}, args...)...)
}

// tag renders a colored [LABEL] status marker, e.g. [OK]/[WARN]/[ERROR]
func tag(color, label string) string {
	return fmt.Sprintf("%s[%s]%s", color, label, colorReset)
}

// fail prints an error the same way in both output modes, then exits
func fail(jsonOut bool, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if jsonOut {
		emitJSON(jsonReport{Error: msg})
	} else {
		fmt.Printf("%s %s\n", tag(colorErr, "ERROR"), msg)
	}
	os.Exit(2)
}

// parseIntList parses a comma-separated list of positive ints, e.g. "8,16,32"
func parseIntList(s string) ([]int, error) {
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid block size %q, expected a comma-separated list of positive integers", p)
		}
		out = append(out, n)
	}
	return out, nil
}

// ldpcBlockLabel: 0 means one block covering the whole remaining key
func ldpcBlockLabel(blockLength int) string {
	if blockLength <= 0 {
		return "auto"
	}
	return strconv.Itoa(blockLength)
}

// simulate generates correlated Alice/Bob click streams with configurable
// error, basis-mismatch, jitter and multi-photon rates --> swap for real logs
func simulate(n int, errRate, multiPhotonRate float64, jitterPS int64, seed int64) ([]qkd.DetectionEvent, []qkd.DetectionEvent) {
	r := rand.New(rand.NewSource(seed))
	alice := make([]qkd.DetectionEvent, 0, n)
	bob := make([]qkd.DetectionEvent, 0, n)

	t := int64(0)
	for range n {
		t += 1000 // nominal 1000 ps spacing between pair-emission attempts
		aBasis := qkd.Basis(r.Intn(2))
		bBasis := qkd.Basis(r.Intn(2))
		aBit := byte(r.Intn(2))
		bBit := aBit
		if aBasis == bBasis && r.Float64() < errRate {
			bBit ^= 1
		} else if aBasis != bBasis {
			bBit = byte(r.Intn(2))
		}

		aMP := r.Float64() < multiPhotonRate
		bMP := r.Float64() < multiPhotonRate

		aJitter := int64(r.Intn(int(2*jitterPS+1))) - jitterPS
		bJitter := int64(r.Intn(int(2*jitterPS+1))) - jitterPS

		alice = append(alice, qkd.DetectionEvent{TimestampPS: t + aJitter, Basis: aBasis, Bit: aBit, MultiPhoton: aMP})
		bob = append(bob, qkd.DetectionEvent{TimestampPS: t + bJitter, Basis: bBasis, Bit: bBit, MultiPhoton: bMP})
	}
	return alice, bob
}

// saveToKeystore mirrors the three runXPipeline functions' keystore step:
// prints the outcome (unless jsonOut) and returns how many keys were saved
func saveToKeystore(keystoreDir string, finalKeyBytes []byte, jsonOut bool) int {
	if keystoreDir == "" {
		return 0
	}
	if len(finalKeyBytes) < qkd.ETSI014KeySizeBytes {
		if !jsonOut {
			field("Keystore", "skipped "+tag(colorWarn, "WARN")+" not enough key material for one ETSI 014 256-bit key")
		}
		return 0
	}
	stored, discardedBits, err := qkd.SaveKeys256(keystoreDir, finalKeyBytes)
	if !jsonOut {
		if err != nil {
			field("Keystore", "%s "+tag(colorErr, "ERROR"), err)
		} else {
			field("Keystore", "%s (%d x 256-bit ETSI 014 keys) "+tag(colorOK, "OK"), keystoreDir, len(stored))
			if discardedBits > 0 {
				field("Keystore remainder", "%d bits (short of a full 256-bit key, discarded)", discardedBits)
			}
			field("First key_ID", "%s", stored[0].KeyID)
		}
	}
	if err != nil {
		return 0
	}
	return len(stored)
}

func main() {
	n := flag.Int("n", 20000, "number of raw emission attempts to simulate")
	errRate := flag.Float64("err", 0.03, "intrinsic bit error rate on matched-basis events")
	mpRate := flag.Float64("multiphoton", 0.01, "probability a given click is flagged multi-photon")
	windowPS := flag.Int64("window", 500, "coincidence window in picoseconds")
	jitterPS := flag.Int64("jitter", 300, "simulated detector timing jitter in picoseconds")
	matrixName := flag.String("matrix", "bch", "step 2 generator matrix: bch")
	chunkSize := flag.Int("chunk", 2048, "step 3 CRC chunk size (bits); with -winnow this is the post-correction CRC pass, so it wants a much smaller value, e.g. 128")
	seed := flag.Int64("seed", 1, "PRNG seed for the simulation")
	keystoreDir := flag.String("keystore", "", "directory to save the final key into (ETSI 014 key_ID/key JSON); empty = don't save")
	searchMatrix := flag.Bool("search-matrix", false, "design a step 2 matrix by searching on independently-simulated proxy data, then apply it here, instead of -matrix (see matrixsearch.go)")
	searchM := flag.Int("search-m", 16, "block size for the searched matrix")
	searchP := flag.Int("search-p", 4, "parity bits for the searched matrix")
	searchIterations := flag.Int("search-iterations", 3000, "hill-climbing steps for the search")
	searchSeed := flag.Int64("search-seed", 99, "PRNG seed for the search itself and its proxy dataset")
	searchOfflineErr := flag.Float64("search-offline-err", 0.03, "target error rate for the simulated proxy data the search designs against; set this to match -err in real practice, never search directly on the actual dataset being reconciled")
	searchOfflineN := flag.Int("search-offline-n", 100000, "raw attempts in the simulated proxy dataset used to design the matrix")
	winnow := flag.Bool("winnow", false, "correct errors with Winnow instead of discarding blocks (see winnow.go/pipeline_winnow.go)")
	winnowBlocks := flag.String("winnow-blocks", "25,50,100", "comma-separated Hamming block size per Winnow pass, one pass each; tune to your QBER (see pipeline_winnow_test.go)")
	winnowSample := flag.Float64("winnow-sample", 0.05, "fraction of sifted bits sacrificed to measure QBER directly before Winnow correction")
	ldpc := flag.Bool("ldpc", false, "correct errors with LDPC belief propagation instead of discarding blocks (see ldpc.go/pipeline_ldpc.go)")
	ldpcBlock := flag.Int("ldpc-block", 0, "LDPC codeword length in bits, 0 = one block covering the whole remaining sifted key")
	ldpcWc := flag.Int("ldpc-wc", 3, "LDPC column weight (variable node degree)")
	ldpcWr := flag.Int("ldpc-wr", 10, "LDPC row weight (check node degree); tune rate=1-wc/wr comfortably below 1-h2(QBER), not just barely under it -- a regular code's real decoding threshold runs below the Shannon bound, see ldpc_test.go")
	ldpcIterations := flag.Int("ldpc-iterations", 0, "belief propagation iteration cap per block, 0 = default 50")
	ldpcSample := flag.Float64("ldpc-sample", 0.05, "fraction of sifted bits sacrificed to measure QBER directly before LDPC correction")
	jsonOut := flag.Bool("json", false, "print one JSON metrics object to stdout instead of the human-readable report")
	flag.Parse()

	if (*winnow && *ldpc) || (*winnow && *searchMatrix) || (*ldpc && *searchMatrix) {
		fail(*jsonOut, "-winnow, -ldpc, and -search-matrix are mutually exclusive")
	}

	method := "discard"
	switch {
	case *winnow:
		method = "winnow"
	case *ldpc:
		method = "ldpc"
	}

	// matrix resolution only matters for the discard/search-matrix path;
	// Winnow and LDPC don't use a step 2 generator matrix at all
	var g qkd.GeneratorMatrix
	matrixLabel := *matrixName
	searchFitness := 0.0
	if method == "discard" {
		if *searchMatrix {
			// proxy dataset is independent of the run below (own seed, own
			// simulate() call) --> the matrix choice never depends on the
			// actual bits it will later be applied to, see matrixsearch.go
			proxyAlice, proxyBob := simulate(*searchOfflineN, *searchOfflineErr, *mpRate, *jitterPS, *searchSeed)
			g, searchFitness = qkd.SearchGeneratorMatrix(*searchM, *searchP, proxyAlice, proxyBob, *windowPS, *chunkSize, *searchIterations, *searchSeed)
			matrixLabel = fmt.Sprintf("search(%d,%d)", *searchM, *searchP)
		} else {
			var ok bool
			g, ok = generatorMatrices[*matrixName]
			if !ok {
				fail(*jsonOut, "unknown -matrix %q, expected bch", *matrixName)
			}
		}
	}

	if !*jsonOut {
		section("qoj_post_processing run configuration:")
		field("Raw attempts", "%d", *n)
		field("Intrinsic error rate", "%.4f", *errRate)
		field("Multi-photon rate", "%.4f", *mpRate)
		field("Coincidence window", "%d ps", *windowPS)
		field("Detector jitter", "%d ps", *jitterPS)
		switch method {
		case "discard":
			field("Generator matrix (step 2)", "%s (m=%d, p=%d)", matrixLabel, g.BlockSize(), g.ParityBits())
			if *searchMatrix {
				field("Search", "designed offline at err=%.3f, n=%d, fitness=%.5f", *searchOfflineErr, *searchOfflineN, searchFitness)
			}
		case "winnow":
			field("Method (step 2/3)", "Winnow correction (blocks=%s, sample=%.2f)", *winnowBlocks, *winnowSample)
		case "ldpc":
			field("Method (step 2/3)", "LDPC correction (block=%s, wc=%d, wr=%d, sample=%.2f)",
				ldpcBlockLabel(*ldpcBlock), *ldpcWc, *ldpcWr, *ldpcSample)
		}
		field("Chunk size (step 3)", "%d bits", *chunkSize)
		field("PRNG seed", "%d", *seed)
		field("GOMAXPROCS", "%d", runtime.GOMAXPROCS(0))
	}

	aliceEv, bobEv := simulate(*n, *errRate, *mpRate, *jitterPS, *seed)

	var report jsonReport
	switch method {
	case "winnow":
		blockSizes, err := parseIntList(*winnowBlocks)
		if err != nil {
			fail(*jsonOut, "-winnow-blocks: %v", err)
		}
		report = runWinnowPipeline(aliceEv, bobEv, *windowPS, blockSizes, *winnowSample, *chunkSize, *keystoreDir, *jsonOut)
	case "ldpc":
		report = runLDPCPipeline(aliceEv, bobEv, *windowPS, *ldpcBlock, *ldpcWc, *ldpcWr, *ldpcIterations, *ldpcSample, *chunkSize, *keystoreDir, *jsonOut)
	default:
		report = runDiscardPipeline(aliceEv, bobEv, *windowPS, g, matrixLabel, *chunkSize, *keystoreDir, *jsonOut)
		report.SearchFitness = searchFitness
	}
	report.Method = method
	report.RawAttempts = *n
	report.ErrRate = *errRate
	if method == "discard" {
		report.MatrixLabel = matrixLabel
	}

	if *jsonOut {
		emitJSON(report)
		return
	}
	section("done")
	field("Wall time", "%dms", report.ElapsedMS)
}

// runDiscardPipeline runs qkd.Run (step 2/3: block discard) and prints the
// original report shape this command always used
func runDiscardPipeline(aliceEv, bobEv []qkd.DetectionEvent, windowPS int64, g qkd.GeneratorMatrix, matrixLabel string, chunkSize int, keystoreDir string, jsonOut bool) jsonReport {
	cfg := qkd.Config{
		CoincidenceWindowPS: windowPS,
		GeneratorMatrix:     g,
		ChunkSize:           chunkSize,
	}
	if keystoreDir != "" {
		cfg.KeyAlignBits = qkd.ETSI014KeySizeBits
	}

	t0 := time.Now()
	res, err := qkd.Run(aliceEv, bobEv, cfg)
	elapsed := time.Since(t0)
	if err != nil {
		fail(jsonOut, "pipeline error: %v", err)
	}

	bd := res.BlockDetect
	cv := res.CRCVerify
	rate := 0.0
	if len(cv.SurvivingAlice) > 0 {
		rate = float64(len(res.FinalKeyBits)) / float64(len(cv.SurvivingAlice))
	}

	if !jsonOut {
		section("(1): sifting")
		field("Sifted bits", "%d", res.SiftedBits)

		blockDiscardFrac := 0.0
		if res.SiftedBits > 0 {
			blockDiscardFrac = float64(len(bd.DiscardedAlice)) / float64(res.SiftedBits)
		}
		blockTag := tag(colorOK, "OK")
		if blockDiscardFrac > 0.5 {
			blockTag = tag(colorWarn, "WARN")
		}
		section(fmt.Sprintf("(2): error detection (matrix=%s, block=%d bits)", matrixLabel, g.BlockSize()))
		field("Blocks total", "%d", bd.BlocksTotal)
		field("Blocks kept", "%d", bd.BlocksKept)
		field("Blocks dropped", "%d (%.1f%%) "+blockTag, bd.BlocksDropped, blockDiscardFrac*100)
		field("Bits surviving", "%d", len(bd.SurvivingAlice))
		field("Bits leaked (parity)", "%d", bd.LeakedBits)

		printCRCAndKey(cv, chunkSize, res.QBER.QBER, res.LeakedBits, res.FinalKeyBits, res.FinalKeyBytes, rate)
	}
	keysSaved := saveToKeystore(keystoreDir, res.FinalKeyBytes, jsonOut)

	return jsonReport{
		SiftedBits:       res.SiftedBits,
		BlocksTotal:      bd.BlocksTotal,
		BlocksKept:       bd.BlocksKept,
		BlocksDropped:    bd.BlocksDropped,
		BitsLeakedParity: bd.LeakedBits,
		ChunksTotal:      cv.ChunksTotal,
		ChunksKept:       cv.ChunksKept,
		ChunksDropped:    cv.ChunksDropped,
		BitsLeakedCRC:    cv.LeakedBits,
		QBER:             res.QBER.QBER,
		LeakedBitsTotal:  res.LeakedBits,
		FinalKeyBits:     len(res.FinalKeyBits),
		CompressionRate:  rate,
		KeysSaved:        keysSaved,
		ElapsedMS:        elapsed.Milliseconds(),
	}
}

// runWinnowPipeline runs qkd.RunWinnow (step 2/3: Winnow correction) and
// prints an analogous report, same shape as cmd/importtt's
func runWinnowPipeline(aliceEv, bobEv []qkd.DetectionEvent, windowPS int64, blockSizes []int, sampleFraction float64, chunkSize int, keystoreDir string, jsonOut bool) jsonReport {
	cfg := qkd.WinnowRunConfig{
		CoincidenceWindowPS: windowPS,
		SampleFraction:      sampleFraction,
		WinnowBlockSizes:    blockSizes,
		Seed:                1,
		ChunkSize:           chunkSize,
	}
	if keystoreDir != "" {
		cfg.KeyAlignBits = qkd.ETSI014KeySizeBits
	}

	t0 := time.Now()
	res, err := qkd.RunWinnow(aliceEv, bobEv, cfg)
	elapsed := time.Since(t0)
	if err != nil {
		fail(jsonOut, "pipeline error: %v", err)
	}

	cv := res.CRCVerify
	rate := 0.0
	if len(cv.SurvivingAlice) > 0 {
		rate = float64(len(res.FinalKeyBits)) / float64(len(cv.SurvivingAlice))
	}

	if !jsonOut {
		section("(1): sifting")
		field("Sifted bits", "%d", res.SiftedBits)

		section("(4 before 2/3): QBER from a sacrificed public sample")
		field("Sample bits", "%d (%.1f%% of sifted)", res.QBER.SampleBits, sampleFraction*100)
		field("Error bits in sample", "%d", res.QBER.ErrorBits)
		field("QBER estimate", "%.4f", res.QBER.QBER)

		section(fmt.Sprintf("(2): Winnow correction (blocks=%v)", blockSizes))
		field("Bits corrected toward Alice", "%d", res.WinnowCorrections)
		field("Bits leaked (syndromes)", "%d", res.WinnowLeakedBits)

		printCRCAndKey(cv, chunkSize, res.QBER.QBER, res.LeakedBits, res.FinalKeyBits, res.FinalKeyBytes, rate)
	}
	keysSaved := saveToKeystore(keystoreDir, res.FinalKeyBytes, jsonOut)

	return jsonReport{
		SiftedBits:        res.SiftedBits,
		WinnowCorrections: res.WinnowCorrections,
		WinnowLeakedBits:  res.WinnowLeakedBits,
		ChunksTotal:       cv.ChunksTotal,
		ChunksKept:        cv.ChunksKept,
		ChunksDropped:     cv.ChunksDropped,
		BitsLeakedCRC:     cv.LeakedBits,
		QBER:              res.QBER.QBER,
		LeakedBitsTotal:   res.LeakedBits,
		FinalKeyBits:      len(res.FinalKeyBits),
		CompressionRate:   rate,
		KeysSaved:         keysSaved,
		ElapsedMS:         elapsed.Milliseconds(),
	}
}

// runLDPCPipeline runs qkd.RunLDPC (step 2/3: LDPC belief propagation) and
// prints an analogous report, same shape as cmd/importtt's
func runLDPCPipeline(aliceEv, bobEv []qkd.DetectionEvent, windowPS int64, blockLength, wc, wr, maxIterations int, sampleFraction float64, chunkSize int, keystoreDir string, jsonOut bool) jsonReport {
	cfg := qkd.LDPCRunConfig{
		CoincidenceWindowPS: windowPS,
		SampleFraction:      sampleFraction,
		BlockLength:         blockLength,
		ColWeight:           wc,
		RowWeight:           wr,
		MaxIterations:       maxIterations,
		Seed:                1,
		ChunkSize:           chunkSize,
	}
	if keystoreDir != "" {
		cfg.KeyAlignBits = qkd.ETSI014KeySizeBits
	}

	t0 := time.Now()
	res, err := qkd.RunLDPC(aliceEv, bobEv, cfg)
	elapsed := time.Since(t0)
	if err != nil {
		fail(jsonOut, "pipeline error: %v", err)
	}

	cv := res.CRCVerify
	rate := 0.0
	if len(cv.SurvivingAlice) > 0 {
		rate = float64(len(res.FinalKeyBits)) / float64(len(cv.SurvivingAlice))
	}

	if !jsonOut {
		section("(1): sifting")
		field("Sifted bits", "%d", res.SiftedBits)

		section("(4 before 2/3): QBER from a sacrificed public sample")
		field("Sample bits", "%d (%.1f%% of sifted)", res.QBER.SampleBits, sampleFraction*100)
		field("Error bits in sample", "%d", res.QBER.ErrorBits)
		field("QBER estimate", "%.4f", res.QBER.QBER)

		convergeTag := tag(colorOK, "OK")
		if res.LDPCConverged < res.LDPCBlocks {
			convergeTag = tag(colorWarn, "WARN")
		}
		section(fmt.Sprintf("(2): LDPC correction (wc=%d, wr=%d, rate=%.3f)", wc, wr, 1-float64(wc)/float64(wr)))
		field("Blocks total", "%d", res.LDPCBlocks)
		field("Blocks converged", "%d / %d "+convergeTag, res.LDPCConverged, res.LDPCBlocks)
		field("Bits leaked (syndromes)", "%d", res.LDPCLeakedBits)

		printCRCAndKey(cv, chunkSize, res.QBER.QBER, res.LeakedBits, res.FinalKeyBits, res.FinalKeyBytes, rate)
	}
	keysSaved := saveToKeystore(keystoreDir, res.FinalKeyBytes, jsonOut)

	return jsonReport{
		SiftedBits:      res.SiftedBits,
		LDPCBlocks:      res.LDPCBlocks,
		LDPCConverged:   res.LDPCConverged,
		LDPCLeakedBits:  res.LDPCLeakedBits,
		ChunksTotal:     cv.ChunksTotal,
		ChunksKept:      cv.ChunksKept,
		ChunksDropped:   cv.ChunksDropped,
		BitsLeakedCRC:   cv.LeakedBits,
		QBER:            res.QBER.QBER,
		LeakedBitsTotal: res.LeakedBits,
		FinalKeyBits:    len(res.FinalKeyBits),
		CompressionRate: rate,
		KeysSaved:       keysSaved,
		ElapsedMS:       elapsed.Milliseconds(),
	}
}

// printCRCAndKey is step 3 + 5's report, identical across all three methods
func printCRCAndKey(cv qkd.CRCVerifyResult, chunkSize int, qber float64, leakedBits int, finalKeyBits, finalKeyBytes []byte, rate float64) {
	section(fmt.Sprintf("(3): error verification (chunk=%d bits, CRC-16)", chunkSize))
	field("Chunks total", "%d", cv.ChunksTotal)
	field("Chunks kept", "%d", cv.ChunksKept)
	field("Chunks dropped", "%d", cv.ChunksDropped)
	field("Bits surviving", "%d", len(cv.SurvivingAlice))
	field("Bits leaked (CRCs)", "%d", cv.LeakedBits)

	residualErr := 0
	for i := range cv.SurvivingAlice {
		if cv.SurvivingAlice[i] != cv.SurvivingBob[i] {
			residualErr++
		}
	}
	sanityTag := tag(colorOK, "OK")
	if residualErr > 0 {
		sanityTag = tag(colorErr, "ERROR")
	}

	section("(4): QBER calculation")
	field("QBER estimate", "%.4f", qber)
	field("Sanity: residual mismatches", "%d / %d "+sanityTag, residualErr, len(cv.SurvivingAlice))

	section("(5): privacy amplification")
	field("Input bits (error-free stream)", "%d", len(cv.SurvivingAlice))
	field("Classical bits leaked (total)", "%d", leakedBits)
	field("Distilled secret key length", "%d bits", len(finalKeyBits))
	field("Compression rate", "%.4f", rate)
	if len(finalKeyBits) > 0 {
		field("Secret key (hex)", "%s "+tag(colorOK, "OK"), hex.EncodeToString(finalKeyBytes))
	} else {
		field("Secret key", "(empty) "+tag(colorWarn, "WARN")+" QBER/leakage too high to distil a key at this length")
	}
}

// emitJSON prints one jsonReport as a single line of JSON to stdout
func emitJSON(r jsonReport) {
	enc := json.NewEncoder(os.Stdout)
	enc.Encode(r)
}
