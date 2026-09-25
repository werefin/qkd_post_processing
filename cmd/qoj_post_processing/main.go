// Command qoj_post_processing runs the BBM92 post-processing chain end to end
// over simulated click streams, or embed qkd.Run as a library call instead
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"math/rand"
	"runtime"
	"time"

	qkd "qoj_post_processing"
)

// generatorMatrices maps -matrix flag values to the example step 2
// generator matrices
var generatorMatrices = map[string]qkd.GeneratorMatrix{
	"ex1": qkd.GeneratorMatrixEx1,
	"ex2": qkd.GeneratorMatrixEx2,
	"ex3": qkd.GeneratorMatrixEx3,
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

func main() {
	n := flag.Int("n", 20000, "number of raw emission attempts to simulate")
	errRate := flag.Float64("err", 0.03, "intrinsic bit error rate on matched-basis events")
	mpRate := flag.Float64("multiphoton", 0.01, "probability a given click is flagged multi-photon")
	windowPS := flag.Int64("window", 500, "coincidence window in picoseconds")
	jitterPS := flag.Int64("jitter", 300, "simulated detector timing jitter in picoseconds")
	matrixName := flag.String("matrix", "ex3", "step 2 generator matrix: ex1, ex2, ex3, or bch")
	chunkSize := flag.Int("chunk", 2048, "step 3 CRC chunk size (bits)")
	seed := flag.Int64("seed", 1, "PRNG seed for the simulation")
	keystoreDir := flag.String("keystore", "", "directory to save the final key into (ETSI 014 key_ID/key JSON); empty = don't save")
	flag.Parse()

	g, ok := generatorMatrices[*matrixName]
	if !ok {
		fmt.Printf("%s unknown -matrix %q, expected ex1, ex2, ex3, or bch\n", tag(colorErr, "ERROR"), *matrixName)
		return
	}

	section("qoj_post_processing run configuration:")
	field("Raw attempts", "%d", *n)
	field("Intrinsic error rate", "%.4f", *errRate)
	field("Multi-photon rate", "%.4f", *mpRate)
	field("Coincidence window", "%d ps", *windowPS)
	field("Detector jitter", "%d ps", *jitterPS)
	field("Generator matrix (step 2)", "%s (m=%d, p=%d)", *matrixName, g.BlockSize(), g.ParityBits())
	field("Chunk size (step 3)", "%d bits", *chunkSize)
	field("PRNG seed", "%d", *seed)
	field("GOMAXPROCS", "%d", runtime.GOMAXPROCS(0))

	aliceEv, bobEv := simulate(*n, *errRate, *mpRate, *jitterPS, *seed)

	cfg := qkd.Config{
		CoincidenceWindowPS: *windowPS,
		GeneratorMatrix:     g,
		ChunkSize:           *chunkSize,
	}

	t0 := time.Now()
	res, err := qkd.Run(aliceEv, bobEv, cfg)
	elapsed := time.Since(t0)
	if err != nil {
		fmt.Printf("\n%s pipeline error: %v\n", tag(colorErr, "ERROR"), err)
		return
	}

	section("(1): sifting")
	field("Raw attempts", "%d", *n)
	field("Sifted bits", "%d", res.SiftedBits)

	bd := res.BlockDetect
	blockDiscardFrac := 0.0
	if res.SiftedBits > 0 {
		blockDiscardFrac = float64(len(bd.DiscardedAlice)) / float64(res.SiftedBits)
	}
	blockTag := tag(colorOK, "OK")
	if blockDiscardFrac > 0.5 {
		blockTag = tag(colorWarn, "WARN")
	}
	section(fmt.Sprintf("(2): error detection (matrix=%s, block=%d bits)", *matrixName, g.BlockSize()))
	field("Blocks total", "%d", bd.BlocksTotal)
	field("Blocks kept", "%d", bd.BlocksKept)
	field("Blocks dropped", "%d (%.1f%%) "+blockTag, bd.BlocksDropped, blockDiscardFrac*100)
	field("Bits surviving", "%d", len(bd.SurvivingAlice))
	field("Bits leaked (parity)", "%d", bd.LeakedBits)

	cv := res.CRCVerify
	section(fmt.Sprintf("(3): error verification (chunk=%d bits, CRC-16)", *chunkSize))
	field("Chunks total", "%d", cv.ChunksTotal)
	field("Chunks kept", "%d", cv.ChunksKept)
	field("Chunks dropped", "%d", cv.ChunksDropped)
	field("Bits surviving", "%d", len(cv.SurvivingAlice))
	field("Bits leaked (CRCs)", "%d", cv.LeakedBits)

	section("(4): QBER calculation")
	field("Sample bits (sifted key)", "%d", res.QBER.SampleBits)
	field("Error bits in faulty blocks", "%d", res.QBER.ErrorBits)
	field("QBER estimate", "%.4f", res.QBER.QBER)

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
	field("Sanity: residual mismatches", "%d / %d "+sanityTag, residualErr, len(cv.SurvivingAlice))

	rate := 0.0
	if len(cv.SurvivingAlice) > 0 {
		rate = float64(len(res.FinalKeyBits)) / float64(len(cv.SurvivingAlice))
	}
	section("(5): privacy amplification")
	field("Input bits (error-free stream)", "%d", len(cv.SurvivingAlice))
	field("Classical bits leaked (total)", "%d", res.LeakedBits)
	field("Distilled secret key length", "%d bits", len(res.FinalKeyBits))
	field("Compression rate", "%.4f", rate)
	if len(res.FinalKeyBits) > 0 {
		field("Secret key (hex)", "%s "+tag(colorOK, "OK"), hex.EncodeToString(res.FinalKeyBytes))
	} else {
		field("Secret key", "(empty) "+tag(colorWarn, "WARN")+" QBER/leakage too high to distil a key at this length")
	}

	if *keystoreDir != "" {
		if len(res.FinalKeyBytes) == 0 {
			field("Keystore", "skipped "+tag(colorWarn, "WARN")+" no key to save")
		} else {
			stored, err := qkd.SaveKey(*keystoreDir, res.FinalKeyBytes)
			if err != nil {
				field("Keystore", "%s "+tag(colorErr, "ERROR"), err)
			} else {
				field("Keystore", "%s/%s.json "+tag(colorOK, "OK"), *keystoreDir, stored.KeyID)
				field("key_ID", "%s", stored.KeyID)
			}
		}
	}

	section("done")
	field("Wall time", "%s", elapsed)
}
