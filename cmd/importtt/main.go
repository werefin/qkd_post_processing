// Command importtt runs the BBM92 post-processing chain over a real dual-TT recording
// from acquisition/tt_record_dual.py, aligned to a common clock by acquisition/coincidence_peak.py
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/bits"
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
	"qkd_post_processing/qkd"
)

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
	Method             string             `json:"method"` // cascade
	RecordingDir       string             `json:"recording_dir"`
	ARecording         string             `json:"a_recording"`
	BRecording         string             `json:"b_recording"`
	ADroppedClicks     int                `json:"a_dropped_clicks"`
	BDroppedClicks     int                `json:"b_dropped_clicks"`
	ASquashedCount     int                `json:"a_squashed_count"` // double clicks made one detection
	BSquashedCount     int                `json:"b_squashed_count"`
	VisibilityHV       float64            `json:"visibility_hv"`
	VisibilityDA       float64            `json:"visibility_da"`
	FidelityVis        float64            `json:"fidelity_vis"`
	QBERVis            float64            `json:"qber_vis"`
	QBERBasisHV        float64            `json:"qber_bs_hv"`
	QBERBasisDA        float64            `json:"qber_bs_da"`
	SkewPPB            float64            `json:"skew_ppb"`
	WindowPS           int64              `json:"window_ps"`
	SiftedBits         int                `json:"sifted_bits"`
	CascadeParities    int                `json:"cascade_parities,omitempty"`
	CascadeCorrections int                `json:"cascade_corrections,omitempty"`
	CascadeBlocks      []int              `json:"cascade_block_sizes,omitempty"`
	FilterWindowPS     int64              `json:"filter_window_ps,omitempty"`
	FilterAccidental   float64            `json:"filter_predicted_accidental_fraction,omitempty"`
	FilterDriftSlices  int                `json:"filter_drift_slices_trusted,omitempty"`
	FilterSigmaPS      float64            `json:"filter_sigma_ps,omitempty"`
	FiniteKey          *finiteKeyJSON     `json:"finite_key,omitempty"` // nil in asymptotic mode
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

// squashRandom gives each squashing its random choices; tests make them repeatable
var squashRandom = func() io.Reader { return rand.Reader }

// squashClicks makes one detection of each group of measurement clicks within windowPS of its first (squashing):
// one channel keeps it, one basis gets a random bit, both bases a random basis and bit; timestamps ascending
func squashClicks(timestamps []int64, channels []int8, windowPS int64, random io.Reader) (squashed []int8, groups int, err error) {
	// squashed[i] is the outcome channel of a group's first click, 0 for the rest and for other channels
	squashed = make([]int8, len(channels))
	rnd := bufio.NewReader(random)
	first, size := -1, 0
	var mask byte
	settle := func() error {
		if size > 1 {
			groups++
		}
		if bits.OnesCount8(mask) == 1 {
			squashed[first] = int8(bits.TrailingZeros8(mask)) + 1
			return nil
		}
		b, err := rnd.ReadByte()
		if err != nil {
			return fmt.Errorf("squashing: %w", err)
		}
		switch {
		case mask&0b1100 == 0:
			squashed[first] = 1 + int8(b&1)
		case mask&0b0011 == 0:
			squashed[first] = 3 + int8(b&1)
		default:
			squashed[first] = 1 + int8(b&3)
		}
		return nil
	}
	for i, ch := range channels {
		if ch < 1 || ch > 4 {
			continue
		}
		if t := timestamps[i]; first >= 0 && t >= timestamps[first] && t-timestamps[first] <= windowPS {
			mask |= 1 << (ch - 1)
			size++
			continue
		}
		if first >= 0 {
			if err := settle(); err != nil {
				return nil, 0, err
			}
		}
		first, mask, size = i, 1<<(ch-1), 1
	}
	if first >= 0 {
		if err := settle(); err != nil {
			return nil, 0, err
		}
	}
	return squashed, groups, nil
}

