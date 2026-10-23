package main

import (
	"bytes"
	crand "crypto/rand"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"qkd_post_processing/qkd"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestFindRecordingAssignsByReportNotSortOrder: the report's input_a/input_b
// must decide which file is Alice, even when that disagrees with plain alphabetical sort
func TestFindRecordingAssignsByReportNotSortOrder(t *testing.T) {
	dir := t.TempDir()
	// "z_..." sorts after "a_...", but the report says z is actually A
	writeFile(t, filepath.Join(dir, "a_site_timestamp_sequence.npy"), "x")
	writeFile(t, filepath.Join(dir, "z_site_timestamp_sequence.npy"), "x")
	writeFile(t, filepath.Join(dir, "report_coincidence.json"), `{
		"detected": true,
		"input_a": "z_site_timestamp_sequence.npy",
		"input_b": "a_site_timestamp_sequence.npy"
	}`)

	tsA, tsB, report, err := findRecording(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !report.Detected {
		t.Fatal("expected Detected=true to round-trip from the report")
	}
	if filepath.Base(tsA) != "z_site_timestamp_sequence.npy" {
		t.Fatalf("A should be z_site (per report.InputA), got %s", tsA)
	}
	if filepath.Base(tsB) != "a_site_timestamp_sequence.npy" {
		t.Fatalf("B should be a_site (per report.InputB), got %s", tsB)
	}
}

// TestFindRecordingFallsBackToSortOrderWhenReportNamesDontMatch: if the report's recorded
// filenames match no file present, fall back to sorted order instead of an empty path
func TestFindRecordingFallsBackToSortOrderWhenReportNamesDontMatch(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a_site_timestamp_sequence.npy"), "x")
	writeFile(t, filepath.Join(dir, "b_site_timestamp_sequence.npy"), "x")
	writeFile(t, filepath.Join(dir, "report_coincidence.json"), `{
		"detected": true,
		"input_a": "nonexistent_1_timestamp_sequence.npy",
		"input_b": "nonexistent_2_timestamp_sequence.npy"
	}`)

	tsA, tsB, _, err := findRecording(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if filepath.Base(tsA) != "a_site_timestamp_sequence.npy" || filepath.Base(tsB) != "b_site_timestamp_sequence.npy" {
		t.Fatalf("expected sorted fallback (a_site, b_site), got (%s, %s)", tsA, tsB)
	}
}

// TestFindRecordingRejectsUndetectedReport: a report whose own coincidence search
// failed must refuse to proceed, not hand back an untrustworthy alignment
func TestFindRecordingRejectsUndetectedReport(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a_site_timestamp_sequence.npy"), "x")
	writeFile(t, filepath.Join(dir, "b_site_timestamp_sequence.npy"), "x")
	writeFile(t, filepath.Join(dir, "report_coincidence.json"), `{"detected": false}`)

	if _, _, _, err := findRecording(dir); err == nil {
		t.Fatal("expected an error for a report with detected=false, got nil")
	}
}

// TestFindRecordingRejectsWrongFileCounts checks the two sanity gates on
// file counts before any report parsing happens
func TestFindRecordingRejectsWrongFileCounts(t *testing.T) {
	t.Run("missing a timestamp file", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "a_site_timestamp_sequence.npy"), "x")
		if _, _, _, err := findRecording(dir); err == nil {
			t.Fatal("expected an error with only one timestamp file present")
		}
	})
	t.Run("missing coincidence report", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "a_site_timestamp_sequence.npy"), "x")
		writeFile(t, filepath.Join(dir, "b_site_timestamp_sequence.npy"), "x")
		if _, _, _, err := findRecording(dir); err == nil {
			t.Fatal("expected an error with no coincidence report present")
		}
	})
}

// TestChannelPath derives the sibling *_channel_sequence.npy path
func TestChannelPath(t *testing.T) {
	got := channelPath("/rec/20261006_095147_192_0_2_10_p40404_timestamp_sequence.npy")
	want := "/rec/20261006_095147_192_0_2_10_p40404_channel_sequence.npy"
	if got != want {
		t.Fatalf("channelPath() = %q, want %q", got, want)
	}
}

