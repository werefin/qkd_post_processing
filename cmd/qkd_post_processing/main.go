// Command qkd_post_processing runs the BBM92 post-processing chain end to end over simulated click streams:
// sifting, optimized Cascade, verification and finite-key privacy amplification
package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"time"

	"qkd_post_processing/kms"
	"qkd_post_processing/qkd"
)

// jsonReport is -json's machine-readable stand-in for the human-readable
// report, one object printed to stdout, for driving e.g. a dashboard
type jsonReport struct {
	Method          string         `json:"method"` // cascade
	RawAttempts     int            `json:"raw_attempts"`
	ErrRate         float64        `json:"err_rate"`
	SiftedBits      int            `json:"sifted_bits"`
	CascadeParities int            `json:"cascade_parities,omitempty"`
	FiniteKey       *finiteKeyJSON `json:"finite_key,omitempty"` // nil in asymptotic mode
	ChunksTotal     int            `json:"chunks_total"`
	ChunksKept      int            `json:"chunks_kept"`
	ChunksDropped   int            `json:"chunks_dropped"`
	BitsLeakedCRC   int            `json:"bits_leaked_crc"`
	QBER            float64        `json:"qber"`
	LeakedBitsTotal int            `json:"leaked_bits_total"`
	FinalKeyBits    int            `json:"final_key_bits"`
	CompressionRate float64        `json:"compression_rate"`
	KeysSaved       int            `json:"keys_saved"`
	ElapsedMS       int64          `json:"elapsed_ms"`
	Error           string         `json:"error,omitempty"`
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

// saveToKeystore is the pipeline's keystore step:
// prints the outcome (unless jsonOut) and returns how many keys were saved
func saveToKeystore(keystoreDir string, finalKeyBytes []byte, jsonOut bool) int {
	if keystoreDir == "" {
		return 0
	}
	if len(finalKeyBytes) < kms.ETSI014KeySizeBytes {
		if !jsonOut {
			field("Keystore", "skipped "+tag(colorWarn, "WARN")+" not enough key material for one ETSI 014 256-bit key")
		}
		return 0
	}
	stored, discardedBits, err := kms.SaveKeys256(keystoreDir, finalKeyBytes)
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
	chunkSize := flag.Int("chunk", 2048, "CRC chunk size (bits) of the verification after Cascade")
	seed := flag.Int64("seed", 1, "PRNG seed for the simulation")
	keystoreDir := flag.String("keystore", "", "directory to save the final key into (ETSI 014 key_ID/key JSON); empty = don't save")
	flag.Bool("cascade", true, "optimized Cascade (cascade.go), the only error correction; accepted for older callers")
	sampleFraction := flag.Float64("sample", 0.05, "fraction of sifted bits sacrificed for the QBER when -sample-auto is off")
	asymptotic := flag.Bool("asymptotic", false, "size the key by the asymptotic n(1-h2(QBER)) instead of the default finite-key bound (doc/security.md)")
	epsSec := flag.Float64("eps-sec", 1e-10, "finite-key secrecy failure probability per block")
	sampleAuto := flag.Bool("sample-auto", true, "finite-key mode: plan from -err as the declared QBER prior")
	epsCor := flag.Float64("eps-cor", 1e-15, "finite-key correctness failure probability per block, sets the universal-hash tag length")
	jsonOut := flag.Bool("json", false, "print one JSON metrics object to stdout instead of the human-readable report")
	flag.Parse()
	keyOpts = keyOptions{asymptotic: *asymptotic, finite: qkd.FiniteKeyConfig{EpsSec: *epsSec, EpsCor: *epsCor}}
	if *sampleAuto && !*asymptotic {
		keyOpts.expectedQBER = max(*errRate, 0.01)
	}

	if !*jsonOut {
		section("qkd_post_processing run configuration:")
		field("Raw attempts", "%d", *n)
		field("Intrinsic error rate", "%.4f", *errRate)
		field("Multi-photon rate", "%.4f", *mpRate)
		field("Coincidence window", "%d ps", *windowPS)
		field("Detector jitter", "%d ps", *jitterPS)
		field("Method", "optimized Cascade")
		field("Chunk size", "%d bits", *chunkSize)
		field("PRNG seed", "%d", *seed)
		field("GOMAXPROCS", "%d", runtime.GOMAXPROCS(0))
	}

	aliceEv, bobEv := simulate(*n, *errRate, *mpRate, *jitterPS, *seed)
	report := runCascadePipeline(aliceEv, bobEv, *windowPS, *sampleFraction, *chunkSize, *keystoreDir, *jsonOut)
	report.Method = "cascade"
	report.RawAttempts = *n
	report.ErrRate = *errRate

	if *jsonOut {
		emitJSON(report)
		return
	}
	section("done")
	field("Wall time", "%dms", report.ElapsedMS)
}

// runCascadePipeline runs qkd.RunCascade and prints its report stage by stage
func runCascadePipeline(aliceEv, bobEv []qkd.DetectionEvent, windowPS int64, sampleFraction float64, chunkSize int, keystoreDir string, jsonOut bool) jsonReport {
	cfg := qkd.CascadeRunConfig{CoincidenceWindowPS: windowPS, SampleFraction: sampleFraction, Seed: 1, ChunkSize: chunkSize,
		Asymptotic: keyOpts.asymptotic, FiniteKey: keyOpts.finite, ExpectedQBER: keyOpts.expectedQBER}
	if keystoreDir != "" {
		cfg.KeyAlignBits = kms.ETSI014KeySizeBits
	}
	t0 := time.Now()
	res, err := qkd.RunCascade(aliceEv, bobEv, cfg)
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
		field("Sample bits", "%d (%.1f%% of sifted)", res.QBER.SampleBits, 100*float64(res.QBER.SampleBits)/float64(max(1, res.SiftedBits)))
		field("Error bits in sample", "%d", res.QBER.ErrorBits)
		field("QBER estimate", "%.4f", res.QBER.QBER)

		section(fmt.Sprintf("(2): optimized Cascade (blocks=%v)", res.Cascade.BlockSizes))
		field("Bits corrected toward Alice", "%d", res.Cascade.Corrections)
		field("Parities disclosed", "%d (%d more inferred)", res.Cascade.Parities, res.Cascade.Inferred)

		printFiniteKey(res.FiniteKey)
		printCRCAndKey(cv, chunkSize, res.QBER.QBER, res.LeakedBits, res.FinalKeyBits, res.FinalKeyBytes, rate)
	}
	keysSaved := saveToKeystore(keystoreDir, res.FinalKeyBytes, jsonOut)
	return jsonReport{
		SiftedBits: res.SiftedBits, CascadeParities: res.Cascade.Parities, FiniteKey: toFiniteKeyJSON(res.FiniteKey),
		ChunksTotal: cv.ChunksTotal, ChunksKept: cv.ChunksKept, ChunksDropped: cv.ChunksDropped, BitsLeakedCRC: cv.LeakedBits,
		QBER: res.QBER.QBER, LeakedBitsTotal: res.LeakedBits, FinalKeyBits: len(res.FinalKeyBits), CompressionRate: rate,
		KeysSaved: keysSaved, ElapsedMS: elapsed.Milliseconds(),
	}
}