// delayOf is the delay of the detector that fired, whatever outcome squashing gave the click, so the announced
// time says nothing of a random bit; 0 without delays
func delayOf(delays *[2][2]float64, ch int8) int64 {
	basis, bit, ok := channelToBasisBit(ch)
	if delays == nil || !ok {
		return 0
	}
	return int64(math.Round(delays[basis][bit]))
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

// buildEvents maps a recording's squashed clicks (squashClicks) to DetectionEvents, shifted by shiftPS less the delay
// of the detector that fired (nil: none); dropped and valid count the raw channels
func buildEvents(timestamps []int64, channels, squashed []int8, keep []bool, shiftPS func(int64) int64, delays *[2][2]float64) (events []qkd.DetectionEvent, dropped, valid int) {
	for _, ch := range channels {
		if _, _, ok := channelToBasisBit(ch); !ok {
			dropped++
		}
	}
	valid = len(channels) - dropped
	// events only for the clicks keep allows (nil: all), so a busy side costs no more than it can pair
	n := 0
	for i, c := range squashed {
		if c > 0 && (keep == nil || keep[i]) {
			n++
		}
	}
	events = make([]qkd.DetectionEvent, 0, n)
	for i, c := range squashed {
		if c == 0 || keep != nil && !keep[i] {
			continue
		}
		basis, bit, _ := channelToBasisBit(c)
		events = append(events, qkd.DetectionEvent{TimestampPS: shiftPS(timestamps[i]) - delayOf(delays, channels[i]), Basis: basis, Bit: bit})
	}
	return events, dropped, valid
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
	multiPhotonWindowPS := flag.Int64("multiphoton-window", 2000, "squash each site's clicks within this many picoseconds of a first click into one detection, at that click's time: a double click in one basis gets that basis and a random bit, one across bases a random basis and bit (own side only, before cross-side matching)")
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
	chunkSize := flag.Int("chunk", 2048, "CRC chunk size (bits) of the verification after Cascade")
	keystoreDir := flag.String("keystore", "", "directory to save the final key into (ETSI 014 key_ID/key JSON); empty = don't save")
	keystoreMax := flag.Int("keystore-max", 0, "maximum keys to keep in -keystore, 0 = unlimited; existing keys are never evicted, only this run's own new keys are held back past the cap")
	flag.Bool("cascade", true, "optimized Cascade (cascade.go), the only error correction; accepted for older callers")
	cascadePasses := flag.Int("cascade-passes", 0, "Cascade passes, 0 = the measured default of 3")
	cascadeSample := flag.Bool("cascade-sample", false, "sacrifice a random sample for the QBER instead of counting the errors correction finds")
	sampleFraction := flag.Float64("sample", 0.05, "with -cascade-sample and -sample-auto=false: fraction of sifted bits sacrificed for the QBER")
	asymptotic := flag.Bool("asymptotic", false, "size the key by the asymptotic n(1-h2(QBER)) instead of the default finite-key bound (doc/security.md)")
	epsSec := flag.Float64("eps-sec", 1e-10, "finite-key secrecy failure probability per block")
	sampleAuto := flag.Bool("sample-auto", true, "finite-key mode: size the QBER sample to maximize the finite key, from the sifted count and -expected-qber only; overrides -sample")
	expectedQBER := flag.Float64("expected-qber", 0, "declared QBER prior for -sample-auto; 0 = the coincidence filter's timing-based prediction, or 0.05 without the filter")
	epsCor := flag.Float64("eps-cor", 1e-15, "finite-key correctness failure probability per block, sets the universal-hash tag length")
	jsonOut := flag.Bool("json", false, "print one JSON metrics object to stdout instead of the human-readable report")
	role := flag.String("role", "", "two-party mode, alice or bob: run one site's half on its own recording, exchanging public messages with -peer through the KMS (optimized Cascade, see twoparty.go)")
	peer := flag.String("peer", "", "two-party: the neighbor node running the other half")
	session := flag.String("session", "", "two-party: the session name both halves use")
	exportPublic := flag.String("export-public", "", "two-party, alice: write the announcement (timing, basis and multi-photon flag, never bits) into this directory and exit")
	streamDigest := flag.String("announcement-digest", "", "two-party, a block streamed in segments: SHA-256 (hex) of the whole announcement both sides hashed as it streamed")
	streamClicks := flag.Int("announcement-clicks", 0, "two-party, a block streamed in segments: how many clicks the whole announcement holds")
	peerTimeout := flag.Duration("peer-timeout", 15*time.Minute, "two-party: how long to wait for each message from the peer")
	cpuProfile := flag.String("cpuprofile", "", "write a CPU profile of this run to this file (go tool pprof)")
	planBlock := flag.String("plan-block", "", "continuous QKD: JSON of a link's recent public record and the lengths allowed (qkd.BlockPlanInput); print the next block's length as JSON and exit")
	flag.Parse()
	if *cpuProfile != "" {
		if f, err := os.Create(*cpuProfile); err == nil && pprof.StartCPUProfile(f) == nil {
			defer pprof.StopCPUProfile()
		}
	}
	keyOpts = keyOptions{asymptotic: *asymptotic, finite: qkd.FiniteKeyConfig{EpsSec: *epsSec, EpsCor: *epsCor}}

	if *planBlock != "" {
		var in qkd.BlockPlanInput
		if err := json.Unmarshal([]byte(*planBlock), &in); err != nil {
			failJSON("-plan-block: %v", err)
		}
		out, _ := json.Marshal(qkd.PlanBlock(in, keyOpts.finite))
		fmt.Println(string(out))
		return
	}
	if *dir == "" {
		if *jsonOut {
			failJSON("-dir is required")
		}
		fmt.Printf("%s -dir is required\n", tag(colorErr, "ERROR"))
		flag.Usage()
		os.Exit(2)
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
		if *calibrateDelays != "" {
			fail(*jsonOut, "two-party mode runs the whole chain; -calibrate-delays is single-site")
		}
		alignBits := 0
		if *keystoreDir != "" {
			alignBits = kms.ETSI014KeySizeBits
		}
		var dirs []string
		for _, d := range strings.Split(*dir, ",") {
			dirs = append(dirs, strings.TrimSpace(d))
		}
		digest, err := hex.DecodeString(*streamDigest)
		if err != nil {
			fail(*jsonOut, "-announcement-digest: %v", err)
		}
		runTwoParty(twoPartyOptions{
			streamDigest: digest, streamClicks: *streamClicks,
			role: *role, peer: *peer, session: *session, exportDir: *exportPublic, keystore: *keystoreDir,
			dirs: dirs, jsonOut: *jsonOut, mpWindowPS: *multiPhotonWindowPS, windowPS: *windowPS,
			filter: *filter, driftCfg: driftCfg, winCfg: winCfg, delays: delays, expectedQBER: *expectedQBER,
			timeout: *peerTimeout,
			cfg: qkd.TwoPartyConfig{ChunkSize: *chunkSize, KeyAlignBits: alignBits, SampleShare: *sampleFraction,
				Asymptotic: keyOpts.asymptotic, FiniteKey: keyOpts.finite, CascadeSample: *cascadeSample},
		})
		return
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
	var droppedA, droppedB, squashedA, squashedB, rawAlice, rawBob int
	for _, r := range recs {
		rawAlice += r.rawAlice
		droppedA += r.droppedA
		droppedB += r.droppedB
		rawBob += r.rawBob
		squashedA += r.squashedA
		squashedB += r.squashedB
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
		field("Double clicks squashed", "A: %d / %d, B: %d / %d (window=%d ps)", squashedA, rawAlice, squashedB, rawBob, *multiPhotonWindowPS)
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
	if !*jsonOut {
		field("Method", "optimized Cascade (passes=%d, chunk=%d)", *cascadePasses, *chunkSize)
	}
	// with no sample, Cascade plans from the QBER the filter predicts from timing, a public number
	plan := *expectedQBER
	if plan <= 0 && filterResult != nil {
		plan = filterResult.PredictedQBER
	}
	if *cascadeSample {
		plan = keyOpts.expectedQBER
	}
	finalKeyBytes, rep := runCascadePipeline(aliceEv, bobEv, window, *cascadePasses, *sampleFraction, *chunkSize, alignBits, plan, *cascadeSample, *jsonOut)
	rep.Method = "cascade"
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
		rep.ASquashedCount = squashedA
		rep.BSquashedCount = squashedB
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

// runCascadePipeline runs qkd.RunCascade and prints its report stage by stage
func runCascadePipeline(aliceEv, bobEv []qkd.DetectionEvent, window int64, passes int, sampleFraction float64, chunkSize, alignBits int, planQBER float64, sacrifice, jsonOut bool) ([]byte, jsonReport) {
	res, err := qkd.RunCascade(aliceEv, bobEv, qkd.CascadeRunConfig{
		CoincidenceWindowPS: window, SampleFraction: sampleFraction, Cascade: qkd.CascadeConfig{Passes: passes}, Seed: 1,
		ChunkSize: chunkSize, KeyAlignBits: alignBits, Asymptotic: keyOpts.asymptotic, FiniteKey: keyOpts.finite,
		ExpectedQBER: planQBER, SacrificeSample: sacrifice,
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
		field("Alice clicks in coincidence range", "%d", len(aliceEv))
		field("Bob clicks in coincidence range", "%d", len(bobEv))
		field("Sifted bits", "%d", res.SiftedBits)

		if sacrifice {
			section("(4 before 2/3): QBER from a sacrificed public sample")
		} else {
			section("(4 after 2/3): QBER counted by error correction, no bit sacrificed")
		}
		field("Sample bits", "%d (%.1f%% of sifted)", res.QBER.SampleBits, 100*float64(res.QBER.SampleBits)/float64(max(1, res.SiftedBits)))
		field("Error bits in sample", "%d", res.QBER.ErrorBits)
		field("QBER", "%.4f", res.QBER.QBER)

		section(fmt.Sprintf("(2): optimized Cascade (blocks=%v)", res.Cascade.BlockSizes))
		field("Bits corrected toward Alice", "%d", res.Cascade.Corrections)
		field("Parities disclosed", "%d (%d more inferred, %d searches)", res.Cascade.Parities, res.Cascade.Inferred, res.Cascade.Searches)

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
		SiftedBits: res.SiftedBits, CascadeParities: res.Cascade.Parities, CascadeCorrections: res.Cascade.Corrections,
		CascadeBlocks: res.Cascade.BlockSizes, FiniteKey: toFiniteKeyJSON(res.FiniteKey),
		ChunksTotal: cv.ChunksTotal, ChunksKept: cv.ChunksKept, ChunksDropped: cv.ChunksDropped, BitsLeakedCRC: cv.LeakedBits,
		QBER: res.QBER.QBER, LeakedBitsTotal: res.LeakedBits, FinalKeyBits: len(res.FinalKeyBits), CompressionRate: rate,
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
	if r.ChainBits > 0 {
		field("Chain rule", "%d bits: errors counted by correction, each basis bounded from the other's rounds", r.ChainBits)
		field("Error counts", "%d bits: Bob's announced errors per basis, charged at their worst case", r.CountBits)
	}
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
	dir                  string
	tsPathA, tsPathB     string
	report               coincidenceReport
	alice, bob           []qkd.DetectionEvent
	index                []int64 // two-party, a streamed block: each announced click's index in the whole announcement
	rawAlice, rawBob     int     // clicks before PruneToCoincidenceRange
	droppedA, droppedB   int
	squashedA, squashedB int
	filter               *qkd.CoincidenceFilter
}

// eventsCacheName holds one recording's aligned, pruned and filtered events, about 1 MB where the raw
// arrays take hundreds, so a continuously recorded segment can drop its raw files once processed
const eventsCacheName = "events.cache"

type recordingCache struct {
	Key                  string
	TsPathA, TsPathB     string
	Report               coincidenceReport
	Alice, Bob           []qkd.DetectionEvent
	RawAlice, RawBob     int
	DroppedA, DroppedB   int
	SquashedA, SquashedB int
	Filter               *qkd.CoincidenceFilter
}

func cacheKey(mpWindowPS, windowPS int64, filter bool, driftCfg qkd.DriftConfig, winCfg qkd.WindowConfig, delays *qkd.DetectorDelays) string {
	d := qkd.DetectorDelays{}
	if delays != nil {
		d = *delays
	}
	return fmt.Sprintf("v2 squash=%d window=%d filter=%t drift=%+v win=%+v delays=%+v", mpWindowPS, windowPS, filter, driftCfg, winCfg, d)
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
				squashedA: c.SquashedA, squashedB: c.SquashedB, filter: c.Filter}, nil
		}
	}
	r, err := loadRecording(dir, mpWindowPS, windowPS, filter, driftCfg, winCfg, delays)
	if err != nil {
		return r, err
	}
	c := recordingCache{Key: key, TsPathA: r.tsPathA, TsPathB: r.tsPathB, Report: r.report, Alice: r.alice, Bob: r.bob,
		RawAlice: r.rawAlice, RawBob: r.rawBob, DroppedA: r.droppedA, DroppedB: r.droppedB,
		SquashedA: r.squashedA, SquashedB: r.squashedB, Filter: r.filter}
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
	// each side's double clicks squashed on its own raw timing, both sides at once
	var sqA, sqB []int8
	var errA, errB error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); sqA, r.squashedA, errA = squashClicks(tsA, chA, mpWindowPS, squashRandom()) }()
	go func() { defer wg.Done(); sqB, r.squashedB, errB = squashClicks(tsB, chB, mpWindowPS, squashRandom()) }()
	wg.Wait()
	if err := errors.Join(errA, errB); err != nil {
		return r, err
	}

	// keep only clicks that could coincide, on both sides since either can be the busy one; the margin covers
	// the detector delays applied later, so the exact prune below sees every click it would have kept
	radius := pruneRadiusPS(windowPS, r.report)
	margin := radius + maxDelayPS(delays)
	bOnA := make([]int64, len(tsB))
	for i, t := range tsB {
		bOnA[i] = toAliceClock(t)
	}
	keepB := nearMask(bOnA, tsA, nil, margin)
	keepA := nearMask(tsA, bOnA, keepB, margin)
	var delA, delB *[2][2]float64
	if delays != nil {
		delA, delB = &delays.Alice, &delays.Bob
	}
	r.alice, r.droppedA, r.rawAlice = buildEvents(tsA, chA, sqA, keepA, func(ts int64) int64 { return ts }, delA)
	r.bob, r.droppedB, r.rawBob = buildEvents(tsB, chB, sqB, keepB, toAliceClock, delB)
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
	ChainRuleBits   int     `json:"chain_rule_bits"` // errors counted by correction: the chain rule's cost
	CountBits       int     `json:"count_bits"`      // and what the announced per-basis error counts can disclose
}

func toFiniteKeyJSON(r *qkd.FiniteKeyReport) *finiteKeyJSON {
	if r == nil {
		return nil
	}
	return &finiteKeyJSON{PhaseErrorPoint: r.PhiPoint, PhaseErrorBound: r.Phi, TagBits: r.TagBits, Verified: r.Verified, SurvivingBits: r.Bits, ChargedBits: r.LeakedBits, ChainRuleBits: r.ChainBits, CountBits: r.CountBits}
}
