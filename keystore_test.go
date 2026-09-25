package qoj_post_processing

import (
	"bytes"
	"regexp"
	"testing"
)

var uuidV4Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestSaveLoadKeyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	key := []byte("some distilled secret key material")

	stored, err := SaveKey(dir, key)
	if err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	if !uuidV4Re.MatchString(stored.KeyID) {
		t.Fatalf("key_ID %q is not a version-4 UUID", stored.KeyID)
	}

	got, err := LoadKey(dir, stored.KeyID)
	if err != nil {
		t.Fatalf("LoadKey: %v", err)
	}
	if !bytes.Equal(got, key) {
		t.Fatalf("round trip mismatch: got %q, want %q", got, key)
	}
}

func TestSaveKeyGeneratesDistinctIDs(t *testing.T) {
	dir := t.TempDir()
	seen := make(map[string]bool)
	for range 100 {
		stored, err := SaveKey(dir, []byte("key"))
		if err != nil {
			t.Fatalf("SaveKey: %v", err)
		}
		if seen[stored.KeyID] {
			t.Fatalf("duplicate key_ID %q", stored.KeyID)
		}
		seen[stored.KeyID] = true
	}
}

func TestSaveKeyCreatesDir(t *testing.T) {
	dir := t.TempDir() + "/nested/keystore"
	if _, err := SaveKey(dir, []byte("key")); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
}

func TestLoadKeyMissingID(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadKey(dir, "00000000-0000-4000-8000-000000000000"); err == nil {
		t.Fatal("expected an error loading a key_ID that was never saved")
	}
}