// TestChannelToBasisBitAllChannels checks every valid channel's mapping and
// that invalid channels are rejected, not silently mapped to channel 1
func TestChannelToBasisBitAllChannels(t *testing.T) {
	cases := []struct {
		ch    int8
		basis qkd.Basis
		bit   byte
		ok    bool
	}{
		{1, 0, 0, true}, {2, 0, 1, true}, {3, 1, 0, true}, {4, 1, 1, true},
		{0, 0, 0, false}, {5, 0, 0, false}, {-1, 0, 0, false},
	}
	for _, c := range cases {
		basis, bit, ok := channelToBasisBit(c.ch)
		if ok != c.ok {
			t.Fatalf("channel %d: ok=%v, want %v", c.ch, ok, c.ok)
		}
		if ok && (basis != c.basis || bit != c.bit) {
			t.Fatalf("channel %d: got (basis=%d,bit=%d), want (basis=%d,bit=%d)", c.ch, basis, bit, c.basis, c.bit)
		}
	}
}

// TestChannelLabelIsExactInverseOfChannelToBasisBit: round-tripping any
// valid channel through both directions must return the same channel
func TestChannelLabelIsExactInverseOfChannelToBasisBit(t *testing.T) {
	labels := map[int8]byte{1: 'H', 2: 'V', 3: 'D', 4: 'A'}
	for ch, want := range labels {
		basis, bit, ok := channelToBasisBit(ch)
		if !ok {
			t.Fatalf("channel %d unexpectedly invalid", ch)
		}
		got := channelLabel(basis, bit)
		if got != want {
			t.Fatalf("channel %d: channelLabel(basis=%d,bit=%d)=%c, want %c", ch, basis, bit, got, want)
		}
	}
}

// bruteForceSquash is an obviously-correct reference: each measurement click no earlier one claimed
// claims every later one within windowPS, and its outcome follows the table below, one random byte a choice
func bruteForceSquash(timestamps []int64, channels []int8, windowPS int64, random []byte) ([]int8, int) {
	out := make([]int8, len(channels))
	claimed := make([]bool, len(channels))
	groups := 0
	for i := range channels {
		if channels[i] < 1 || channels[i] > 4 || claimed[i] {
			continue
		}
		seen := map[int8]bool{channels[i]: true}
		size := 1
		for j := i + 1; j < len(channels); j++ {
			if channels[j] >= 1 && channels[j] <= 4 && timestamps[j]-timestamps[i] <= windowPS {
				claimed[j] = true
				seen[channels[j]] = true
				size++
			}
		}
		if size > 1 {
			groups++
		}
		hv, da := seen[1] || seen[2], seen[3] || seen[4]
		switch {
		case len(seen) == 1:
			out[i] = channels[i]
		case hv && !da:
			out[i], random = 1+int8(random[0]%2), random[1:]
		case da && !hv:
			out[i], random = 3+int8(random[0]%2), random[1:]
		default:
			out[i], random = 1+int8(random[0]%4), random[1:]
		}
	}
	return out, groups
}

// TestSquashClicksAgainstBruteForce fuzzes squashClicks against the reference on dense click streams, with
// the same random bytes, so every group and every outcome must agree
func TestSquashClicksAgainstBruteForce(t *testing.T) {
	for seed := int64(0); seed < 300; seed++ {
		r := rand.New(rand.NewSource(seed))
		n := 5 + r.Intn(80)
		spanPS := int64(50 + r.Intn(2000))
		windowPS := int64(r.Intn(100))
		timestamps := make([]int64, n)
		channels := make([]int8, n)
		for i := range timestamps {
			timestamps[i] = r.Int63n(spanPS)
			channels[i] = int8(r.Intn(6))
		}
		slices.Sort(timestamps)
		random := make([]byte, n)
		r.Read(random)

		got, gotGroups, err := squashClicks(timestamps, channels, windowPS, bytes.NewReader(random))
		if err != nil {
			t.Fatal(err)
		}
		want, wantGroups := bruteForceSquash(timestamps, channels, windowPS, random)
		if !slices.Equal(got, want) || gotGroups != wantGroups {
			t.Fatalf("seed=%d window=%d: got %v (%d groups), want %v (%d)\nts=%v\nch=%v",
				seed, windowPS, got, gotGroups, want, wantGroups, timestamps, channels)
		}
	}
}

