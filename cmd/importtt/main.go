// Command importtt runs the BBM92 post-processing chain over a real dual-TT recording
// from acquisition/tt_record_dual.py, aligned to a common clock by acquisition/coincidence_peak.py
package main

import (
	"bufio"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"qkd_post_processing/kms"
	"qkd_post_processing/pyec"
	"qkd_post_processing/qkd"
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
// generator matrices, same set cmd/qkd_post_processing exposes
var generatorMatrices = map[string]qkd.GeneratorMatrix{
	"bch": qkd.GeneratorMatrixBCHt2,
}

// ANSI color codes for status tags, duplicated from cmd/qkd_post_processing so this
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

// note prints a tagged follow-up line under a field, matching the acquisition
// scripts' "[LEVEL] explain why" convention instead of a bare status tag
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

// saveToKeystore mirrors cmd/qkd_post_processing's helper of the same name;
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
		if capBytes := remaining * kms.ETSI014KeySizeBytes; len(finalKeyBytes) > capBytes {
			finalKeyBytes = finalKeyBytes[:capBytes]
		}
	}
	if len(finalKeyBytes) < kms.ETSI014KeySizeBytes {
		if !jsonOut {
			field("Keystore", "skipped")
			note(colorWarn, "WARN", "this run's distilled key is shorter than one ETSI 014 key (256 bits); nothing to save yet")
		}
		return 0
	}
	stored, discardedBits, err := kms.SaveKeys256(keystoreDir, finalKeyBytes)
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

// jsonReport is -json's machine-readable stand-in for the human-readable report;
// same shape as cmd/qkd_post_processing's jsonReport plus this tool's recording fields
type jsonReport struct {
	Method             string             `json:"method"` // discard, discard-epoched, winnow, ldpc, or polar
	RecordingDir       string             `json:"recording_dir"`
	ARecording         string             `json:"a_recording"`
	BRecording         string             `json:"b_recording"`
	ADroppedClicks     int                `json:"a_dropped_clicks"`
	BDroppedClicks     int                `json:"b_dropped_clicks"`
	AMultiPhotonCount  int                `json:"a_multiphoton_count"`
	BMultiPhotonCount  int                `json:"b_multiphoton_count"`
	VisibilityHV       float64            `json:"visibility_hv"`
	VisibilityDA       float64            `json:"visibility_da"`
	FidelityVis        float64            `json:"fidelity_vis"`
	QBERVis            float64            `json:"qber_vis"`
	QBERBasisHV        float64            `json:"qber_bs_hv"`
	QBERBasisDA        float64            `json:"qber_bs_da"`
	SkewPPB            float64            `json:"skew_ppb"`
	WindowPS           int64              `json:"window_ps"`
	MatrixLabel        string             `json:"matrix_label,omitempty"`
	SearchFitness      float64            `json:"search_fitness,omitempty"`
	SiftedBits         int                `json:"sifted_bits"`
	BlocksTotal        int                `json:"blocks_total,omitempty"`
	BlocksKept         int                `json:"blocks_kept,omitempty"`
	BlocksDropped      int                `json:"blocks_dropped,omitempty"`
	BitsLeakedParity   int                `json:"bits_leaked_parity,omitempty"`
	WinnowCorrections  int                `json:"winnow_corrections,omitempty"`
	WinnowLeakedBits   int                `json:"winnow_leaked_bits,omitempty"`
	LDPCBlocks         int                `json:"ldpc_blocks,omitempty"`
	LDPCConverged      int                `json:"ldpc_converged,omitempty"`
	LDPCLeakedBits     int                `json:"ldpc_leaked_bits,omitempty"`
	PolarBlocks        int                `json:"polar_blocks,omitempty"`
	PolarBitsCorrected int                `json:"polar_bits_corrected,omitempty"`
	PolarLeakedBits    int                `json:"polar_leaked_bits,omitempty"`
	LDPC5GRows         int                `json:"ldpc5g_rows,omitempty"`
	LDPC5GFrames       int                `json:"ldpc5g_frames,omitempty"`
	LDPC5GFailed       int                `json:"ldpc5g_failed,omitempty"`
	LDPC5GAttempts     int                `json:"ldpc5g_attempts,omitempty"`
	LDPC5GCorrected    int                `json:"ldpc5g_bits_corrected,omitempty"`
	LDPC5GLeakedBits   int                `json:"ldpc5g_leaked_bits,omitempty"`
	FilterWindowPS     int64              `json:"filter_window_ps,omitempty"`
	FilterAccidental   float64            `json:"filter_predicted_accidental_fraction,omitempty"`
	FilterDriftSlices  int                `json:"filter_drift_slices_trusted,omitempty"`
	FilterSigmaPS      float64            `json:"filter_sigma_ps,omitempty"`
	TimingFlatBound    float64            `json:"timing_flat_bound,omitempty"`
	TimingSoftBound    float64            `json:"timing_soft_bound,omitempty"`
	FiniteKey          *finiteKeyJSON     `json:"finite_key,omitempty"` // nil in asymptotic mode and for block discard
	ChunksTotal        int                `json:"chunks_total"`
	ChunksKept         int                `json:"chunks_kept"`
	ChunksDropped      int                `json:"chunks_dropped"`
	BitsLeakedCRC      int                `json:"bits_leaked_crc"`
	QBER               float64            `json:"qber"`
	LeakedBitsTotal    int                `json:"leaked_bits_total"`
	FinalKeyBits       int                `json:"final_key_bits"`
	CompressionRate    float64            `json:"compression_rate"`
	KeysSaved          int                `json:"keys_saved"`
	ElapsedMS          int64              `json:"elapsed_ms"`
	WindowReport       []windowBucketJSON `json:"window_report,omitempty"`
	WindowSweep        []windowSweepJSON  `json:"window_sweep,omitempty"`
	Error              string             `json:"error,omitempty"`
}

// windowSweepJSON is one -window-sweep width: what sifting and the visibility would give with it
type windowSweepJSON struct {
	WindowPS   int64   `json:"window_ps"`
	SiftedBits int     `json:"sifted_bits"`
	QBERVis    float64 `json:"qber_vis"`
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

// coincidenceReport is the subset of coincidence_peak.py's *_coincidence.json this tool
// needs: which recording was "A"/"B", and the affine clock model mapping B onto A's clock
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

	// Match by basename against the report's recorded inputs; if that fails
	// (files moved, or two explicit files were analyzed), fall back to sorted order
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

// channelToBasisBit maps a physical TimeTagger channel to a BBM92 (basis, bit) pair:
// channels 1-2 are one polarization basis, 3-4 the other; any other channel is dropped
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
	// per-channel index lists in two passes over plain arrays: a map append per click costs more than the scan
	var counts [256]int
	for _, ch := range channels {
		counts[uint8(ch)]++
	}
	var byChannel [256][]int32
	for c, n := range counts {
		if n > 0 {
			byChannel[c] = make([]int32, 0, n)
		}
	}
	for i, ch := range channels {
		byChannel[uint8(ch)] = append(byChannel[uint8(ch)], int32(i))
	}
	// each basis pair once (the relation is symmetric), the pairs in parallel: their clicks never overlap
	var wg sync.WaitGroup
	for c := range byChannel {
		ch := int8(uint8(c))
		partner := basisPartnerChannel(ch)
		idxs, partnerIdxs := byChannel[c], byChannel[uint8(partner)]
		if partner == 0 || partner < ch || len(idxs) == 0 || len(partnerIdxs) == 0 {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
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
		}()
	}
	wg.Wait()
	return flagged
}

