package main

import (
	"os"
	"path/filepath"
	"testing"

	qkd "qkd_post_processing"
)

func writeFakeKeys(t *testing.T, dir string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		name := filepath.Join(dir, "existing-"+string(rune('a'+i))+".json")
		if err := os.WriteFile(name, []byte(`{"key_ID":"x","key":"aa"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func countKeystoreFiles(t *testing.T, dir string) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return len(matches)
}

func TestSaveToKeystoreUncapped(t *testing.T) {
	dir := t.TempDir()
	keyBytes := make([]byte, qkd.ETSI014KeySizeBytes*3) // 3 whole keys
	saved := saveToKeystore(dir, keyBytes, 0, true)
	if saved != 3 {
		t.Fatalf("saved = %d, want 3", saved)
	}
	if got := countKeystoreFiles(t, dir); got != 3 {
		t.Fatalf("keystore has %d files, want 3", got)
	}
}

func TestSaveToKeystoreCapLeavesRoom(t *testing.T) {
	dir := t.TempDir()
	writeFakeKeys(t, dir, 5) // pre-existing "real" keys, never touched
	keyBytes := make([]byte, qkd.ETSI014KeySizeBytes*4)
	saved := saveToKeystore(dir, keyBytes, 8, true) // room for 3 more
	if saved != 3 {
		t.Fatalf("saved = %d, want 3 (cap=8, 5 already there)", saved)
	}
	if got := countKeystoreFiles(t, dir); got != 8 {
		t.Fatalf("keystore has %d files, want 8", got)
	}
}

func TestSaveToKeystoreAtCapSavesNothing(t *testing.T) {
	dir := t.TempDir()
	writeFakeKeys(t, dir, 8)
	keyBytes := make([]byte, qkd.ETSI014KeySizeBytes*4)
	saved := saveToKeystore(dir, keyBytes, 8, true)
	if saved != 0 {
		t.Fatalf("saved = %d, want 0 (already at cap)", saved)
	}
	if got := countKeystoreFiles(t, dir); got != 8 {
		t.Fatalf("keystore has %d files, want 8 (existing keys must survive untouched)", got)
	}
}

func TestSaveToKeystoreCapExactFit(t *testing.T) {
	dir := t.TempDir()
	writeFakeKeys(t, dir, 6)
	keyBytes := make([]byte, qkd.ETSI014KeySizeBytes*2) // exactly fills remaining room
	saved := saveToKeystore(dir, keyBytes, 8, true)
	if saved != 2 {
		t.Fatalf("saved = %d, want 2", saved)
	}
	if got := countKeystoreFiles(t, dir); got != 8 {
		t.Fatalf("keystore has %d files, want 8", got)
	}
}

// TestPoolRecordingsKeepsEveryEventAndSeparatesRecordings: pooling must keep all events of
// all recordings, keep each recording's internal timing, and leave a gap no window can bridge
func TestPoolRecordingsKeepsEveryEventAndSeparatesRecordings(t *testing.T) {
	mk := func(ts ...int64) []qkd.DetectionEvent {
		out := make([]qkd.DetectionEvent, len(ts))
		for i, v := range ts {
			out[i] = qkd.DetectionEvent{TimestampPS: v}
		}
		return out
	}
	recs := []loadedRecording{
		{alice: mk(100, 500), bob: mk(110, 490)},
		{alice: mk(9_000_000, 9_000_300), bob: mk(9_000_010)},
	}
	alice, bob := poolRecordings(recs)
	if len(alice) != 4 || len(bob) != 3 {
		t.Fatalf("pooled %d alice / %d bob events, want 4 / 3", len(alice), len(bob))
	}
	if bob[0].TimestampPS-alice[0].TimestampPS != 10 || bob[2].TimestampPS-alice[2].TimestampPS != 10 {
		t.Fatal("pooling changed a recording's internal alice-bob offsets")
	}
	if gap := alice[2].TimestampPS - alice[1].TimestampPS; gap < poolRecordingGapPS {
		t.Fatalf("recordings only %d ps apart, want >= %d", gap, poolRecordingGapPS)
	}
}