// TestSquashOutcomesAreUniform: a double click in one basis lands on either bit, one across bases on all four
// outcomes, each about equally often, so a squashed click tells nothing of the physics behind it
func TestSquashOutcomesAreUniform(t *testing.T) {
	const n = 40_000
	for _, tc := range []struct {
		pair    [2]int8
		allowed []int8
	}{{[2]int8{1, 2}, []int8{1, 2}}, {[2]int8{4, 3}, []int8{3, 4}}, {[2]int8{1, 3}, []int8{1, 2, 3, 4}}, {[2]int8{4, 2}, []int8{1, 2, 3, 4}}} {
		timestamps, channels := make([]int64, 2*n), make([]int8, 2*n)
		for i := range n {
			timestamps[2*i], timestamps[2*i+1] = int64(i)*1e6, int64(i)*1e6+500
			channels[2*i], channels[2*i+1] = tc.pair[0], tc.pair[1]
		}
		sq, groups, err := squashClicks(timestamps, channels, 2000, crand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if groups != n {
			t.Fatalf("%v: %d groups, want %d", tc.pair, groups, n)
		}
		counts := map[int8]int{}
		for i, c := range sq {
			if i%2 == 1 && c != 0 {
				t.Fatalf("%v: the second click of a group kept its own detection", tc.pair)
			}
			if i%2 == 0 {
				counts[c]++
			}
		}
		for _, c := range tc.allowed {
			want := float64(n) / float64(len(tc.allowed))
			if math.Abs(float64(counts[c])-want) > 6*math.Sqrt(want) {
				t.Fatalf("%v: outcome %d in %d of %d, want about %.0f", tc.pair, c, counts[c], n, want)
			}
		}
		if total := len(counts); total != len(tc.allowed) {
			t.Fatalf("%v: outcomes %v, allowed %v", tc.pair, counts, tc.allowed)
		}
	}
}

// bruteForceVisibility independently recomputes visibilityMetrics from MatchCoincidences'
// output, spelling out the formulas by hand rather than reusing computeVisibilityMetrics
func bruteForceVisibility(aliceEv, bobEv []qkd.DetectionEvent, windowPS int64) visibilityMetrics {
	pairs := qkd.MatchCoincidences(qkd.StripBit(aliceEv), qkd.StripBit(bobEv), windowPS)
	counts := map[[2]byte]int{}
	for _, p := range pairs {
		a, b := aliceEv[p.AliceIdx], bobEv[p.BobIdx]
		al := map[bool]map[byte]byte{false: {0: 'H', 1: 'V'}, true: {0: 'D', 1: 'A'}}[a.Basis == 1][a.Bit]
		bl := map[bool]map[byte]byte{false: {0: 'H', 1: 'V'}, true: {0: 'D', 1: 'A'}}[b.Basis == 1][b.Bit]
		counts[[2]byte{al, bl}]++
	}
	var m visibilityMetrics
	m.CoincidenceHH, m.CoincidenceVV = counts[[2]byte{'H', 'H'}], counts[[2]byte{'V', 'V'}]
	m.CoincidenceHV, m.CoincidenceVH = counts[[2]byte{'H', 'V'}], counts[[2]byte{'V', 'H'}]
	m.CoincidenceDD, m.CoincidenceAA = counts[[2]byte{'D', 'D'}], counts[[2]byte{'A', 'A'}]
	m.CoincidenceDA, m.CoincidenceAD = counts[[2]byte{'D', 'A'}], counts[[2]byte{'A', 'D'}]

	hvTotal := m.CoincidenceHH + m.CoincidenceVV + m.CoincidenceHV + m.CoincidenceVH
	daTotal := m.CoincidenceDD + m.CoincidenceAA + m.CoincidenceDA + m.CoincidenceAD
	m.SiftedHV, m.SiftedDA = hvTotal, daTotal
	if hvTotal > 0 {
		agree, disagree := m.CoincidenceHH+m.CoincidenceVV, m.CoincidenceHV+m.CoincidenceVH
		m.VisibilityHV = float64(agree-disagree) / float64(hvTotal)
		m.QBERBasisHV = float64(disagree) / float64(hvTotal)
	}
	if daTotal > 0 {
		agree, disagree := m.CoincidenceDD+m.CoincidenceAA, m.CoincidenceDA+m.CoincidenceAD
		m.VisibilityDA = float64(agree-disagree) / float64(daTotal)
		m.QBERBasisDA = float64(disagree) / float64(daTotal)
	}
	if hvTotal+daTotal > 0 {
		m.FidelityVis = (m.VisibilityHV + m.VisibilityDA) / 2
		disagreeTotal := m.CoincidenceHV + m.CoincidenceVH + m.CoincidenceDA + m.CoincidenceAD
		m.QBERVis = float64(disagreeTotal) / float64(hvTotal+daTotal)
	}
	return m
}

// bruteForceBuildEvents independently re-derives buildEvents' output field
// by field, without reusing channelToBasisBit or any of its control flow
func bruteForceBuildEvents(timestamps []int64, channels, squashed []int8, shiftPS func(int64) int64, delays [2][2]float64) ([]qkd.DetectionEvent, int) {
	labels := map[int8][2]byte{1: {0, 0}, 2: {0, 1}, 3: {1, 0}, 4: {1, 1}}
	var events []qkd.DetectionEvent
	dropped := 0
	for i := range timestamps {
		if _, ok := labels[channels[i]]; !ok {
			dropped++
		}
		bb, ok := labels[squashed[i]]
		if !ok {
			continue
		}
		// delay of the detector that fired, not of the squashed outcome
		var delay float64
		if phys, ok := labels[channels[i]]; ok {
			delay = delays[phys[0]][phys[1]]
		}
		events = append(events, qkd.DetectionEvent{
			TimestampPS: shiftPS(timestamps[i]) - int64(math.Round(delay)),
			Basis:       qkd.Basis(bb[0]),
			Bit:         bb[1],
		})
	}
	return events, dropped
}

// TestBuildEventsAgainstBruteForce fuzzes buildEvents (turns squashed recorded arrays into
// DetectionEvents) against an independent reference, including invalid channels, a shift and delays
func TestBuildEventsAgainstBruteForce(t *testing.T) {
	for seed := int64(0); seed < 200; seed++ {
		r := rand.New(rand.NewSource(20000 + seed))
		n := 1 + r.Intn(100)
		timestamps := make([]int64, n)
		channels := make([]int8, n)
		squashed := make([]int8, n)
		for i := 0; i < n; i++ {
			timestamps[i] = r.Int63n(1_000_000_000)
			// mix valid (1-4) and invalid (0, 5, -1) channels
			switch r.Intn(6) {
			case 0:
				channels[i] = 0
			case 1:
				channels[i] = 5
			case 2:
				channels[i] = -1
			default:
				channels[i] = int8(1 + r.Intn(4))
				// most keep their channel, some were absorbed (0) or given another outcome
				switch r.Intn(5) {
				case 0:
				case 1:
					squashed[i] = int8(1 + r.Intn(4))
				default:
					squashed[i] = channels[i]
				}
			}
		}
		offset := r.Int63n(2000) - 1000
		shift := func(ts int64) int64 { return ts*2 + offset } // non-trivial, non-identity
		delays := [2][2]float64{{r.Float64() * 300, r.Float64() * 300}, {r.Float64() * 300, r.Float64() * 300}}

		gotEvents, gotDropped, _ := buildEvents(timestamps, channels, squashed, nil, shift, &delays)
		wantEvents, wantDropped := bruteForceBuildEvents(timestamps, channels, squashed, shift, delays)

		if gotDropped != wantDropped {
			t.Fatalf("seed=%d: dropped=%d want %d", seed, gotDropped, wantDropped)
		}
		if len(gotEvents) != len(wantEvents) {
			t.Fatalf("seed=%d: %d events, want %d", seed, len(gotEvents), len(wantEvents))
		}
		for i := range gotEvents {
			if gotEvents[i] != wantEvents[i] {
				t.Fatalf("seed=%d: event %d: got %+v, want %+v", seed, i, gotEvents[i], wantEvents[i])
			}
		}
	}
}

// TestComputeVisibilityMetricsAgainstBruteForce fuzzes computeVisibilityMetrics against an
// independent reference across both basis-correlated and uncorrelated (noise-like) regimes
func TestComputeVisibilityMetricsAgainstBruteForce(t *testing.T) {
	for seed := int64(0); seed < 300; seed++ {
		r := rand.New(rand.NewSource(9000 + seed))
		n := 10 + r.Intn(200)
		spanPS := int64(1000 + r.Intn(20000))
		windowPS := int64(10 + r.Intn(500))
		// half the seeds simulate real correlation, half pure noise, so the
		// reference and the real function are compared in both regimes
		correlated := seed%2 == 0
		errRate := 0.02
		if !correlated {
			errRate = 0.5
		}

		var aliceEv, bobEv []qkd.DetectionEvent
		for i := 0; i < n; i++ {
			ts := r.Int63n(spanPS)
			basis := qkd.Basis(r.Intn(2))
			bit := byte(r.Intn(2))
			bBit := bit
			if r.Float64() < errRate {
				bBit ^= 1
			}
			aliceEv = append(aliceEv, qkd.DetectionEvent{TimestampPS: ts, Basis: basis, Bit: bit})
			bobEv = append(bobEv, qkd.DetectionEvent{TimestampPS: ts + int64(r.Intn(21)-10), Basis: basis, Bit: bBit})
		}

		got := computeVisibilityMetrics(aliceEv, bobEv, windowPS)
		want := bruteForceVisibility(aliceEv, bobEv, windowPS)

		if got != want {
			t.Fatalf("seed=%d window=%d correlated=%v: mismatch\ngot=%+v\nwant=%+v",
				seed, windowPS, correlated, got, want)
		}
	}
}