// printCRCAndKey is step 3 + 5's report, identical across all four methods
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

// keyOptions is step 5's accounting, set once from flags: finite-key unless -asymptotic
type keyOptions struct {
	asymptotic   bool
	finite       qkd.FiniteKeyConfig
	expectedQBER float64 // >0: size the QBER sample automatically from this prior
}

var keyOpts keyOptions

// printFiniteKey shows how the finite-key length was derived, or notes asymptotic mode
func printFiniteKey(r *qkd.FiniteKeyReport) {
	if r == nil {
		field("Key length bound", "asymptotic n(1-h2(QBER)) - leaked (-asymptotic, no finite-size margin)")
		return
	}
	fk := keyOpts.finite
	field("Key length bound", "finite-key, eps_sec=%.0e eps_cor=%.0e", fk.EpsSec, fk.EpsCor)
	field("Phase error", "point %.4f, bound %.4f (cross-basis, fluctuation and post-selection included)", r.PhiPoint, r.Phi)
	switch {
	case r.Bits == 0:
		field("Correctness check", "nothing survived error correction, nothing to verify")
	case r.Verified:
		field("Correctness check", "universal hash, %d-bit tag: match", r.TagBits)
	default:
		field("Correctness check", "universal hash, %d-bit tag: MISMATCH, no key produced", r.TagBits)
	}
	field("Bits charged to key length", "%d (all disclosure, incl. the %d-bit tag)", r.LeakedBits, r.TagBits)
}

// finiteKeyJSON is the -json view of a finite-key report; nil (asymptotic mode) stays nil
type finiteKeyJSON struct {
	PhaseErrorPoint float64 `json:"phase_error_point"`
	PhaseErrorBound float64 `json:"phase_error_bound"`
	TagBits         int     `json:"tag_bits"`
	Verified        bool    `json:"verified"`
	SurvivingBits   int     `json:"surviving_bits"`
	ChargedBits     int     `json:"charged_bits"`
}

func toFiniteKeyJSON(r *qkd.FiniteKeyReport) *finiteKeyJSON {
	if r == nil {
		return nil
	}
	return &finiteKeyJSON{PhaseErrorPoint: r.PhiPoint, PhaseErrorBound: r.Phi, TagBits: r.TagBits, Verified: r.Verified, SurvivingBits: r.Bits, ChargedBits: r.LeakedBits}
}
