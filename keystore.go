package qoj_post_processing

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// StoredKey is one key record in ETSI 014 key container format a UUID key_ID and the key material, base64-encoded
type StoredKey struct {
	KeyID string `json:"key_ID"`
	Key   string `json:"key"`
}

// newKeyID generates a random UUID (version 4, RFC 4122) for use as an ETSI 014 key_ID
func newKeyID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate key_ID: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// SaveKey assigns a fresh key_ID to keyBytes and writes it as <dir>/<key_ID>.json in the ETSI 014 key container format,
// creating dir if it does not exist --> returns the stored record, key_ID included
func SaveKey(dir string, keyBytes []byte) (StoredKey, error) {
	keyID, err := newKeyID()
	if err != nil {
		return StoredKey{}, err
	}
	stored := StoredKey{
		KeyID: keyID,
		Key:   base64.StdEncoding.EncodeToString(keyBytes),
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return StoredKey{}, fmt.Errorf("save key %s: %w", keyID, err)
	}
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return StoredKey{}, fmt.Errorf("save key %s: %w", keyID, err)
	}
	// 0o600: key material, not group/world readable
	if err := os.WriteFile(filepath.Join(dir, keyID+".json"), data, 0o600); err != nil {
		return StoredKey{}, fmt.Errorf("save key %s: %w", keyID, err)
	}
	return stored, nil
}

// LoadKey reads back a key previously written by SaveKey and decodes its base64 key material
func LoadKey(dir, keyID string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(dir, keyID+".json"))
	if err != nil {
		return nil, fmt.Errorf("load key %s: %w", keyID, err)
	}
	var stored StoredKey
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("load key %s: %w", keyID, err)
	}
	keyBytes, err := base64.StdEncoding.DecodeString(stored.Key)
	if err != nil {
		return nil, fmt.Errorf("load key %s: decode: %w", keyID, err)
	}
	return keyBytes, nil
}
