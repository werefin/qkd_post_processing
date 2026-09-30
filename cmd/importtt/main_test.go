package main

import (
	"os"
	"path/filepath"
	"testing"

	qkd "qoj_post_processing"
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
