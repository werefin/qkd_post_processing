package main

import (
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"qkd_post_processing/qkd"
)

// every squashing starts from the same random bytes, so a site squashing its recording twice, once for the
// joint pipeline and once for its role, makes the same choices and the two can be compared pair for pair
func init() {
	squashRandom = func() io.Reader { return rand.New(rand.NewSource(1)) }
}

// splitRecording turns one joint recording directory into what each site really holds: Alice
// her own files, Bob his own files plus Alice's announcement and a report naming it input A
func splitRecording(t *testing.T, joint string) (aliceDir, bobDir string) {
	t.Helper()
	// example recording is real data, kept locally and never committed, so CI has none
	if _, err := os.Stat(joint); errors.Is(err, os.ErrNotExist) {
		t.Skipf("%s not present", joint)
	}
	tsA, tsB, report, err := findRecording(joint)
	if err != nil {
		t.Fatal(err)
	}
	aliceDir, bobDir = t.TempDir(), t.TempDir()
	link := func(src, dir string) {
		abs, _ := filepath.Abs(src)
		if err := os.Symlink(abs, filepath.Join(dir, filepath.Base(src))); err != nil {
			t.Fatal(err)
		}
	}
	link(tsA, aliceDir)
	link(channelPath(tsA), aliceDir)
	link(tsB, bobDir)
	link(channelPath(tsB), bobDir)
	if _, err := runAliceRole(twoPartyOptions{role: "alice", dirs: []string{aliceDir}, exportDir: bobDir, mpWindowPS: 2000}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(mustGlob(t, filepath.Join(joint, "*_coincidence.json")))
	var m map[string]any
	json.Unmarshal(raw, &m)
	m["input_a"] = strings.TrimSuffix(filepath.Base(tsA), "_timestamp_sequence.npy") + publicTSSuffix
	m["input_b"] = filepath.Base(tsB)
	out, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(bobDir, "pair_coincidence.json"), out, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = report
	return aliceDir, bobDir
}

func mustGlob(t *testing.T, pattern string) string {
	t.Helper()
	m, _ := filepath.Glob(pattern)
	if len(m) != 1 {
		t.Fatalf("%s: %d matches", pattern, len(m))
	}
	return m[0]
}

// runRoles runs Alice's and Bob's roles at once over an in-process connection
func runRoles(t *testing.T, aliceDirs, bobDirs []string, cfg qkd.TwoPartyConfig, keysA, keysB string) (a, b twoPartyJSON) {
	t.Helper()
	ca, cb := qkd.PipeConns()
	base := twoPartyOptions{mpWindowPS: 2000, filter: true, cfg: cfg}
	oa, ob := base, base
	oa.role, oa.dirs, oa.keystore, oa.dial = "alice", aliceDirs, keysA, func() (qkd.Conn, error) { return ca, nil }
	ob.role, ob.dirs, ob.keystore, ob.dial = "bob", bobDirs, keysB, func() (qkd.Conn, error) { return cb, nil }
	errA := make(chan error, 1)
	go func() {
		var err error
		a, err = runAliceRole(oa)
		errA <- err
	}()
	b, errB := runBobRole(ob)
	if err := <-errA; err != nil || errB != nil {
		t.Fatalf("alice %v, bob %v", err, errB)
	}
	return a, b
}

// TestTwoPartyRolesOnRealRecording: on the example recording, each site using only its own
// files reaches the same sifted pairs and QBER as the joint pipeline does
func TestTwoPartyRolesOnRealRecording(t *testing.T) {
	joint := filepath.Join("..", "..", "acquisition", "example", "20260909")
	aliceDir, bobDir := splitRecording(t, joint)
	cfg := qkd.TwoPartyConfig{ChunkSize: 1024, SampleSeed: 11, ExpectedQBER: 0.05}
	a, b := runRoles(t, []string{aliceDir}, []string{bobDir}, cfg, "", "")

	rec, err := loadRecording(joint, 2000, 0, true, qkd.DriftConfig{}, qkd.WindowConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sifted := len(qkd.Sift(rec.alice, rec.bob, rec.filter.WindowPS))
	if a.SiftedBits != sifted || b.SiftedBits != sifted || a.QBER != b.QBER || a.AnnouncementDigest != b.AnnouncementDigest {
		t.Fatalf("joint %d sifted; alice %+v; bob %+v", sifted, a, b)
	}
	if b.FilterWindowPS != rec.filter.WindowPS {
		t.Fatalf("Bob's window %d, joint %d", b.FilterWindowPS, rec.filter.WindowPS)
	}
	t.Logf("%d sifted, QBER %.3f, window +-%d ps, key %d bits (10 s is too short for a finite key)", a.SiftedBits, a.QBER, b.FilterWindowPS, a.FinalKeyBits)
}

// TestTwoPartyRolesCascadeOnExample: optimized Cascade across the two roles on the example recording,
// each site with only its own files: same pairs, the same error counts and disclosure on both sides, every
// chunk verified, and Alice's own count of her parities equal to Bob's
func TestTwoPartyRolesCascadeOnExample(t *testing.T) {
	aliceDir, bobDir := splitRecording(t, filepath.Join("..", "..", "acquisition", "example", "20260909"))
	cfg := qkd.TwoPartyConfig{ChunkSize: 1024, SampleSeed: 11, ExpectedQBER: 0.05}
	a, b := runRoles(t, []string{aliceDir}, []string{bobDir}, cfg, "", "")
	if a.SiftedBits != b.SiftedBits || a.QBER != b.QBER || a.LeakedBitsTotal != b.LeakedBitsTotal || a.ECLeakedBits != b.CascadeParities || b.ChunksKept != b.ChunksTotal {
		t.Fatalf("alice %+v; bob %+v", a, b)
	}
	t.Logf("%d sifted, QBER %.3f counted by correction, %d parities in %d round trips, %d corrections, all chunks kept",
		a.SiftedBits, a.QBER, b.CascadeParities, b.CascadeRounds, b.CascadeCorrected)
}

// TestBobCountsPairsPerPolarization: Bob's own pair counts add up to his verified bits, and the agreeing pairs
// outnumber the others by far at this QBER
func TestBobCountsPairsPerPolarization(t *testing.T) {
	aliceDir, bobDir := splitRecording(t, filepath.Join("..", "..", "acquisition", "example", "20260909"))
	cfg := qkd.TwoPartyConfig{ChunkSize: 1024, SampleSeed: 11, ExpectedQBER: 0.05}
	a, b := runRoles(t, []string{aliceDir}, []string{bobDir}, cfg, "", "")
	total, agree := 0, 0
	for k, v := range b.PairCounts {
		total += v
		if k[0] == k[1] {
			agree += v
		}
	}
	if a.PairCounts != nil || total != b.BasisBits[0]+b.BasisBits[1] || agree < 9*(total-agree) {
		t.Fatalf("alice %v; bob %v over %v bits", a.PairCounts, b.PairCounts, b.BasisBits)
	}
}

// TestAliceAnnouncesWhatSheKeeps: with real randomness, Alice's announcement and her own events for the role
// come from the same squashing, kept beside her recording, and the announcement holds no bits nor the choices
func TestAliceAnnouncesWhatSheKeeps(t *testing.T) {
	defer func(f func() io.Reader) { squashRandom = f }(squashRandom)
	squashRandom = func() io.Reader { return crand.Reader }
	r := rand.New(rand.NewSource(3))
	var ts []int64
	var ch []int8
	for i := range 20_000 {
		at := int64(i) * 1_000_000
		ts, ch = append(ts, at), append(ch, int8(1+r.Intn(4)))
		// every third a double click 300 ps later, on any detector
		if i%3 == 0 {
			ts, ch = append(ts, at+300), append(ch, int8(1+r.Intn(4)))
		}
	}
	own, pub := t.TempDir(), t.TempDir()
	if err := writeNpyInt64(filepath.Join(own, "x_timestamp_sequence.npy"), ts); err != nil {
		t.Fatal(err)
	}
	if err := writeNpyInt8(filepath.Join(own, "x_channel_sequence.npy"), ch); err != nil {
		t.Fatal(err)
	}
	o := twoPartyOptions{role: "alice", dirs: []string{own}, exportDir: pub, mpWindowPS: 2000}
	out, err := runAliceRole(o)
	if err != nil {
		t.Fatal(err)
	}
	o.exportDir = ""
	events, _, _, err := aliceEvents(o)
	if err != nil {
		t.Fatal(err)
	}
	at, err := loadNpyInt64(filepath.Join(pub, "x"+publicTSSuffix))
	if err != nil {
		t.Fatal(err)
	}
	flags, err := loadNpyInt8(filepath.Join(pub, "x"+publicFlagsSuffix))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 20_000 || len(at) != len(events) || out.AnnouncementDigest != fmt.Sprintf("%x", qkd.PublicDigest(qkd.StripBit(events))[:8]) {
		t.Fatalf("%d events, %d announced, digest %s", len(events), len(at), out.AnnouncementDigest)
	}
	for i, e := range events {
		if e.TimestampPS != at[i] || int8(e.Basis) != flags[i] {
			t.Fatalf("click %d: announced %d basis %d, kept %+v", i, at[i], flags[i], e)
		}
	}
	if files, _ := os.ReadDir(pub); len(files) != 2 {
		t.Fatalf("announcement holds %d files, want timing and bases only", len(files))
	}
	if !fileExists(filepath.Join(own, "x"+squashedSuffix)) {
		t.Fatal("the squashing choices were not kept beside the recording")
	}
}