// nearMask marks each x within radius of some ref (with refKeep, of some kept ref), in one pass over
// both when they are ascending, as recordings are; out of order, every x is kept and nothing is lost
func nearMask(x, ref []int64, refKeep []bool, radius int64) []bool {
	keep := make([]bool, len(x))
	if !slices.IsSorted(x) || !slices.IsSorted(ref) {
		for i := range keep {
			keep[i] = true
		}
		return keep
	}
	j := 0
	for i, t := range x {
		for j < len(ref) && ref[j] < t-radius {
			j++
		}
		for k := j; k < len(ref) && ref[k] <= t+radius; k++ {
			if refKeep == nil || refKeep[k] {
				keep[i] = true
				break
			}
		}
	}
	return keep
}

// buildEvents maps a recording's arrays to DetectionEvents, applying
// shiftPS first; multiPhoton[i] comes from detectMultiPhoton on raw timestamps
func buildEvents(timestamps []int64, channels []int8, multiPhoton, keep []bool, shiftPS func(int64) int64) (events []qkd.DetectionEvent, dropped, flaggedMP, valid int) {
	for i, ch := range channels {
		if _, _, ok := channelToBasisBit(ch); !ok {
			dropped++
		} else if multiPhoton[i] {
			flaggedMP++
		}
	}
	valid = len(channels) - dropped
	// events only for the clicks keep allows (nil: all), so a busy side costs no more than it can pair
	n := valid
	if keep != nil {
		n = 0
		for _, k := range keep {
			if k {
				n++
			}
		}
	}
	events = make([]qkd.DetectionEvent, 0, n)
	for i, ch := range channels {
		if keep != nil && !keep[i] {
			continue
		}
		if basis, bit, ok := channelToBasisBit(ch); ok {
			events = append(events, qkd.DetectionEvent{TimestampPS: shiftPS(timestamps[i]), Basis: basis, Bit: bit, MultiPhoton: multiPhoton[i]})
		}
	}
	return events, dropped, flaggedMP, valid
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
	dir := flag.String("dir", "", "recording directory (from tt_record_dual.py + coincidence_peak.py); a comma-separated list pools several recordings into one privacy-amplification block, all of them included")
	windowPS := flag.Int64("window", 0, "coincidence window in picoseconds (0 = use the report's corrected window)")
	multiPhotonWindowPS := flag.Int64("multiphoton-window", 2000, "reject a click as an ambiguous multi-photon event if another click lands on its basis partner channel within this many picoseconds of it (own side only, before cross-side matching); 0 disables this check")
	filter := flag.Bool("filter", true, "coincidence filtering from public timing only: track and remove the coincidence peak's drift over the recording, then set the window half-width to the HWHM of a Gaussian fit to the peak (coincfilter.go, doc/coincidence_filtering.md); an explicit -window still overrides the chosen width")
	filterE0 := flag.Float64("filter-e0", 0.03, "declared intrinsic QBER prior used to predict QBER from the fitted peak (and to size the sample under -sample-auto); a public constant, never measured from the key bits")
	filterSliceMS := flag.Int64("filter-slice-ms", 250, "time slice per drift estimate for -filter, in milliseconds")
	calibrateDelays := flag.String("calibrate-delays", "", "measure per-detector delays on these recordings, write them to this JSON file and exit without producing key: it reads detector identities (bit values), so use a dedicated calibration recording, never one used for key")
	detectorDelays := flag.String("detector-delays", "", "JSON file from -calibrate-delays: each party shifts its own clicks by its own detector delay before sifting, so announced timing no longer depends on the bit")
	eventsCache := flag.Bool("events-cache", false, "keep each recording's aligned, pruned events in <dir>/"+eventsCacheName+" (about 1 MB) and reuse them when the parameters match, so the raw files can be deleted after a first run")
	windowSweep := flag.String("window-sweep", "", "comma-separated window half-widths in ps: also report sifted bits and QBER over all sifted pairs for each, from this one load (at most 10000 ps, the prune radius)")
	windowReport := flag.Bool("window-report", false, "print each time-bucket's own measured QBER as a diagnostic; does not select bits for the key (see windowreport.go for why not)")
	windowReportBucketS := flag.Float64("window-report-bucket-s", 0.5, "time-bucket width for -window-report, in seconds")
	windowReportMinPairs := flag.Int("window-report-min-pairs", 10, "buckets with fewer sifted pairs than this report as insufficient data rather than a QBER")
	epochS := flag.Float64("epoch-s", 0, "run the protocol independently once per this many seconds instead of pooling the whole recording into one QBER estimate; each epoch sizes its own privacy amplification from its own measured QBER, so a noisy epoch contributes zero bits rather than being picked after the fact (see epoch.go); 0 disables this; only applies to the default block-discard method, not -winnow/-ldpc/-search-matrix")
	matrixName := flag.String("matrix", "bch", "step 2 generator matrix: bch")
	chunkSize := flag.Int("chunk", 2048, "step 3 CRC chunk size (bits); with -winnow too, the CRC pass after correction")
	keystoreDir := flag.String("keystore", "", "directory to save the final key into (ETSI 014 key_ID/key JSON); empty = don't save")
	keystoreMax := flag.Int("keystore-max", 0, "maximum keys to keep in -keystore, 0 = unlimited; existing keys are never evicted, only this run's own new keys are held back past the cap")
	winnow := flag.Bool("winnow", false, "correct errors with Winnow instead of discarding blocks (see winnow.go/pipeline_winnow.go)")
	winnowBlocks := flag.String("winnow-blocks", "", "comma-separated block size per Winnow pass; empty plans them from the sample QBER: a power of two holding about 0.45 errors, doubling each pass (qkd.WinnowSchedule)")
	winnowSample := flag.Float64("winnow-sample", 0.05, "fraction of sifted bits sacrificed to measure QBER directly before Winnow correction")
	ldpc := flag.Bool("ldpc", false, "correct errors with LDPC belief propagation instead of discarding blocks (see ldpc.go/pipeline_ldpc.go)")
	ldpcBlock := flag.Int("ldpc-block", 0, "LDPC codeword length in bits, 0 = one block covering the whole remaining sifted key")
	ldpcWc := flag.Int("ldpc-wc", 3, "LDPC column weight (variable node degree)")
	ldpcWr := flag.Int("ldpc-wr", 10, "LDPC row weight (check node degree); tune rate=1-wc/wr comfortably below 1-h2(QBER), not just barely under it; a regular code's real decoding threshold runs below the Shannon bound, see ldpc_test.go")
	ldpcIterations := flag.Int("ldpc-iterations", 0, "belief propagation iteration cap per block, 0 = default 50")
	ldpcSample := flag.Float64("ldpc-sample", 0.05, "fraction of sifted bits sacrificed to measure QBER directly before LDPC correction")
	ldpc5g := flag.Bool("ldpc5g", false, "correct errors with 5G NR LDPC (TS 38.212 matrices from py3gpp, belief propagation from the ldpc library, blind reconciliation), the most efficient method; needs ec/requirements.txt")
	polar := flag.Bool("polar", false, "correct errors with polar codes (successive cancellation list decoding) instead of discarding blocks (see polar.go/pipeline_polar.go)")
	polarN := flag.Int("polar-n", 0, "polar codeword length in bits, must be a power of two; 0 = largest power of two covering the remaining sifted key, at most 2^16")
	polarMargin := flag.Float64("polar-margin", 0.02, "starting rate margin below the per-block capacity; on a failed block hash the margin rises by 0.02 and only the extra frozen values are revealed (rate-adaptive)")
	polarSample := flag.Float64("polar-sample", 0.05, "fraction of sifted bits sacrificed to measure QBER directly before polar correction")
	polarList := flag.Int("polar-list", 8, "SCL decoder list size (path count L); 1 = plain successive cancellation, larger L trades decode time for a lower block failure rate")
	polarSeed := flag.Int64("polar-seed", 1, "PRNG seed for polar's QBER sample selection; must be agreed publicly")
	softBins := flag.Int("soft-bins", 0, "with -ldpc or -polar: timing-aware soft reconciliation, per-bit error rates from this many public time-offset bins (doc/timing_aware_reconciliation.md); 0 = one flat QBER")
	searchMatrix := flag.Bool("search-matrix", false, "design a step 2 matrix by searching on simulated data at -search-offline-err, then apply it here, instead of -matrix (see matrixsearch.go)")
	searchM := flag.Int("search-m", 16, "block size for the searched matrix")
	searchP := flag.Int("search-p", 4, "parity bits for the searched matrix")
	searchIterations := flag.Int("search-iterations", 3000, "hill-climbing steps for the search")
	searchSeed := flag.Int64("search-seed", 99, "PRNG seed for the search itself")
	searchOfflineErr := flag.Float64("search-offline-err", 0.03, "target error rate for the simulated proxy data the search designs against; set this to your link's known QBER, never the real recording's own bits")
	searchOfflineN := flag.Int("search-offline-n", 100000, "raw attempts in the simulated proxy dataset used to design the matrix")
	searchOfflineMP := flag.Float64("search-offline-multiphoton", 0.01, "multi-photon rate for the simulated proxy data; match your detector's real rate so the search sees realistic sifting losses")
	searchOfflineJitter := flag.Int64("search-offline-jitter", 300, "detector timing jitter in picoseconds for the simulated proxy data; match your setup so the search sees realistic coincidence-window losses")
	asymptotic := flag.Bool("asymptotic", false, "with -winnow/-ldpc/-polar: size the key by the asymptotic n(1-h2(QBER)) instead of the default finite-key bound (doc/security_proof.md section 2)")
	epsSec := flag.Float64("eps-sec", 1e-10, "finite-key secrecy failure probability per block")
	sampleAuto := flag.Bool("sample-auto", true, "finite-key mode: size the QBER sample to maximize the finite key, from the sifted count and -expected-qber only; overrides -ldpc-sample/-polar-sample/-winnow-sample")
	expectedQBER := flag.Float64("expected-qber", 0, "declared QBER prior for -sample-auto; 0 = the coincidence filter's timing-based prediction, or 0.05 without the filter")
	epsCor := flag.Float64("eps-cor", 1e-15, "finite-key correctness failure probability per block, sets the universal-hash tag length")
	jsonOut := flag.Bool("json", false, "print one JSON metrics object to stdout instead of the human-readable report")
	role := flag.String("role", "", "two-party mode, alice or bob: run one site's half on its own recording, exchanging public messages with -peer through the KMS (-ldpc5g or -polar, see twoparty.go)")
	peer := flag.String("peer", "", "two-party: the neighbor node running the other half")
	session := flag.String("session", "", "two-party: the session name both halves use")
	exportPublic := flag.String("export-public", "", "two-party, alice: write the announcement (timing, basis and multi-photon flag, never bits) into this directory and exit")
	peerTimeout := flag.Duration("peer-timeout", 15*time.Minute, "two-party: how long to wait for each message from the peer")
	cpuProfile := flag.String("cpuprofile", "", "write a CPU profile of this run to this file (go tool pprof)")
	flag.Parse()
	if *cpuProfile != "" {
		if f, err := os.Create(*cpuProfile); err == nil && pprof.StartCPUProfile(f) == nil {
			defer pprof.StopCPUProfile()
		}
	}
	keyOpts = keyOptions{asymptotic: *asymptotic, finite: qkd.FiniteKeyConfig{EpsSec: *epsSec, EpsCor: *epsCor}}

	if *dir == "" {
		if *jsonOut {
			failJSON("-dir is required")
		}
		fmt.Printf("%s -dir is required\n", tag(colorErr, "ERROR"))
		flag.Usage()
		os.Exit(2)
	}
	methods := 0
	for _, m := range []bool{*winnow, *ldpc, *polar, *searchMatrix, *ldpc5g} {
		if m {
			methods++
		}
	}
	if methods > 1 {
		fail(*jsonOut, "-winnow, -ldpc, -polar, -ldpc5g and -search-matrix are mutually exclusive")
	}
	if *epochS > 0 && (*winnow || *ldpc || *polar || *searchMatrix) {
		fail(*jsonOut, "-epoch-s only applies to the default block-discard method, not -winnow/-ldpc/-polar/-search-matrix")
	}
	if *softBins < 0 || (*softBins > 0 && !*ldpc && !*polar && !*ldpc5g) {
		fail(*jsonOut, "-soft-bins must be >= 0 and only applies to -ldpc, -polar or -ldpc5g")
	}

	g, ok := generatorMatrices[*matrixName]
	if !ok {
		fail(*jsonOut, "unknown -matrix %q, expected bch", *matrixName)
	}

	driftCfg := qkd.DriftConfig{SliceDurationPS: *filterSliceMS * 1_000_000_000}
	winCfg := qkd.WindowConfig{IntrinsicQBER: *filterE0}
	var delays *qkd.DetectorDelays
	if *detectorDelays != "" {
		delays = new(qkd.DetectorDelays)
		raw, err := os.ReadFile(*detectorDelays)
		if err == nil {
			err = json.Unmarshal(raw, delays)
		}
		if err != nil {
			fail(*jsonOut, "-detector-delays: %v", err)
		}
	}
	if *role != "" {
		if *winnow || *ldpc || *searchMatrix || *epochS > 0 || *calibrateDelays != "" {
			fail(*jsonOut, "two-party mode runs polar or 5G NR LDPC correction only")
		}
		method := "polar"
		var backend qkd.LDPC5GBackend
		if *ldpc5g && *exportPublic == "" {
			pool, err := pyec.Start(0)
			if err != nil {
				fail(*jsonOut, "%v", err)
			}
			defer pool.Close()
			method, backend = "ldpc5g", pool
		}
		alignBits := 0
		if *keystoreDir != "" {
			alignBits = kms.ETSI014KeySizeBits
		}
		var dirs []string
		for _, d := range strings.Split(*dir, ",") {
			dirs = append(dirs, strings.TrimSpace(d))
		}
		runTwoParty(twoPartyOptions{
			role: *role, peer: *peer, session: *session, exportDir: *exportPublic, keystore: *keystoreDir,
			dirs: dirs, jsonOut: *jsonOut, mpWindowPS: *multiPhotonWindowPS, windowPS: *windowPS,
			filter: *filter, driftCfg: driftCfg, winCfg: winCfg, delays: delays, expectedQBER: *expectedQBER,
			timeout: *peerTimeout,
			cfg: qkd.TwoPartyConfig{RateMargin: *polarMargin, ListSize: *polarList, BlockLength: *polarN,
				ChunkSize: *chunkSize, KeyAlignBits: alignBits, TimingBins: *softBins, SampleShare: *polarSample,
				Asymptotic: keyOpts.asymptotic, FiniteKey: keyOpts.finite, Method: method, LDPC: backend},
		})
		return
	}

	if *ldpc5g && *calibrateDelays == "" {
		ecStarted = startECPool()
	}
	var recs []loadedRecording
	for _, d := range strings.Split(*dir, ",") {
		load := loadRecording
		if *eventsCache {
			load = loadRecordingCached
		}
		r, err := load(strings.TrimSpace(d), *multiPhotonWindowPS, *windowPS, *filter, driftCfg, winCfg, delays)
		if err != nil {
			fail(*jsonOut, "%v", err)
		}
		recs = append(recs, r)
	}
	aliceEv, bobEv := poolRecordings(recs)

	// one window for the pooled block, chosen on the drift-corrected union, same procedure
	var filterResult *qkd.CoincidenceFilter
	if *filter {
		if len(recs) == 1 {
			filterResult = recs[0].filter
		} else {
			corrected, fr, ferr := qkd.FilterCoincidences(aliceEv, bobEv, driftCfg, winCfg)
			if ferr != nil {
				fail(*jsonOut, "coincidence filter on the pooled recordings: %v", ferr)
			}
			bobEv, filterResult = corrected, &fr
		}
	}

	if *sampleAuto && !keyOpts.asymptotic {
		keyOpts.expectedQBER = *expectedQBER
		if keyOpts.expectedQBER <= 0 && filterResult != nil {
			keyOpts.expectedQBER = filterResult.PredictedQBER
		}
		if keyOpts.expectedQBER <= 0 {
			keyOpts.expectedQBER = 0.05
		}
	}

	if *calibrateDelays != "" {
		runDelayCalibration(aliceEv, bobEv, *calibrateDelays, *jsonOut)
		return
	}

	first := recs[0]
	tsPathA, tsPathB, report := first.tsPathA, first.tsPathB, first.report
	skew := report.ClockSkew
	var droppedA, droppedB, flaggedMPA, flaggedMPB, rawAlice int
	for _, r := range recs {
		rawAlice += r.rawAlice
		droppedA += r.droppedA
		droppedB += r.droppedB
		flaggedMPA += r.flaggedMPA
		flaggedMPB += r.flaggedMPB
	}

	window := *windowPS
	if window == 0 && filterResult != nil {
		window = filterResult.WindowPS
	}
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
		if len(recs) == 1 {
			field("Recording directory", "%s", *dir)
			field("A recording", "%s (%d clicks, %d dropped, %d within coincidence range kept)", filepath.Base(tsPathA), rawAlice, droppedA, len(aliceEv))
			field("B recording", "%s (%d clicks, %d dropped, %d within coincidence range kept)", filepath.Base(tsPathB), first.rawBob, droppedB, len(bobEv))
			field("Clock skew (B vs A)", "%.3f ppb", skew.SkewPPB)
		} else {
			field("Pooled recordings", "%d, one privacy-amplification block, all included", len(recs))
			for _, r := range recs {
				field("  "+filepath.Base(r.dir), "A %d clicks (%d in range), B %d clicks (%d in range), skew %.3f ppb, %s", r.rawAlice, len(r.alice), r.rawBob, len(r.bob), r.report.ClockSkew.SkewPPB, recordingFilterLabel(r.filter))
			}
		}
		field("Coincidence window", "+-%d ps", window)
		printCoincidenceFilter(filterResult)
		field("Multi-photon clicks", "A: %d / %d, B: %d / %d (window=%d ps)", flaggedMPA, rawAlice, flaggedMPB, len(bobEv), *multiPhotonWindowPS)
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
		if len(recs) == 1 {
			printChannelDelays(report.ChannelDelays)
		}
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
		alignBits = kms.ETSI014KeySizeBits // no remainder left for SaveKeys256 to discard
	}

	t0 := time.Now()
	var finalKeyBytes []byte
	var rep jsonReport
	if *winnow {
		var blockSizes []int
		var err error
		if *winnowBlocks != "" {
			blockSizes, err = parseIntList(*winnowBlocks)
		}
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
		finalKeyBytes, rep = runLDPCPipeline(aliceEv, bobEv, window, *ldpcBlock, *ldpcWc, *ldpcWr, *ldpcIterations, *ldpcSample, *chunkSize, alignBits, *softBins, *jsonOut)
		rep.Method = "ldpc"
	} else if *polar {
		if !*jsonOut {
			field("Method (step 2/3)", "polar correction (n=%s, margin=%.2f, list=%d, sample=%.2f, chunk=%d)",
				ldpcBlockLabel(*polarN), *polarMargin, *polarList, *polarSample, *chunkSize)
		}
		finalKeyBytes, rep = runPolarPipeline(aliceEv, bobEv, window, *polarN, *polarMargin, *polarList, *polarSeed, *polarSample, *chunkSize, alignBits, *softBins, *jsonOut)
		rep.Method = "polar"
	} else if *ldpc5g {
		if !*jsonOut {
			field("Method (step 2/3)", "5G NR LDPC, blind (sample=%.2f, chunk=%d)", *polarSample, *chunkSize)
		}
		finalKeyBytes, rep = runLDPC5GPipeline(aliceEv, bobEv, window, *polarSeed, *polarSample, *chunkSize, alignBits, *softBins, *jsonOut)
		rep.Method = "ldpc5g"
	} else if *searchMatrix {
		// Designed on simulated proxy data only, never the real events above, or
		// the matrix choice itself would leak secret-bit info outside the leaked-bits budget
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

	if *windowSweep != "" {
		widths, err := parseIntList(*windowSweep)
		if err != nil {
			fail(*jsonOut, "-window-sweep: %v", err)
		}
		// windows apart, the same events every -window run would sift: the drift correction ignores the window
		// and the prune radius covers twice any width up to 10 ns
		rep.WindowSweep = make([]windowSweepJSON, len(widths))
		var wg sync.WaitGroup
		for i, w := range widths {
			if w <= 0 || w > 10_000 {
				fail(*jsonOut, "-window-sweep: %d ps is outside 1 to 10000", w)
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				rep.WindowSweep[i] = windowSweepJSON{WindowPS: int64(w), SiftedBits: len(qkd.Sift(aliceEv, bobEv, int64(w))),
					QBERVis: computeVisibilityMetrics(aliceEv, bobEv, int64(w)).QBERVis}
			}()
		}
		wg.Wait()
	}

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
		if filterResult != nil {
			rep.FilterWindowPS = filterResult.WindowPS
			rep.FilterAccidental = filterResult.AccidentalFraction
			rep.FilterDriftSlices = filterResult.DriftTrusted
			rep.FilterSigmaPS = filterResult.Fit.SigmaPS
		}
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

// runDiscardPipeline runs qkd.Run (step 2/3: block discard) and prints the same
// report cmd/qkd_post_processing does; returns final key bytes plus the -json report
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
		field("Alice clicks in coincidence range", "%d", len(aliceEv))
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

		field("Key length bound", "asymptotic n(1-h2(QBER)) - leaked; block discard has no random sample to bound the phase error")
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

// runEpochedPipeline runs qkd.RunEpoched (step 1-5 repeated once per fixed time
// epoch, see epoch.go) instead of pooling the whole recording into one QBER estimate
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

// printWindowReport prints each time-bucket's measured QBER: a diagnostic for spotting
// drift, never a basis for selecting which bits become key (see windowreport.go for why)
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

// runWinnowPipeline runs qkd.RunWinnow (step 2/3: Winnow correction) and prints
// an analogous report, returning the final key bytes plus the -json report
func runWinnowPipeline(aliceEv, bobEv []qkd.DetectionEvent, window int64, blockSizes []int, sampleFraction float64, chunkSize, alignBits int, jsonOut bool) ([]byte, jsonReport) {
	cfg := qkd.WinnowRunConfig{
		CoincidenceWindowPS: window,
		SampleFraction:      sampleFraction,
		WinnowBlockSizes:    blockSizes,
		Seed:                1,
		ChunkSize:           chunkSize,
		KeyAlignBits:        alignBits,
		Asymptotic:          keyOpts.asymptotic,
		FiniteKey:           keyOpts.finite,
		ExpectedQBER:        keyOpts.expectedQBER,
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
		field("Alice clicks in coincidence range", "%d", len(aliceEv))
		field("Bob raw clicks", "%d", len(bobEv))
		field("Sifted bits", "%d", res.SiftedBits)

		section("(4 before 2/3): QBER from a sacrificed public sample")
		field("Sample bits", "%d (%.1f%% of sifted)", res.QBER.SampleBits, 100*float64(res.QBER.SampleBits)/float64(max(1, res.SiftedBits)))
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

		printFiniteKey(res.FiniteKey)
		printSanityAndKey(cv.SurvivingAlice, cv.SurvivingBob, res.LeakedBits, res.FinalKeyBits, res.FinalKeyBytes)
	}

	return res.FinalKeyBytes, jsonReport{
		SiftedBits:        res.SiftedBits,
		WinnowCorrections: res.WinnowCorrections,
		WinnowLeakedBits:  res.WinnowLeakedBits,
		FiniteKey:         toFiniteKeyJSON(res.FiniteKey),
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

// runLDPCPipeline runs qkd.RunLDPC (step 2/3: LDPC belief propagation) and prints
// an analogous report. Returns the final key bytes plus the -json report
func runLDPCPipeline(aliceEv, bobEv []qkd.DetectionEvent, window int64, blockLength, wc, wr, maxIterations int, sampleFraction float64, chunkSize, alignBits, timingBins int, jsonOut bool) ([]byte, jsonReport) {
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
		TimingBins:          timingBins,
		Asymptotic:          keyOpts.asymptotic,
		FiniteKey:           keyOpts.finite,
		ExpectedQBER:        keyOpts.expectedQBER,
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
		field("Alice clicks in coincidence range", "%d", len(aliceEv))
		field("Bob raw clicks", "%d", len(bobEv))
		field("Sifted bits", "%d", res.SiftedBits)

		section("(4 before 2/3): QBER from a sacrificed public sample")
		field("Sample bits", "%d (%.1f%% of sifted)", res.QBER.SampleBits, 100*float64(res.QBER.SampleBits)/float64(max(1, res.SiftedBits)))
		field("Error bits in sample", "%d", res.QBER.ErrorBits)
		field("QBER estimate", "%.4f", res.QBER.QBER)

		section(fmt.Sprintf("(2): LDPC correction (wc=%d, wr=%d, rate=%.3f)", wc, wr, 1-float64(wc)/float64(wr)))
		field("Blocks total", "%d", res.LDPCBlocks)
		field("Blocks converged", "%d / %d", res.LDPCConverged, res.LDPCBlocks)
		if res.LDPCConverged < res.LDPCBlocks {
			note(colorWarn, "WARN", fmt.Sprintf("%d block(s) did not converge within the iteration cap; their bits are likely still wrong and will show up as residual QBER below", res.LDPCBlocks-res.LDPCConverged))
		}
		field("Bits leaked (syndromes)", "%d", res.LDPCLeakedBits)
		printTimingReport(res.Timing)

		section(fmt.Sprintf("(3): error verification (chunk=%d bits, CRC-16)", chunkSize))
		field("Chunks total", "%d", cv.ChunksTotal)
		field("Chunks kept", "%d", cv.ChunksKept)
		field("Chunks dropped", "%d", cv.ChunksDropped)
		field("Bits surviving", "%d", len(cv.SurvivingAlice))
		field("Bits leaked (CRCs)", "%d", cv.LeakedBits)

		printFiniteKey(res.FiniteKey)
		printSanityAndKey(cv.SurvivingAlice, cv.SurvivingBob, res.LeakedBits, res.FinalKeyBits, res.FinalKeyBytes)
	}

	return res.FinalKeyBytes, jsonReport{
		SiftedBits:      res.SiftedBits,
		LDPCBlocks:      res.LDPCBlocks,
		LDPCConverged:   res.LDPCConverged,
		LDPCLeakedBits:  res.LDPCLeakedBits,
		TimingFlatBound: timingFlat(res.Timing),
		TimingSoftBound: timingSoft(res.Timing),
		FiniteKey:       toFiniteKeyJSON(res.FiniteKey),
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

// runPolarPipeline runs qkd.RunPolar (step 2/3: polar SC/SCL decoding) and prints
// an analogous report. Returns the final key bytes plus the -json report
func runPolarPipeline(aliceEv, bobEv []qkd.DetectionEvent, window int64, blockLength int, margin float64, listSize int, seed int64, sampleFraction float64, chunkSize, alignBits, timingBins int, jsonOut bool) ([]byte, jsonReport) {
	cfg := qkd.PolarRunConfig{
		CoincidenceWindowPS: window,
		SampleFraction:      sampleFraction,
		BlockLength:         blockLength,
		RateMargin:          margin,
		ListSize:            listSize,
		Seed:                seed,
		ChunkSize:           chunkSize,
		KeyAlignBits:        alignBits,
		TimingBins:          timingBins,
		Asymptotic:          keyOpts.asymptotic,
		FiniteKey:           keyOpts.finite,
		ExpectedQBER:        keyOpts.expectedQBER,
	}
	res, err := qkd.RunPolar(aliceEv, bobEv, cfg)
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
		field("Alice clicks in coincidence range", "%d", len(aliceEv))
		field("Bob raw clicks", "%d", len(bobEv))
		field("Sifted bits", "%d", res.SiftedBits)

		section("(4 before 2/3): QBER from a sacrificed public sample")
		field("Sample bits", "%d (%.1f%% of sifted)", res.QBER.SampleBits, 100*float64(res.QBER.SampleBits)/float64(max(1, res.SiftedBits)))
		field("Error bits in sample", "%d", res.QBER.ErrorBits)
		field("QBER estimate", "%.4f", res.QBER.QBER)

		section(fmt.Sprintf("(2): polar correction (list=%d)", listSize))
		field("Blocks total", "%d", res.PolarBlocks)
		field("Bits corrected toward Alice", "%d", res.PolarBitsCorrected)
		field("Bits leaked (frozen reveals)", "%d", res.PolarLeakedBits)
		printTimingReport(res.Timing)

		section(fmt.Sprintf("(3): error verification (chunk=%d bits, CRC-16)", chunkSize))
		field("Chunks total", "%d", cv.ChunksTotal)
		field("Chunks kept", "%d", cv.ChunksKept)
		field("Chunks dropped", "%d", cv.ChunksDropped)
		field("Bits surviving", "%d", len(cv.SurvivingAlice))
		field("Bits leaked (CRCs)", "%d", cv.LeakedBits)

		printFiniteKey(res.FiniteKey)
		printSanityAndKey(cv.SurvivingAlice, cv.SurvivingBob, res.LeakedBits, res.FinalKeyBits, res.FinalKeyBytes)
	}

	return res.FinalKeyBytes, jsonReport{
		SiftedBits:         res.SiftedBits,
		PolarBlocks:        res.PolarBlocks,
		PolarBitsCorrected: res.PolarBitsCorrected,
		PolarLeakedBits:    res.PolarLeakedBits,
		TimingFlatBound:    timingFlat(res.Timing),
		TimingSoftBound:    timingSoft(res.Timing),
		FiniteKey:          toFiniteKeyJSON(res.FiniteKey),
		ChunksTotal:        cv.ChunksTotal,
		ChunksKept:         cv.ChunksKept,
		ChunksDropped:      cv.ChunksDropped,
		BitsLeakedCRC:      cv.LeakedBits,
		QBER:               res.QBER.QBER,
		LeakedBitsTotal:    res.LeakedBits,
		FinalKeyBits:       len(res.FinalKeyBits),
		CompressionRate:    rate,
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

// printTimingReport shows the timing model's disclosure bounds when soft reconciliation ran
func printTimingReport(t *qkd.TimingReport) {
	if t == nil {
		return
	}
	field("Timing model", "%d bins over +-%d ps (from the QBER sample)", len(t.Model.ErrorRate), t.Model.WindowPS)
	saving := 0.0
	if t.FlatBound > 0 {
		saving = 100 * (t.FlatBound - t.SoftBound) / t.FlatBound
	}
	field("Disclosure bound", "flat %.4f, timing-aware %.4f bits/bit (%.1f%% less)", t.FlatBound, t.SoftBound, saving)
}

// timingFlat and timingSoft read a possibly-nil TimingReport for the -json output
func timingFlat(t *qkd.TimingReport) float64 {
	if t == nil {
		return 0
	}
	return t.FlatBound
}

func timingSoft(t *qkd.TimingReport) float64 {
	if t == nil {
		return 0
	}
	return t.SoftBound
}

// printCoincidenceFilter reports the drift tracking and window choice, all from public timing
func printCoincidenceFilter(f *qkd.CoincidenceFilter) {
	if f == nil {
		field("Coincidence filter", "off")
		return
	}
	d := f.Drift
	span := 0.0
	if n := len(d.OffsetsPS); n > 0 {
		lo, hi := d.OffsetsPS[0], d.OffsetsPS[0]
		for _, o := range d.OffsetsPS {
			lo, hi = min(lo, o), max(hi, o)
		}
		span = hi - lo
	}
	field("Drift tracking", "peak moved over %.0f ps, removed in %d pass(es), %d/%d slices trusted", span, f.DriftPasses, f.DriftTrusted, d.Slices)
	field("Gaussian fit (after drift)", "sigma %.1f ps, centre %+.1f ps, peak %.0f over floor %.1f per %d ps bin", f.Fit.SigmaPS, f.Fit.CentrePS, f.Fit.Amplitude, f.Fit.Background, f.Fit.BinPS)
	field("Window (fit HWHM)", "+-%d ps, predicted accidentals %.1f%%", f.WindowPS, 100*f.AccidentalFraction)
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

// loadedRecording is one recording's events on Alice's clock, drift-filtered if asked
type loadedRecording struct {
	dir                    string
	tsPathA, tsPathB       string
	report                 coincidenceReport
	alice, bob             []qkd.DetectionEvent
	rawAlice, rawBob       int // clicks before PruneToCoincidenceRange
	droppedA, droppedB     int
	flaggedMPA, flaggedMPB int
	filter                 *qkd.CoincidenceFilter
}

// eventsCacheName holds one recording's aligned, pruned and filtered events, about 1 MB where the raw
// arrays take hundreds, so a continuously recorded segment can drop its raw files once processed
const eventsCacheName = "events.cache"

type recordingCache struct {
	Key                    string
	TsPathA, TsPathB       string
	Report                 coincidenceReport
	Alice, Bob             []qkd.DetectionEvent
	RawAlice, RawBob       int
	DroppedA, DroppedB     int
	FlaggedMPA, FlaggedMPB int
	Filter                 *qkd.CoincidenceFilter
}

func cacheKey(mpWindowPS, windowPS int64, filter bool, driftCfg qkd.DriftConfig, winCfg qkd.WindowConfig, delays *qkd.DetectorDelays) string {
	d := qkd.DetectorDelays{}
	if delays != nil {
		d = *delays
	}
	return fmt.Sprintf("v1 mp=%d window=%d filter=%t drift=%+v win=%+v delays=%+v", mpWindowPS, windowPS, filter, driftCfg, winCfg, d)
}

// loadRecordingCached reads dir's events cache when it was made with these parameters, else loads the
// recording and writes the cache; a recording whose raw files are gone needs a matching cache
func loadRecordingCached(dir string, mpWindowPS, windowPS int64, filter bool, driftCfg qkd.DriftConfig, winCfg qkd.WindowConfig, delays *qkd.DetectorDelays) (loadedRecording, error) {
	key := cacheKey(mpWindowPS, windowPS, filter, driftCfg, winCfg, delays)
	path := filepath.Join(dir, eventsCacheName)
	if f, err := os.Open(path); err == nil {
		var c recordingCache
		err = gob.NewDecoder(bufio.NewReader(f)).Decode(&c)
		f.Close()
		if err == nil && c.Key == key {
			return loadedRecording{dir: dir, tsPathA: c.TsPathA, tsPathB: c.TsPathB, report: c.Report, alice: c.Alice, bob: c.Bob,
				rawAlice: c.RawAlice, rawBob: c.RawBob, droppedA: c.DroppedA, droppedB: c.DroppedB,
				flaggedMPA: c.FlaggedMPA, flaggedMPB: c.FlaggedMPB, filter: c.Filter}, nil
		}
	}
	r, err := loadRecording(dir, mpWindowPS, windowPS, filter, driftCfg, winCfg, delays)
	if err != nil {
		return r, err
	}
	c := recordingCache{Key: key, TsPathA: r.tsPathA, TsPathB: r.tsPathB, Report: r.report, Alice: r.alice, Bob: r.bob,
		RawAlice: r.rawAlice, RawBob: r.rawBob, DroppedA: r.droppedA, DroppedB: r.droppedB,
		FlaggedMPA: r.flaggedMPA, FlaggedMPB: r.flaggedMPB, Filter: r.filter}
	// written beside and renamed, so a reader never sees half a cache
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return r, fmt.Errorf("%s: events cache: %w", dir, err)
	}
	w := bufio.NewWriter(f)
	err = gob.NewEncoder(w).Encode(c)
	if err == nil {
		err = w.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return r, fmt.Errorf("%s: events cache: %w", dir, err)
	}
	return r, nil
}

// loadRecording finds, loads and clock-aligns one recording, flags multi-photon clicks,
// and applies the coincidence filter on its own, since each recording has its own drift
func loadRecording(dir string, mpWindowPS, windowPS int64, filter bool, driftCfg qkd.DriftConfig, winCfg qkd.WindowConfig, delays *qkd.DetectorDelays) (loadedRecording, error) {
	r := loadedRecording{dir: dir}
	var err error
	r.tsPathA, r.tsPathB, r.report, err = findRecording(dir)
	if err != nil {
		return r, err
	}
	tsA, err := loadNpyInt64(r.tsPathA)
	if err != nil {
		return r, err
	}
	chA, err := loadNpyInt8(channelPath(r.tsPathA))
	if err != nil {
		return r, err
	}
	tsB, err := loadNpyInt64(r.tsPathB)
	if err != nil {
		return r, err
	}
	chB, err := loadNpyInt8(channelPath(r.tsPathB))
	if err != nil {
		return r, err
	}
	if len(tsA) != len(chA) || len(tsB) != len(chB) {
		return r, fmt.Errorf("%s: timestamp and channel counts differ (A %d/%d, B %d/%d)", dir, len(tsA), len(chA), len(tsB), len(chB))
	}
	if len(tsA) == 0 || len(tsB) == 0 {
		return r, fmt.Errorf("%s: empty recording", dir)
	}

	// Alignment from coincidence_peak.py's report (delay is B - A): maps a B timestamp onto
	// A's clock, correcting the constant lag and the fitted clock skew
	a0, b0 := tsA[0], tsB[0]
	skew := r.report.ClockSkew
	toAliceClock := func(ts int64) int64 {
		bRel := float64(ts - b0)
		aRel := skew.ReferenceElapsedPS + (bRel-(skew.ReferenceElapsedPS+skew.LagAtReferencePS))/(1+skew.SkewFraction)
		return a0 + int64(math.Round(aRel))
	}
	// own-side timing check, run before any cross-side alignment or shifting, both sides at once
	var mpA, mpB []bool
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); mpA = detectMultiPhoton(tsA, chA, mpWindowPS) }()
	go func() { defer wg.Done(); mpB = detectMultiPhoton(tsB, chB, mpWindowPS) }()
	wg.Wait()

	// one side often records ~50x the other's clicks, nearly all unpaired: on the raw arrays, keep only
	// clicks that could coincide, on both sides, since either can be the busy one; the margin covers
	// detector delays, applied later, so the exact prune below sees every click it would have kept
	radius := pruneRadiusPS(windowPS, r.report)
	margin := radius + maxDelayPS(delays)
	bOnA := make([]int64, len(tsB))
	for i, t := range tsB {
		bOnA[i] = toAliceClock(t)
	}
	keepB := nearMask(bOnA, tsA, nil, margin)
	keepA := nearMask(tsA, bOnA, keepB, margin)
	r.alice, r.droppedA, r.flaggedMPA, r.rawAlice = buildEvents(tsA, chA, mpA, keepA, func(ts int64) int64 { return ts })
	r.bob, r.droppedB, r.flaggedMPB, r.rawBob = buildEvents(tsB, chB, mpB, keepB, toAliceClock)
	if delays != nil {
		r.alice = qkd.ApplyDetectorDelays(r.alice, delays.Alice)
		r.bob = qkd.ApplyDetectorDelays(r.bob, delays.Bob)
	}
	r.bob = qkd.PruneToCoincidenceRange(r.alice, r.bob, radius)
	r.alice = qkd.PruneToCoincidenceRange(r.bob, r.alice, radius)

	if filter {
		corrected, fr, ferr := qkd.FilterCoincidences(r.alice, r.bob, driftCfg, winCfg)
		if ferr != nil {
			return r, fmt.Errorf("%s: coincidence filter: %v (rerun with -filter=false to skip it)", dir, ferr)
		}
		r.bob, r.filter = corrected, &fr
	}
	return r, nil
}

// poolRecordingGapPS separates consecutive recordings on the pooled timeline, far beyond any window
const poolRecordingGapPS = 1_000_000_000_000

// poolRecordings lays recordings end to end on one timeline, so they sift and privacy-amplify as
// one block; every recording given is included, none is chosen by its error rate
func poolRecordings(recs []loadedRecording) (alice, bob []qkd.DetectionEvent) {
	if len(recs) == 1 {
		return recs[0].alice, recs[0].bob
	}
	var cursor int64
	for _, r := range recs {
		lo, hi := int64(math.MaxInt64), int64(math.MinInt64)
		for _, evs := range [][]qkd.DetectionEvent{r.alice, r.bob} {
			for _, e := range evs {
				lo, hi = min(lo, e.TimestampPS), max(hi, e.TimestampPS)
			}
		}
		shift := cursor - lo
		for _, e := range r.alice {
			e.TimestampPS += shift
			alice = append(alice, e)
		}
		for _, e := range r.bob {
			e.TimestampPS += shift
			bob = append(bob, e)
		}
		cursor = hi + shift + poolRecordingGapPS
	}
	return alice, bob
}

// recordingFilterLabel summarizes one recording's drift filter for the pooled header
func recordingFilterLabel(f *qkd.CoincidenceFilter) string {
	if f == nil {
		return "filter off"
	}
	return fmt.Sprintf("drift %d/%d slices in %d pass(es), fit sigma %.0f ps, HWHM +-%d ps", f.DriftTrusted, f.Drift.Slices, f.DriftPasses, f.Fit.SigmaPS, f.WindowPS)
}

// maxDelayPS is the largest detector-delay shift, so a prune before the delays are applied can widen by it
func maxDelayPS(d *qkd.DetectorDelays) int64 {
	if d == nil {
		return 0
	}
	m := 0.0
	for _, side := range [][2][2]float64{d.Alice, d.Bob} {
		for _, b := range side {
			m = max(m, math.Abs(b[0]), math.Abs(b[1]))
		}
	}
	return int64(math.Ceil(m)) + 1
}

// pruneRadiusPS is how far from any Alice click a Bob click is kept: 20 ns, or twice the
// widest window this run could use, so pruning never removes a possible coincidence
func pruneRadiusPS(windowFlagPS int64, report coincidenceReport) int64 {
	return max(20_000, 2*windowFlagPS, 2*report.ClockSkew.CorrectedWindow.WidthPS)
}

// calibrationRadiusPS bounds each detector class's peak, several sigma of a ~30 ps peak
const calibrationRadiusPS = 150

// runDelayCalibration measures per-detector delays on drift-corrected events, writes them,
// and reports the timing leak before and after applying them
func runDelayCalibration(alice, bob []qkd.DetectionEvent, outPath string, jsonOut bool) {
	delays, before, err := qkd.CalibrateDetectorDelays(alice, bob, calibrationRadiusPS)
	if err != nil {
		fail(jsonOut, "%v", err)
	}
	after, afterPeaks, err := qkd.CalibrateDetectorDelays(qkd.ApplyDetectorDelays(alice, delays.Alice), qkd.ApplyDetectorDelays(bob, delays.Bob), calibrationRadiusPS)
	if err != nil {
		fail(jsonOut, "%v", err)
	}
	raw, _ := json.MarshalIndent(delays, "", "  ")
	if err := os.WriteFile(outPath, raw, 0o644); err != nil {
		fail(jsonOut, "write %s: %v", outPath, err)
	}
	if jsonOut {
		fmt.Println(string(raw))
		return
	}
	section("detector delay calibration (reads bit values: never use these recordings for key)")
	for i, p := range before {
		field(fmt.Sprintf("basis %d, Alice bit %d, Bob bit %d", p.Basis, p.AliceBit, p.BobBit),
			"%5d pairs, centre %+6.1f ps -> %+6.1f ps after correction", p.Count, p.CentrePS, afterPeaks[i].CentrePS)
	}
	field("Timing leak I(bit; dt)", "%.4f bits per sifted bit before, %.4f after", qkd.TimingLeakBits(before), qkd.TimingLeakBits(afterPeaks))
	field("Residual delays after one pass", "Alice %v, Bob %v ps", roundDelays(after.Alice), roundDelays(after.Bob))
	field("Written", "%s (apply with -detector-delays, or set as per-channel delays in the TimeTaggers)", outPath)
}

func roundDelays(d [2][2]float64) [2][2]float64 {
	for i := range 2 {
		for j := range 2 {
			d[i][j] = math.Round(d[i][j]*10) / 10
		}
	}
	return d
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

// ecPool starts the 5G NR LDPC service in the background, so its imports overlap loading the recording
type ecPool struct {
	done chan struct{}
	pool *pyec.Pool
	err  error
}

func startECPool() *ecPool {
	p := &ecPool{done: make(chan struct{})}
	go func() {
		defer close(p.done)
		p.pool, p.err = pyec.Start(0)
	}()
	return p
}

func (p *ecPool) wait() (*pyec.Pool, error) {
	<-p.done
	return p.pool, p.err
}

var ecStarted *ecPool

func runLDPC5GPipeline(aliceEv, bobEv []qkd.DetectionEvent, window, seed int64, sampleFraction float64, chunkSize, alignBits, timingBins int, jsonOut bool) ([]byte, jsonReport) {
	if ecStarted == nil {
		ecStarted = startECPool()
	}
	pool, err := ecStarted.wait()
	if err != nil {
		fail(jsonOut, "%v", err)
	}
	defer pool.Close()
	res, err := qkd.RunLDPC5G(aliceEv, bobEv, qkd.LDPC5GRunConfig{
		Backend: pool, CoincidenceWindowPS: window, SampleFraction: sampleFraction, Seed: seed, ChunkSize: chunkSize,
		KeyAlignBits: alignBits, TimingBins: timingBins, Asymptotic: keyOpts.asymptotic, FiniteKey: keyOpts.finite,
		ExpectedQBER: keyOpts.expectedQBER,
	})
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
		field("Sample bits", "%d", res.QBER.SampleBits)
		field("QBER estimate", "%.4f", res.QBER.QBER)
		section("(2): 5G NR LDPC, blind reconciliation")
		field("Code", "base graph 1, %d rows, Z=384, %d key bits per frame, puncturing credit %v", res.LDPC.M, res.LDPC.FrameKeyBits, res.LDPC.Credit)
		field("Frames", "%d (%d failed), %d attempts, %d punctured values revealed", res.LDPC.Frames, res.LDPC.Failed, res.LDPC.Attempts, res.LDPC.Revealed)
		field("Bits corrected toward Alice", "%d", res.LDPC.BitsCorrected)
		field("Bits leaked", "%d", res.LDPC.LeakedBits)
		printTimingReport(res.Timing)
		section(fmt.Sprintf("(3): error verification (chunk=%d bits, CRC-16)", chunkSize))
		field("Chunks kept", "%d / %d", cv.ChunksKept, cv.ChunksTotal)
		printFiniteKey(res.FiniteKey)
		printSanityAndKey(cv.SurvivingAlice, cv.SurvivingBob, res.LeakedBits, res.FinalKeyBits, res.FinalKeyBytes)
	}
	return res.FinalKeyBytes, jsonReport{
		SiftedBits: res.SiftedBits, LDPC5GRows: res.LDPC.M, LDPC5GFrames: res.LDPC.Frames, LDPC5GFailed: res.LDPC.Failed,
		LDPC5GAttempts: res.LDPC.Attempts, LDPC5GCorrected: res.LDPC.BitsCorrected, LDPC5GLeakedBits: res.LDPC.LeakedBits,
		TimingFlatBound: timingFlat(res.Timing), TimingSoftBound: timingSoft(res.Timing), FiniteKey: toFiniteKeyJSON(res.FiniteKey),
		ChunksTotal: cv.ChunksTotal, ChunksKept: cv.ChunksKept, ChunksDropped: cv.ChunksDropped, BitsLeakedCRC: cv.LeakedBits,
		QBER: res.QBER.QBER, LeakedBitsTotal: res.LeakedBits, FinalKeyBits: len(res.FinalKeyBits), CompressionRate: rate,
	}
}
