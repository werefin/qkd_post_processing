// Command importtt runs the BBM92 post-processing chain over a real dual-TT recording
// from acquisition/tt_record_dual.py, aligned to a common clock by acquisition/coincidence_peak.py
package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	qkd "qoj_post_processing"
)

// simulateOffline is a click-stream generator used only to design a
// -search-matrix candidate independent of this recording's real bits
func simulateOffline(n int, errRate, multiPhotonRate float64, jitterPS, seed int64) (alice, bob []qkd.DetectionEvent) {
	r := rand.New(rand.NewSource(seed))
	t := int64(0)
	for range n {
		t += 1000
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

// generatorMatrices maps -matrix flag values to the fixed step 2
// generator matrices, same set cmd/qoj_post_processing exposes
var generatorMatrices = map[string]qkd.GeneratorMatrix{
	"bch": qkd.GeneratorMatrixBCHt2,
}

// ANSI color codes for status tags, duplicated from cmd/qoj_post_processing so this
// tool has no dependency beyond the library itself
const (
	colorReset = "\033[0m"
	colorOK    = "\033[32m"
	colorWarn  = "\033[33m"
	colorErr   = "\033[31m"
)

const labelWidth = 32

func section(title string) {
	fmt.Printf("\n%s\n", title)
}

func field(label, format string, args ...any) {
	fmt.Printf("  %-*s"+format+"\n", append([]any{labelWidth, label + ":"}, args...)...)
}

func tag(color, label string) string {
	return fmt.Sprintf("%s[%s]%s", color, label, colorReset)
}

// note prints a leading-tagged, self-explanatory follow-up line under a
// field that needs one, same "[LEVEL] explain why" convention the acquisition
// scripts use, instead of a bare status tag with no explanation
func note(color, level, msg string) {
	fmt.Printf("  %s %s\n", tag(color, level), msg)
}

// fail prints an error the same way in both output modes, then exits
func fail(jsonOut bool, format string, args ...any) {
	if jsonOut {
		failJSON(format, args...)
		return
	}
	fmt.Printf("%s %s\n", tag(colorErr, "ERROR"), fmt.Sprintf(format, args...))
	os.Exit(2)
}

// saveToKeystore mirrors cmd/qoj_post_processing's helper of the same name:
// prints the outcome (unless jsonOut) and returns how many keys were saved
// maxKeys (0 = unlimited) caps the keystore's total key count
func saveToKeystore(keystoreDir string, finalKeyBytes []byte, maxKeys int, jsonOut bool) int {
	if keystoreDir == "" {
		return 0
	}
	if maxKeys > 0 {
		existing, _ := filepath.Glob(filepath.Join(keystoreDir, "*.json"))
		remaining := maxKeys - len(existing)
		if remaining <= 0 {
			if !jsonOut {
				field("Keystore", "skipped")
				note(colorWarn, "WARN", fmt.Sprintf("already at the %d-key cap (%d stored); nothing from this run was saved", maxKeys, len(existing)))
			}
			return 0
		}
		if capBytes := remaining * qkd.ETSI014KeySizeBytes; len(finalKeyBytes) > capBytes {
			finalKeyBytes = finalKeyBytes[:capBytes]
		}
	}
	if len(finalKeyBytes) < qkd.ETSI014KeySizeBytes {
		if !jsonOut {
			field("Keystore", "skipped")
			note(colorWarn, "WARN", "this run's distilled key is shorter than one ETSI 014 key (256 bits); nothing to save yet")
		}
		return 0
	}
	stored, discardedBits, err := qkd.SaveKeys256(keystoreDir, finalKeyBytes)
	if !jsonOut {
		if err != nil {
			field("Keystore", "save failed")
			note(colorErr, "ERROR", fmt.Sprintf("%v", err))
		} else {
			field("Keystore", tag(colorOK, "OK")+" saved %d x 256-bit ETSI 014 key(s) to %s", len(stored), keystoreDir)
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

// jsonReport is -json's machine-readable stand-in for the human-readable
// report, one object printed to stdout, for driving e.g., a dashboard
// same shape as cmd/qoj_post_processing's jsonReport plus this tool's own
// recording-specific fields
type jsonReport struct {
	Method            string             `json:"method"` // discard, discard-epoched, winnow, or ldpc
	RecordingDir      string             `json:"recording_dir"`
	ARecording        string             `json:"a_recording"`
	BRecording        string             `json:"b_recording"`
	ADroppedClicks    int                `json:"a_dropped_clicks"`
	BDroppedClicks    int                `json:"b_dropped_clicks"`
	AMultiPhotonCount int                `json:"a_multiphoton_count"`
	BMultiPhotonCount int                `json:"b_multiphoton_count"`
	VisibilityHV      float64            `json:"visibility_hv"`
	VisibilityDA      float64            `json:"visibility_da"`
	FidelityVis       float64            `json:"fidelity_vis"`
	QBERVis           float64            `json:"qber_vis"`
	QBERBasisHV       float64            `json:"qber_bs_hv"`
	QBERBasisDA       float64            `json:"qber_bs_da"`
	SkewPPB           float64            `json:"skew_ppb"`
	WindowPS          int64              `json:"window_ps"`
	MatrixLabel       string             `json:"matrix_label,omitempty"`
	SearchFitness     float64            `json:"search_fitness,omitempty"`
	SiftedBits        int                `json:"sifted_bits"`
	BlocksTotal       int                `json:"blocks_total,omitempty"`
	BlocksKept        int                `json:"blocks_kept,omitempty"`
	BlocksDropped     int                `json:"blocks_dropped,omitempty"`
	BitsLeakedParity  int                `json:"bits_leaked_parity,omitempty"`
	WinnowCorrections int                `json:"winnow_corrections,omitempty"`
	WinnowLeakedBits  int                `json:"winnow_leaked_bits,omitempty"`
	LDPCBlocks        int                `json:"ldpc_blocks,omitempty"`
	LDPCConverged     int                `json:"ldpc_converged,omitempty"`
	LDPCLeakedBits    int                `json:"ldpc_leaked_bits,omitempty"`
	ChunksTotal       int                `json:"chunks_total"`
	ChunksKept        int                `json:"chunks_kept"`
	ChunksDropped     int                `json:"chunks_dropped"`
	BitsLeakedCRC     int                `json:"bits_leaked_crc"`
	QBER              float64            `json:"qber"`
	LeakedBitsTotal   int                `json:"leaked_bits_total"`
	FinalKeyBits      int                `json:"final_key_bits"`
	CompressionRate   float64            `json:"compression_rate"`
	KeysSaved         int                `json:"keys_saved"`
	ElapsedMS         int64              `json:"elapsed_ms"`
	WindowReport      []windowBucketJSON `json:"window_report,omitempty"`
	Error             string             `json:"error,omitempty"`
}

// windowBucketJSON is one -window-report bucket's diagnostic QBER, for -json
type windowBucketJSON struct {
	StartS     float64 `json:"start_s"`
	PairCount  int     `json:"pair_count"`
	QBER       float64 `json:"qber,omitempty"`
	Reportable bool    `json:"reportable"`
}

// emitJSON prints one jsonReport as a single line of JSON to stdout
func emitJSON(r jsonReport) {
	enc := json.NewEncoder(os.Stdout)
	enc.Encode(r)
}

// failJSON prints a jsonReport carrying only an error message, mirroring how
// the human-readable path prints [ERROR] and exits
func failJSON(format string, args ...any) {
	emitJSON(jsonReport{Error: fmt.Sprintf(format, args...)})
	os.Exit(2)
}

// coincidenceReport is the subset of coincidence_peak.py's *_coincidence.json
// this tool needs: which recording was "A"/"B", and the affine clock model
// (lag + skew) that maps a B timestamp onto A's clock
type coincidenceReport struct {
	InputA    string `json:"input_a"`
	InputB    string `json:"input_b"`
	Detected  bool   `json:"detected"`
	ClockSkew struct {
		ReferenceElapsedPS float64 `json:"reference_elapsed_ps"`
		LagAtReferencePS   float64 `json:"lag_at_reference_ps"`
		SkewFraction       float64 `json:"skew_fraction"`
		SkewPPB            float64 `json:"skew_ppb"`
		CorrectedWindow    struct {
			WidthPS int64 `json:"width_ps"`
		} `json:"corrected_window"`
	} `json:"clock_skew"`
	// per-channel residual delay from coincidence_peak.py's calibrate_channel_delays,
	// keyed "a"/"b" then channel number "1".."4"; nil if no channel npy files were found
	ChannelDelays map[string]map[string]struct {
		OffsetPS int64   `json:"offset_ps"`
		Score    float64 `json:"score"`
		N        int     `json:"n"`
	} `json:"channel_delays"`
}

// findRecording locates the two *_timestamp_sequence.npy files and the one
// *_coincidence.json report in dir, and loads the report
func findRecording(dir string) (tsA, tsB string, report coincidenceReport, err error) {
	tsFiles, globErr := filepath.Glob(filepath.Join(dir, "*_timestamp_sequence.npy"))
	if globErr != nil {
		return "", "", report, globErr
	}
	if len(tsFiles) != 2 {
		return "", "", report, fmt.Errorf("%s: expected exactly two *_timestamp_sequence.npy files, found %d", dir, len(tsFiles))
	}
	sort.Strings(tsFiles)

	coincidenceFiles, globErr := filepath.Glob(filepath.Join(dir, "*_coincidence.json"))
	if globErr != nil {
		return "", "", report, globErr
	}
	if len(coincidenceFiles) != 1 {
		return "", "", report, fmt.Errorf("%s: expected exactly one *_coincidence.json report, found %d; "+
			"run acquisition/coincidence_peak.py on this directory first", dir, len(coincidenceFiles))
	}

	raw, err := os.ReadFile(coincidenceFiles[0])
	if err != nil {
		return "", "", report, err
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		return "", "", report, fmt.Errorf("%s: %w", coincidenceFiles[0], err)
	}
	if !report.Detected {
		return "", "", report, fmt.Errorf("%s: no significant coincidence peak was detected; "+
			"the clock alignment in this report is not trustworthy", coincidenceFiles[0])
	}

	// Match by basename against the report's recorded inputs; directory-mode
	// analysis always assigns A/B by sorted filename, so an exact match
	// failure (files moved, or the report analyzed two explicit files) still
	// falls back to that same sorted order
	aBase, bBase := filepath.Base(report.InputA), filepath.Base(report.InputB)
	for _, p := range tsFiles {
		switch filepath.Base(p) {
		case aBase:
			tsA = p
		case bBase:
			tsB = p
		}
	}
	if tsA == "" || tsB == "" {
		tsA, tsB = tsFiles[0], tsFiles[1]
	}
	return tsA, tsB, report, nil
}

// channelPath derives a recording's *_channel_sequence.npy path from its
// *_timestamp_sequence.npy path
func channelPath(timestampPath string) string {
	return strings.TrimSuffix(timestampPath, "_timestamp_sequence.npy") + "_channel_sequence.npy"
}

// channelToBasisBit maps a physical TimeTagger channel to a BBM92 (basis,
// bit) pair: channels 1-2 are one polarization basis, 3-4 are the other
// Any other channel (a negative falling-edge tag, an unused monitor line)
// is not a measurement click and is dropped
func channelToBasisBit(ch int8) (basis qkd.Basis, bit byte, ok bool) {
	switch ch {
	case 1:
		return 0, 0, true
	case 2:
		return 0, 1, true
	case 3:
		return 1, 0, true
	case 4:
		return 1, 1, true
	default:
		return 0, 0, false
	}
}

// basisPartnerChannel returns the other physical channel in the same basis
// pair (1 <--> 2 for H/V, 3 <--> 4 for D/A), or 0 if ch isn't a measurement channel
func basisPartnerChannel(ch int8) int8 {
	switch ch {
	case 1:
		return 2
	case 2:
		return 1
	case 3:
		return 4
	case 4:
		return 3
	default:
		return 0
	}
}

// detectMultiPhoton flags clicks with another click on their basis partner
// channel within windowPS ps; timestamps must be sorted ascending
func detectMultiPhoton(timestamps []int64, channels []int8, windowPS int64) []bool {
	flagged := make([]bool, len(timestamps))
	byChannel := map[int8][]int{}
	for i, ch := range channels {
		byChannel[ch] = append(byChannel[ch], i)
	}
	for ch, idxs := range byChannel {
		partner := basisPartnerChannel(ch)
		partnerIdxs, ok := byChannel[partner]
		if partner == 0 || !ok {
			continue
		}
		pLo := 0
		for _, i := range idxs {
			t := timestamps[i]
			for pLo < len(partnerIdxs) && timestamps[partnerIdxs[pLo]] < t-windowPS {
				pLo++
			}
			for p := pLo; p < len(partnerIdxs) && timestamps[partnerIdxs[p]] <= t+windowPS; p++ {
				flagged[i] = true
				flagged[partnerIdxs[p]] = true
			}
		}
	}
	return flagged
}

// buildEvents maps a recording's arrays to DetectionEvents, applying
// shiftPS first; multiPhoton[i] comes from detectMultiPhoton on raw timestamps
func buildEvents(timestamps []int64, channels []int8, multiPhoton []bool, shiftPS func(int64) int64) (events []qkd.DetectionEvent, dropped, flaggedMP int) {
	events = make([]qkd.DetectionEvent, 0, len(channels))
	for i, ch := range channels {
		basis, bit, ok := channelToBasisBit(ch)
		if !ok {
			dropped++
			continue
		}
		if multiPhoton[i] {
			flaggedMP++
		}
		events = append(events, qkd.DetectionEvent{TimestampPS: shiftPS(timestamps[i]), Basis: basis, Bit: bit, MultiPhoton: multiPhoton[i]})
	}
	return events, dropped, flaggedMP
}

// visibilityMetrics is a read-only diagnostic matching the vendor API's "qkd"
// component; our best-effort interpretation of their metric names, not confirmed-identical
type visibilityMetrics struct {
	CoincidenceHH, CoincidenceVV, CoincidenceHV, CoincidenceVH int
	CoincidenceDD, CoincidenceAA, CoincidenceDA, CoincidenceAD int
	SiftedHV, SiftedDA                                         int
	VisibilityHV, VisibilityDA                                 float64
	FidelityVis                                                float64
	QBERVis                                                    float64
	QBERBasisHV, QBERBasisDA                                   float64
}

// channelLabel returns the H/V/D/A label for a (basis,bit) pair, the
// inverse of channelToBasisBit
func channelLabel(basis qkd.Basis, bit byte) byte {
	switch {
	case basis == 0 && bit == 0:
		return 'H'
	case basis == 0 && bit == 1:
		return 'V'
	case basis == 1 && bit == 0:
		return 'D'
	default:
		return 'A'
	}
}

func computeVisibilityMetrics(aliceEv, bobEv []qkd.DetectionEvent, windowPS int64) visibilityMetrics {
	pairs := qkd.MatchCoincidences(qkd.StripBit(aliceEv), qkd.StripBit(bobEv), windowPS)
	var m visibilityMetrics
	for _, p := range pairs {
		a, b := aliceEv[p.AliceIdx], bobEv[p.BobIdx]
		aLabel, bLabel := channelLabel(a.Basis, a.Bit), channelLabel(b.Basis, b.Bit)
		switch {
		case aLabel == 'H' && bLabel == 'H':
			m.CoincidenceHH++
		case aLabel == 'V' && bLabel == 'V':
			m.CoincidenceVV++
		case aLabel == 'H' && bLabel == 'V':
			m.CoincidenceHV++
		case aLabel == 'V' && bLabel == 'H':
			m.CoincidenceVH++
		case aLabel == 'D' && bLabel == 'D':
			m.CoincidenceDD++
		case aLabel == 'A' && bLabel == 'A':
			m.CoincidenceAA++
		case aLabel == 'D' && bLabel == 'A':
			m.CoincidenceDA++
		case aLabel == 'A' && bLabel == 'D':
			m.CoincidenceAD++
		}
	}

	hvTotal := m.CoincidenceHH + m.CoincidenceVV + m.CoincidenceHV + m.CoincidenceVH
	daTotal := m.CoincidenceDD + m.CoincidenceAA + m.CoincidenceDA + m.CoincidenceAD
	m.SiftedHV, m.SiftedDA = hvTotal, daTotal

	if hvTotal > 0 {
		correlated, accidental := m.CoincidenceHH+m.CoincidenceVV, m.CoincidenceHV+m.CoincidenceVH
		m.VisibilityHV = float64(correlated-accidental) / float64(hvTotal)
		m.QBERBasisHV = float64(accidental) / float64(hvTotal)
	}
	if daTotal > 0 {
		correlated, accidental := m.CoincidenceDD+m.CoincidenceAA, m.CoincidenceDA+m.CoincidenceAD
		m.VisibilityDA = float64(correlated-accidental) / float64(daTotal)
		m.QBERBasisDA = float64(accidental) / float64(daTotal)
	}
	if hvTotal+daTotal > 0 {
		m.FidelityVis = (m.VisibilityHV + m.VisibilityDA) / 2
		totalAccidental := m.CoincidenceHV + m.CoincidenceVH + m.CoincidenceDA + m.CoincidenceAD
		m.QBERVis = float64(totalAccidental) / float64(hvTotal+daTotal)
	}
	return m
}

// channelDelayMinScore mirrors coincidence_peak.py's MIN_PEAK_SCORE; below
// this, an offset is noise-level and only shown, never meant to be applied
const channelDelayMinScore = 6.0

// printChannelDelays reports calibrate_channel_delays's per-channel offsets,
// diagnostic only --> not applied anywhere in this tool's actual alignment
func printChannelDelays(delays map[string]map[string]struct {
	OffsetPS int64   `json:"offset_ps"`
	Score    float64 `json:"score"`
	N        int     `json:"n"`
}) {
	if delays == nil {
		field("Per-channel delay calibration", "unavailable (no *_channel_sequence.npy found by coincidence_peak.py)")
		return
	}
	for _, side := range []string{"a", "b"} {
		chs, ok := delays[side]
		if !ok {
			continue
		}
		parts := make([]string, 0, 4)
		for _, ch := range []string{"1", "2", "3", "4"} {
			d, ok := chs[ch]
			if !ok {
				continue
			}
			confidence := "low-confidence"
			if d.Score >= channelDelayMinScore {
				confidence = "confident"
			}
			parts = append(parts, fmt.Sprintf("ch%s=%+dps(score=%.1f,%s)", ch, d.OffsetPS, d.Score, confidence))
		}
		field(fmt.Sprintf("Channel delays (%s)", side), "%s", strings.Join(parts, " "))
	}
}

func main() {
	dir := flag.String("dir", "", "recording directory (from tt_record_dual.py + coincidence_peak.py)")
	windowPS := flag.Int64("window", 0, "coincidence window in picoseconds (0 = use the report's corrected window)")
	multiPhotonWindowPS := flag.Int64("multiphoton-window", 2000, "reject a click as an ambiguous multi-photon event if another click lands on its basis partner channel within this many picoseconds of it (own side only, before cross-side matching); 0 disables this check")
	windowReport := flag.Bool("window-report", false, "print each time-bucket's own measured QBER as a diagnostic; does not select bits for the key (see windowreport.go for why not)")
	windowReportBucketS := flag.Float64("window-report-bucket-s", 0.5, "time-bucket width for -window-report, in seconds")
	windowReportMinPairs := flag.Int("window-report-min-pairs", 10, "buckets with fewer sifted pairs than this report as insufficient data rather than a QBER")
	epochS := flag.Float64("epoch-s", 0, "run the protocol independently once per this many seconds instead of pooling the whole recording into one QBER estimate; each epoch sizes its own privacy amplification from its own measured QBER, so a noisy epoch contributes zero bits rather than being picked after the fact (see epoch.go); 0 disables this; only applies to the default block-discard method, not -winnow/-ldpc/-search-matrix")
	matrixName := flag.String("matrix", "bch", "step 2 generator matrix: bch")
	chunkSize := flag.Int("chunk", 2048, "step 3 CRC chunk size (bits); with -winnow this is the post-correction CRC pass, so it wants a much smaller value, e.g. 128")
	keystoreDir := flag.String("keystore", "", "directory to save the final key into (ETSI 014 key_ID/key JSON); empty = don't save")
	keystoreMax := flag.Int("keystore-max", 0, "maximum keys to keep in -keystore, 0 = unlimited; existing keys are never evicted, only this run's own new keys are held back past the cap")
	winnow := flag.Bool("winnow", false, "correct errors with Winnow instead of discarding blocks (see winnow.go/pipeline_winnow.go)")
	winnowBlocks := flag.String("winnow-blocks", "25,50,100", "comma-separated Hamming block size per Winnow pass, one pass each; tune to your QBER (see pipeline_winnow_test.go)")
	winnowSample := flag.Float64("winnow-sample", 0.05, "fraction of sifted bits sacrificed to measure QBER directly before Winnow correction")
	ldpc := flag.Bool("ldpc", false, "correct errors with LDPC belief propagation instead of discarding blocks (see ldpc.go/pipeline_ldpc.go)")
	ldpcBlock := flag.Int("ldpc-block", 0, "LDPC codeword length in bits, 0 = one block covering the whole remaining sifted key")
	ldpcWc := flag.Int("ldpc-wc", 3, "LDPC column weight (variable node degree)")
	ldpcWr := flag.Int("ldpc-wr", 10, "LDPC row weight (check node degree); tune rate=1-wc/wr comfortably below 1-h2(QBER), not just barely under it; a regular code's real decoding threshold runs below the Shannon bound, see ldpc_test.go")
	ldpcIterations := flag.Int("ldpc-iterations", 0, "belief propagation iteration cap per block, 0 = default 50")
	ldpcSample := flag.Float64("ldpc-sample", 0.05, "fraction of sifted bits sacrificed to measure QBER directly before LDPC correction")
	searchMatrix := flag.Bool("search-matrix", false, "design a step 2 matrix by searching on simulated data at -search-offline-err, then apply it here, instead of -matrix (see matrixsearch.go)")
	searchM := flag.Int("search-m", 16, "block size for the searched matrix")
	searchP := flag.Int("search-p", 4, "parity bits for the searched matrix")
	searchIterations := flag.Int("search-iterations", 3000, "hill-climbing steps for the search")
	searchSeed := flag.Int64("search-seed", 99, "PRNG seed for the search itself")
	searchOfflineErr := flag.Float64("search-offline-err", 0.03, "target error rate for the simulated proxy data the search designs against; set this to your link's known QBER, never the real recording's own bits")
	searchOfflineN := flag.Int("search-offline-n", 100000, "raw attempts in the simulated proxy dataset used to design the matrix")
	searchOfflineMP := flag.Float64("search-offline-multiphoton", 0.01, "multi-photon rate for the simulated proxy data; match your detector's real rate so the search sees realistic sifting losses")
	searchOfflineJitter := flag.Int64("search-offline-jitter", 300, "detector timing jitter in picoseconds for the simulated proxy data; match your setup so the search sees realistic coincidence-window losses")
	jsonOut := flag.Bool("json", false, "print one JSON metrics object to stdout instead of the human-readable report")
	flag.Parse()

	if *dir == "" {
		if *jsonOut {
			failJSON("-dir is required")
		}
		fmt.Printf("%s -dir is required\n", tag(colorErr, "ERROR"))
		flag.Usage()
		os.Exit(2)
	}
	if (*winnow && *ldpc) || (*winnow && *searchMatrix) || (*ldpc && *searchMatrix) {
		fail(*jsonOut, "-winnow, -ldpc, and -search-matrix are mutually exclusive")
	}
	if *epochS > 0 && (*winnow || *ldpc || *searchMatrix) {
		fail(*jsonOut, "-epoch-s only applies to the default block-discard method, not -winnow/-ldpc/-search-matrix")
	}

	g, ok := generatorMatrices[*matrixName]
	if !ok {
		fail(*jsonOut, "unknown -matrix %q, expected bch", *matrixName)
	}

	tsPathA, tsPathB, report, err := findRecording(*dir)
	if err != nil {
		fail(*jsonOut, "%v", err)
	}

	tsA, err := loadNpyInt64(tsPathA)
	if err != nil {
		fail(*jsonOut, "%v", err)
	}
	chA, err := loadNpyInt8(channelPath(tsPathA))
	if err != nil {
		fail(*jsonOut, "%v", err)
	}
	tsB, err := loadNpyInt64(tsPathB)
	if err != nil {
		fail(*jsonOut, "%v", err)
	}
	chB, err := loadNpyInt8(channelPath(tsPathB))
	if err != nil {
		fail(*jsonOut, "%v", err)
	}
	if len(tsA) != len(chA) {
		fail(*jsonOut, "%s: %d timestamps but %d channels", tsPathA, len(tsA), len(chA))
	}
	if len(tsB) != len(chB) {
		fail(*jsonOut, "%s: %d timestamps but %d channels", tsPathB, len(tsB), len(chB))
	}
	if len(tsA) == 0 || len(tsB) == 0 {
		fail(*jsonOut, "empty recording")
	}

	// Alignment formula from coincidence_peak.py's report (delay is B - A):
	//   A_est = A0 + tref + ((B - B0) - (tref + lag_ref)) / (1 + skew_fraction)
	// maps a B timestamp onto A's clock, correcting both the constant lag
	// and the fitted linear clock skew between the two TimeTaggers
	a0, b0 := tsA[0], tsB[0]
	skew := report.ClockSkew
	toAliceClock := func(tsB int64) int64 {
		bRel := float64(tsB - b0)
		aRel := skew.ReferenceElapsedPS + (bRel-(skew.ReferenceElapsedPS+skew.LagAtReferencePS))/(1+skew.SkewFraction)
		return a0 + int64(math.Round(aRel))
	}

	// own-side timing check, run before any cross-side alignment or shifting
	mpA := detectMultiPhoton(tsA, chA, *multiPhotonWindowPS)
	mpB := detectMultiPhoton(tsB, chB, *multiPhotonWindowPS)

	aliceEv, droppedA, flaggedMPA := buildEvents(tsA, chA, mpA, func(ts int64) int64 { return ts })
	bobEv, droppedB, flaggedMPB := buildEvents(tsB, chB, mpB, toAliceClock)

	window := *windowPS
	if window == 0 {
		window = skew.CorrectedWindow.WidthPS
		if window == 0 {
			window = 500
			if !*jsonOut {
				fmt.Printf("%s no -window given and the report has no corrected window; falling back to %d ps\n", tag(colorWarn, "WARN"), window)
			}
		}
	}

	if !*jsonOut {
		section("importtt run configuration:")
		field("Recording directory", "%s", *dir)
		field("A recording", "%s (%d clicks, %d dropped)", filepath.Base(tsPathA), len(aliceEv), droppedA)
		field("B recording", "%s (%d clicks, %d dropped)", filepath.Base(tsPathB), len(bobEv), droppedB)
		field("Clock skew (B vs A)", "%.3f ppb", skew.SkewPPB)
		field("Coincidence window", "%d ps", window)
		field("Multi-photon clicks", "A: %d / %d, B: %d / %d (window=%d ps)", flaggedMPA, len(aliceEv), flaggedMPB, len(bobEv), *multiPhotonWindowPS)
		field("GOMAXPROCS", "%d", runtime.GOMAXPROCS(0))
	}

	// same "run configuration" section as above, continued here since Method
	// (set further below) is still part of it visually; no new section() call
	vis := computeVisibilityMetrics(aliceEv, bobEv, window)
	if !*jsonOut {
		field("HV basis coincidences", "HH=%d VV=%d HV=%d VH=%d (sifted=%d)", vis.CoincidenceHH, vis.CoincidenceVV, vis.CoincidenceHV, vis.CoincidenceVH, vis.SiftedHV)
		field("DA basis coincidences", "DD=%d AA=%d DA=%d AD=%d (sifted=%d)", vis.CoincidenceDD, vis.CoincidenceAA, vis.CoincidenceDA, vis.CoincidenceAD, vis.SiftedDA)
		field("Visibility (HV, DA)", "%.4f, %.4f", vis.VisibilityHV, vis.VisibilityDA)
		field("Fidelity (from visibility)", "%.4f", vis.FidelityVis)
		field("QBER (from visibility)", "%.4f", vis.QBERVis)
		field("QBER (HV basis only)", "%.4f", vis.QBERBasisHV)
		field("QBER (DA basis only)", "%.4f", vis.QBERBasisDA)
		printChannelDelays(report.ChannelDelays)
	}

	var windowBuckets []qkd.WindowBucketReport
	if *windowReport {
		pairs := qkd.MatchCoincidences(qkd.StripBit(aliceEv), qkd.StripBit(bobEv), window)
		windowBuckets = qkd.ReportWindowQuality(aliceEv, bobEv, pairs, qkd.WindowReportConfig{
			BucketPS: int64(*windowReportBucketS * 1e12),
			MinPairs: *windowReportMinPairs,
		})
		if !*jsonOut {
			printWindowReport(windowBuckets, *windowReportBucketS)
		}
	}

	alignBits := 0
	if *keystoreDir != "" {
		alignBits = qkd.ETSI014KeySizeBits // no remainder left for SaveKeys256 to discard
	}

	t0 := time.Now()
	var finalKeyBytes []byte
	var rep jsonReport
	if *winnow {
		blockSizes, err := parseIntList(*winnowBlocks)
		if err != nil {
			fail(*jsonOut, "-winnow-blocks: %v", err)
		}
		if !*jsonOut {
			field("Method (step 2/3)", "Winnow correction (blocks=%v, sample=%.2f, chunk=%d)", blockSizes, *winnowSample, *chunkSize)
		}
		finalKeyBytes, rep = runWinnowPipeline(aliceEv, bobEv, window, blockSizes, *winnowSample, *chunkSize, alignBits, *jsonOut)
		rep.Method = "winnow"
	} else if *ldpc {
		if !*jsonOut {
			field("Method (step 2/3)", "LDPC correction (block=%s, wc=%d, wr=%d, sample=%.2f, chunk=%d)",
				ldpcBlockLabel(*ldpcBlock), *ldpcWc, *ldpcWr, *ldpcSample, *chunkSize)
		}
		finalKeyBytes, rep = runLDPCPipeline(aliceEv, bobEv, window, *ldpcBlock, *ldpcWc, *ldpcWr, *ldpcIterations, *ldpcSample, *chunkSize, alignBits, *jsonOut)
		rep.Method = "ldpc"
	} else if *searchMatrix {
		// designed on simulated proxy data only, never the real events
		// above, or the matrix choice itself would leak information about
		// this session's actual secret bits outside the leaked-bits budget
		proxyAlice, proxyBob := simulateOffline(*searchOfflineN, *searchOfflineErr, *searchOfflineMP, *searchOfflineJitter, *searchSeed)
		searched, fitness := qkd.SearchGeneratorMatrix(*searchM, *searchP, proxyAlice, proxyBob, window, *chunkSize, *searchIterations, *searchSeed)
		matrixLabel := fmt.Sprintf("search(%d,%d)", *searchM, *searchP)
		if !*jsonOut {
			field("Method (step 2/3)", "searched matrix (m=%d, p=%d, designed offline at err=%.3f, fitness=%.5f, chunk=%d)",
				*searchM, *searchP, *searchOfflineErr, fitness, *chunkSize)
		}
		finalKeyBytes, rep = runDiscardPipeline(aliceEv, bobEv, window, searched, matrixLabel, *chunkSize, alignBits, *jsonOut)
		rep.Method = "discard"
		rep.SearchFitness = fitness
	} else if *epochS > 0 {
		epochPS := int64(*epochS * 1e12)
		if !*jsonOut {
			field("Method (step 2/3)", "block discard, run per-epoch (matrix=%s, chunk=%d, epoch=%gs)", *matrixName, *chunkSize, *epochS)
		}
		finalKeyBytes, rep = runEpochedPipeline(aliceEv, bobEv, window, g, *matrixName, *chunkSize, alignBits, epochPS, *jsonOut)
		rep.Method = "discard-epoched"
	} else {
		if !*jsonOut {
			field("Method (step 2/3)", "block discard (matrix=%s, chunk=%d)", *matrixName, *chunkSize)
		}
		finalKeyBytes, rep = runDiscardPipeline(aliceEv, bobEv, window, g, *matrixName, *chunkSize, alignBits, *jsonOut)
		rep.Method = "discard"
	}
	elapsed := time.Since(t0)

	rep.KeysSaved = saveToKeystore(*keystoreDir, finalKeyBytes, *keystoreMax, *jsonOut)

	if *jsonOut {
		rep.RecordingDir = *dir
		rep.ARecording = filepath.Base(tsPathA)
		rep.BRecording = filepath.Base(tsPathB)
		rep.ADroppedClicks = droppedA
		rep.BDroppedClicks = droppedB
		rep.AMultiPhotonCount = flaggedMPA
		rep.BMultiPhotonCount = flaggedMPB
		rep.VisibilityHV = vis.VisibilityHV
		rep.VisibilityDA = vis.VisibilityDA
		rep.FidelityVis = vis.FidelityVis
		rep.QBERVis = vis.QBERVis
		rep.QBERBasisHV = vis.QBERBasisHV
		rep.QBERBasisDA = vis.QBERBasisDA
		rep.SkewPPB = skew.SkewPPB
		rep.WindowPS = window
		rep.ElapsedMS = elapsed.Milliseconds()
		if windowBuckets != nil {
			rep.WindowReport = make([]windowBucketJSON, len(windowBuckets))
			for i, b := range windowBuckets {
				rep.WindowReport[i] = windowBucketJSON{
					StartS:     float64(b.StartPS) / 1e12,
					PairCount:  b.PairCount,
					QBER:       b.QBER,
					Reportable: b.Reportable,
				}
			}
		}
		emitJSON(rep)
		return
	}

	section("done")
	field("Wall time", "%s", elapsed)
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

// runDiscardPipeline runs qkd.Run (step 2/3: block discard) and prints the
// same report cmd/qoj_post_processing does; returns the final key bytes, if
// any, plus the structured report -json wants
func runDiscardPipeline(aliceEv, bobEv []qkd.DetectionEvent, window int64, g qkd.GeneratorMatrix, matrixName string, chunkSize, alignBits int, jsonOut bool) ([]byte, jsonReport) {
	cfg := qkd.Config{
		CoincidenceWindowPS: window,
		GeneratorMatrix:     g,
		ChunkSize:           chunkSize,
		KeyAlignBits:        alignBits,
	}
	res, err := qkd.Run(aliceEv, bobEv, cfg)
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
		field("Alice raw clicks", "%d", len(aliceEv))
		field("Bob raw clicks", "%d", len(bobEv))
		field("Sifted bits", "%d", res.SiftedBits)

		blockDiscardFrac := 0.0
		if res.SiftedBits > 0 {
			blockDiscardFrac = float64(len(bd.DiscardedAlice)) / float64(res.SiftedBits)
		}
		section(fmt.Sprintf("(2): error detection (matrix=%s, block=%d bits)", matrixName, g.BlockSize()))
		field("Blocks total", "%d", bd.BlocksTotal)
		field("Blocks kept", "%d", bd.BlocksKept)
		field("Blocks dropped", "%d (%.1f%%)", bd.BlocksDropped, blockDiscardFrac*100)
		if blockDiscardFrac > 0.5 {
			note(colorWarn, "WARN", "more than half the sifted blocks were dropped for a parity mismatch; QBER is likely too high for this link/matrix, expect a short or empty final key")
		}
		field("Bits surviving", "%d", len(bd.SurvivingAlice))
		field("Bits leaked (parity)", "%d", bd.LeakedBits)

		section(fmt.Sprintf("(3): error verification (chunk=%d bits, CRC-16)", chunkSize))
		field("Chunks total", "%d", cv.ChunksTotal)
		field("Chunks kept", "%d", cv.ChunksKept)
		field("Chunks dropped", "%d", cv.ChunksDropped)
		field("Bits surviving", "%d", len(cv.SurvivingAlice))
		field("Bits leaked (CRCs)", "%d", cv.LeakedBits)

		section("(4): QBER calculation")
		field("Sample bits (sifted key)", "%d", res.QBER.SampleBits)
		field("Error bits in faulty blocks", "%d", res.QBER.ErrorBits)
		field("QBER estimate", "%.4f", res.QBER.QBER)

		printSanityAndKey(cv.SurvivingAlice, cv.SurvivingBob, res.LeakedBits, res.FinalKeyBits, res.FinalKeyBytes)
	}

	return res.FinalKeyBytes, jsonReport{
		MatrixLabel:      matrixName,
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
	}
}

// runEpochedPipeline runs qkd.RunEpoched (step 1-5 repeated once per fixed
// time epoch, see epoch.go) instead of pooling the whole recording into one
// QBER estimate, and prints each epoch's own outcome plus the concatenated
// result
func runEpochedPipeline(aliceEv, bobEv []qkd.DetectionEvent, window int64, g qkd.GeneratorMatrix, matrixName string, chunkSize, alignBits int, epochPS int64, jsonOut bool) ([]byte, jsonReport) {
	cfg := qkd.Config{
		CoincidenceWindowPS: window,
		GeneratorMatrix:     g,
		ChunkSize:           chunkSize,
		KeyAlignBits:        alignBits,
	}
	res, err := qkd.RunEpoched(aliceEv, bobEv, cfg, epochPS)
	if err != nil {
		fail(jsonOut, "pipeline error: %v", err)
	}

	var allSurvivingAlice, allSurvivingBob []byte
	totalSifted, totalLeaked, epochsWithKey := 0, 0, 0
	for _, e := range res.Epochs {
		allSurvivingAlice = append(allSurvivingAlice, e.CRCVerify.SurvivingAlice...)
		allSurvivingBob = append(allSurvivingBob, e.CRCVerify.SurvivingBob...)
		totalSifted += e.SiftedBits
		totalLeaked += e.LeakedBits
		if len(e.FinalKeyBits) > 0 {
			epochsWithKey++
		}
	}

	if !jsonOut {
		section(fmt.Sprintf("(1)-(4): per-epoch sifting, error detection, verification, QBER (epoch=%gs, matrix=%s)", float64(epochPS)/1e12, matrixName))
		for _, e := range res.Epochs {
			startS := float64(e.StartPS) / 1e12
			if len(e.FinalKeyBits) > 0 {
				field(fmt.Sprintf("t=%.1fs", startS), "sifted=%d qber=%.4f key_bits=%d", e.SiftedBits, e.QBER.QBER, len(e.FinalKeyBits))
			} else {
				field(fmt.Sprintf("t=%.1fs", startS), "sifted=%d qber=%.4f (no usable key)", e.SiftedBits, e.QBER.QBER)
			}
		}
		field("Epochs total / with key", "%d / %d", len(res.Epochs), epochsWithKey)
		field("Sifted bits (all epochs)", "%d", totalSifted)
		if epochsWithKey == 0 {
			note(colorErr, "ERROR", "no epoch's own honest QBER cleared the security threshold; this recording had no genuinely good moment, not even briefly")
		} else {
			note(colorOK, "OK", fmt.Sprintf("%d of %d epochs independently produced key material", epochsWithKey, len(res.Epochs)))
		}

		printSanityAndKey(allSurvivingAlice, allSurvivingBob, totalLeaked, res.FinalKeyBits, res.FinalKeyBytes)
	}

	avgQBER := 0.0
	if totalSifted > 0 {
		errSum := 0.0
		for _, e := range res.Epochs {
			errSum += e.QBER.QBER * float64(e.SiftedBits)
		}
		avgQBER = errSum / float64(totalSifted)
	}
	rate := 0.0
	if len(allSurvivingAlice) > 0 {
		rate = float64(len(res.FinalKeyBits)) / float64(len(allSurvivingAlice))
	}

	return res.FinalKeyBytes, jsonReport{
		MatrixLabel:     matrixName,
		SiftedBits:      totalSifted,
		QBER:            avgQBER,
		LeakedBitsTotal: totalLeaked,
		FinalKeyBits:    len(res.FinalKeyBits),
		CompressionRate: rate,
	}
}

// printWindowReport prints each time-bucket's own measured QBER: a
// diagnostic for spotting when/where a link's quality drifts, never a
// basis for selecting which bits become key (see windowreport.go for why)
func printWindowReport(buckets []qkd.WindowBucketReport, bucketS float64) {
	section(fmt.Sprintf("(0): time-window QBER report (bucket=%gs, diagnostic only, not used for the key)", bucketS))
	for _, b := range buckets {
		startS := float64(b.StartPS) / 1e12
		if b.Reportable {
			field(fmt.Sprintf("t=%.1fs", startS), "pairs=%d QBER=%.4f", b.PairCount, b.QBER)
		} else {
			field(fmt.Sprintf("t=%.1fs", startS), "pairs=%d (below min-pairs, not reported)", b.PairCount)
		}
	}
}

// runWinnowPipeline runs qkd.RunWinnow (step 2/3: Winnow correction) and
// prints an analogous report, returning the final key bytes, if any, plus
// the structured report -json wants
func runWinnowPipeline(aliceEv, bobEv []qkd.DetectionEvent, window int64, blockSizes []int, sampleFraction float64, chunkSize, alignBits int, jsonOut bool) ([]byte, jsonReport) {
	cfg := qkd.WinnowRunConfig{
		CoincidenceWindowPS: window,
		SampleFraction:      sampleFraction,
		WinnowBlockSizes:    blockSizes,
		Seed:                1,
		ChunkSize:           chunkSize,
		KeyAlignBits:        alignBits,
	}
	res, err := qkd.RunWinnow(aliceEv, bobEv, cfg)
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
		field("Alice raw clicks", "%d", len(aliceEv))
		field("Bob raw clicks", "%d", len(bobEv))
		field("Sifted bits", "%d", res.SiftedBits)

		section("(4 before 2/3): QBER from a sacrificed public sample")
		field("Sample bits", "%d (%.1f%% of sifted)", res.QBER.SampleBits, sampleFraction*100)
		field("Error bits in sample", "%d", res.QBER.ErrorBits)
		field("QBER estimate", "%.4f", res.QBER.QBER)

		section(fmt.Sprintf("(2): Winnow correction (blocks=%v)", blockSizes))
		field("Bits corrected toward Alice", "%d", res.WinnowCorrections)
		field("Bits leaked (syndromes)", "%d", res.WinnowLeakedBits)

		section(fmt.Sprintf("(3): error verification (chunk=%d bits, CRC-16)", chunkSize))
		field("Chunks total", "%d", cv.ChunksTotal)
		field("Chunks kept", "%d", cv.ChunksKept)
		field("Chunks dropped", "%d", cv.ChunksDropped)
		field("Bits surviving", "%d", len(cv.SurvivingAlice))
		field("Bits leaked (CRCs)", "%d", cv.LeakedBits)

		printSanityAndKey(cv.SurvivingAlice, cv.SurvivingBob, res.LeakedBits, res.FinalKeyBits, res.FinalKeyBytes)
	}

	return res.FinalKeyBytes, jsonReport{
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
	}
}

// ldpcBlockLabel: 0 means one block covering the whole remaining key
func ldpcBlockLabel(blockLength int) string {
	if blockLength <= 0 {
		return "auto"
	}
	return strconv.Itoa(blockLength)
}

// runLDPCPipeline runs qkd.RunLDPC (step 2/3: LDPC belief propagation) and
// prints an analogous report. Returns the final key bytes, if any, plus the
// structured report -json wants
func runLDPCPipeline(aliceEv, bobEv []qkd.DetectionEvent, window int64, blockLength, wc, wr, maxIterations int, sampleFraction float64, chunkSize, alignBits int, jsonOut bool) ([]byte, jsonReport) {
	cfg := qkd.LDPCRunConfig{
		CoincidenceWindowPS: window,
		SampleFraction:      sampleFraction,
		BlockLength:         blockLength,
		ColWeight:           wc,
		RowWeight:           wr,
		MaxIterations:       maxIterations,
		Seed:                1,
		ChunkSize:           chunkSize,
		KeyAlignBits:        alignBits,
	}
	res, err := qkd.RunLDPC(aliceEv, bobEv, cfg)
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
		field("Alice raw clicks", "%d", len(aliceEv))
		field("Bob raw clicks", "%d", len(bobEv))
		field("Sifted bits", "%d", res.SiftedBits)

		section("(4 before 2/3): QBER from a sacrificed public sample")
		field("Sample bits", "%d (%.1f%% of sifted)", res.QBER.SampleBits, sampleFraction*100)
		field("Error bits in sample", "%d", res.QBER.ErrorBits)
		field("QBER estimate", "%.4f", res.QBER.QBER)

		section(fmt.Sprintf("(2): LDPC correction (wc=%d, wr=%d, rate=%.3f)", wc, wr, 1-float64(wc)/float64(wr)))
		field("Blocks total", "%d", res.LDPCBlocks)
		field("Blocks converged", "%d / %d", res.LDPCConverged, res.LDPCBlocks)
		if res.LDPCConverged < res.LDPCBlocks {
			note(colorWarn, "WARN", fmt.Sprintf("%d block(s) did not converge within the iteration cap; their bits are likely still wrong and will show up as residual QBER below", res.LDPCBlocks-res.LDPCConverged))
		}
		field("Bits leaked (syndromes)", "%d", res.LDPCLeakedBits)

		section(fmt.Sprintf("(3): error verification (chunk=%d bits, CRC-16)", chunkSize))
		field("Chunks total", "%d", cv.ChunksTotal)
		field("Chunks kept", "%d", cv.ChunksKept)
		field("Chunks dropped", "%d", cv.ChunksDropped)
		field("Bits surviving", "%d", len(cv.SurvivingAlice))
		field("Bits leaked (CRCs)", "%d", cv.LeakedBits)

		printSanityAndKey(cv.SurvivingAlice, cv.SurvivingBob, res.LeakedBits, res.FinalKeyBits, res.FinalKeyBytes)
	}

	return res.FinalKeyBytes, jsonReport{
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
	}
}

// printSanityAndKey prints the shared tail both pipelines share: the
// residual-mismatch sanity check and the step 5 privacy-amplification report
func printSanityAndKey(survivingAlice, survivingBob []byte, leakedBits int, finalKeyBits, finalKeyBytes []byte) {
	residualErr := 0
	for i := range survivingAlice {
		if survivingAlice[i] != survivingBob[i] {
			residualErr++
		}
	}
	field("Sanity check", "%d / %d bits mismatched between the two sides' surviving streams", residualErr, len(survivingAlice))
	if residualErr > 0 {
		note(colorErr, "ERROR", "the error-free streams still disagree; this should never happen and points to a bug in error detection or CRC verification, not a property of the real link")
	} else {
		note(colorOK, "OK", "both sides' surviving streams are bit-identical, as expected")
	}

	rate := 0.0
	if len(survivingAlice) > 0 {
		rate = float64(len(finalKeyBits)) / float64(len(survivingAlice))
	}
	section("(5): privacy amplification")
	field("Input bits (error-free stream)", "%d", len(survivingAlice))
	field("Classical bits leaked (total)", "%d", leakedBits)
	field("Distilled secret key length", "%d bits", len(finalKeyBits))
	field("Compression rate", "%.4f", rate)
	if len(finalKeyBits) > 0 {
		field("Secret key (hex)", "%s", hex.EncodeToString(finalKeyBytes))
		note(colorOK, "OK", "distilled a nonzero secret key from this recording")
	} else {
		field("Secret key", "(empty)")
		note(colorWarn, "WARN", "QBER and/or classical-channel leakage were too high to distil any key at this length; nothing usable was produced from this recording")
	}
}
