package kms

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

// ETSI014KeySizeBytes is 256 bits, the key size ETSI GS QKD 014's Get Key API hands out by default
const ETSI014KeySizeBytes = 32

// ETSI014KeySizeBits is ETSI014KeySizeBytes in bits, for aligning
// PrivacyAmplify's output so SaveKeys256 never discards a remainder
const ETSI014KeySizeBits = ETSI014KeySizeBytes * 8

// SaveKeys256 splits keyBytes into ETSI014KeySizeBytes chunks and saves each as its own key_ID via SaveKey
func SaveKeys256(dir string, keyBytes []byte) (stored []StoredKey, discardedBits int, err error) {
	for len(keyBytes) >= ETSI014KeySizeBytes {
		s, err := SaveKey(dir, keyBytes[:ETSI014KeySizeBytes])
		if err != nil {
			return stored, discardedBits, err
		}
		stored = append(stored, s)
		keyBytes = keyBytes[ETSI014KeySizeBytes:]
	}
	return stored, len(keyBytes) * 8, nil
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

// SaveKeys256WithIDs is SaveKeys256 under key_IDs agreed with the peer, one per 256-bit chunk,
// so both ends of a link store the same key under the same ID
func SaveKeys256WithIDs(dir string, keyBytes []byte, ids []string) ([]StoredKey, error) {
	n := len(keyBytes) / ETSI014KeySizeBytes
	if len(ids) < n {
		return nil, fmt.Errorf("save keys: %d key_IDs for %d keys", len(ids), n)
	}
	stored := make([]StoredKey, 0, n)
	for i := range n {
		sk, err := saveKeyAs(dir, ids[i], keyBytes[i*ETSI014KeySizeBytes:(i+1)*ETSI014KeySizeBytes])
		if err != nil {
			return stored, err
		}
		stored = append(stored, sk)
	}
	return stored, nil
}

// SaveKey assigns a fresh key_ID to keyBytes and writes it as <dir>/<key_ID>.json in the ETSI 014 key container format,
// creating dir if it does not exist --> returns the stored record, key_ID included
func SaveKey(dir string, keyBytes []byte) (StoredKey, error) {
	keyID, err := newKeyID()
	if err != nil {
		return StoredKey{}, err
	}
	return saveKeyAs(dir, keyID, keyBytes)
}

// saveKeyAs writes one record under keyID, refusing to replace an existing one
func saveKeyAs(dir, keyID string, keyBytes []byte) (StoredKey, error) {
	if !validKeyID(keyID) {
		return StoredKey{}, fmt.Errorf("save key: invalid key_ID %q", keyID)
	}
	stored := StoredKey{
		KeyID: keyID,
		Key:   base64.StdEncoding.EncodeToString(keyBytes),
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return StoredKey{}, fmt.Errorf("save key %s: %w", keyID, err)
	}
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return StoredKey{}, fmt.Errorf("save key %s: %w", keyID, err)
	}
	// 0o600: key material, not group/world readable
	f, err := os.OpenFile(filepath.Join(dir, keyID+".json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		_, err = f.Write(data)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
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

// NewKeyID is a fresh random key_ID, for keys minted outside SaveKey
func NewKeyID() (string, error) { return newKeyID() }
