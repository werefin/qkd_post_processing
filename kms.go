// A lifecycle-aware key store (ETSI GS QKD 014's Get Key / Get Key with Key
// IDs): single delivery, expiry, at-rest encryption. One writer per directory only
package qkd_post_processing

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

var (
	// ErrKeyNotFound: no record exists for this key_ID
	ErrKeyNotFound = errors.New("kms: key_ID not found")
	// ErrKeyConsumed: key_IDs are single-delivery, a repeat request is refused
	ErrKeyConsumed = errors.New("kms: key already delivered")
	// ErrKeyExpired: the key's retention window has passed
	ErrKeyExpired = errors.New("kms: key expired")
)

// kmsRecord is one key_ID's on-disk record; Ciphertext is EncryptWithKey's
// output under the KMS's own master key, never the raw key material
type kmsRecord struct {
	KeyID      string    `json:"key_ID"`
	Ciphertext string    `json:"ciphertext"` // base64, AES-256-GCM under the master key
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Consumed   bool      `json:"consumed"`
	ConsumedAt time.Time `json:"consumed_at,omitzero"`
}

// KMS is one site's key store; the Put side mints a key_ID, the peer
// stores its own already-identical key under that ID via PutWithID
type KMS struct {
	dir       string
	masterKey []byte // 32-byte AES-256 key, operator-provisioned, not QKD-derived
	mu        sync.Mutex
}

// NewKMS opens a store at dir, encrypting records under masterKey (32
// bytes; keep it outside this repo -- losing it loses every stored key)
func NewKMS(dir string, masterKey []byte) (*KMS, error) {
	if len(masterKey) != 32 {
		return nil, fmt.Errorf("kms: master key must be exactly 32 bytes, got %d", len(masterKey))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("kms: %w", err)
	}
	return &KMS{dir: dir, masterKey: masterKey}, nil
}

func (k *KMS) path(keyID string) string {
	return filepath.Join(k.dir, keyID+".json")
}

// Put mints a fresh key_ID for keyBytes; this side is "master" for this key
func (k *KMS) Put(keyBytes []byte, ttl time.Duration) (keyID string, err error) {
	keyID, err = newKeyID()
	if err != nil {
		return "", err
	}
	return keyID, k.PutWithID(keyID, keyBytes, ttl)
}

// PutWithID stores keyBytes under a peer-chosen key_ID; errors if that
// key_ID already exists, since one ID must never map to two different keys
func (k *KMS) PutWithID(keyID string, keyBytes []byte, ttl time.Duration) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	if _, err := os.Stat(k.path(keyID)); err == nil {
		return fmt.Errorf("kms: key_ID %s already exists", keyID)
	}

	ct, err := EncryptWithKey(k.masterKey, keyBytes)
	if err != nil {
		return fmt.Errorf("kms: encrypt key %s: %w", keyID, err)
	}
	now := time.Now()
	rec := kmsRecord{
		KeyID:      keyID,
		Ciphertext: base64.StdEncoding.EncodeToString(ct),
		CreatedAt:  now,
		ExpiresAt:  now.Add(ttl),
	}
	return k.write(rec)
}

func (k *KMS) write(rec kmsRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("kms: %w", err)
	}
	// 0o600: key material (encrypted, but still), not group/world readable
	if err := os.WriteFile(k.path(rec.KeyID), data, 0o600); err != nil {
		return fmt.Errorf("kms: %w", err)
	}
	return nil
}

func (k *KMS) read(keyID string) (kmsRecord, error) {
	data, err := os.ReadFile(k.path(keyID))
	if err != nil {
		if os.IsNotExist(err) {
			return kmsRecord{}, ErrKeyNotFound
		}
		return kmsRecord{}, fmt.Errorf("kms: %w", err)
	}
	var rec kmsRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return kmsRecord{}, fmt.Errorf("kms: %w", err)
	}
	return rec, nil
}

// Get decrypts keyID's material and marks it consumed; a second Get
// returns ErrKeyConsumed, so at most one caller ever receives the key
func (k *KMS) Get(keyID string) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	rec, err := k.read(keyID)
	if err != nil {
		return nil, err
	}
	if rec.Consumed {
		return nil, ErrKeyConsumed
	}
	if time.Now().After(rec.ExpiresAt) {
		return nil, ErrKeyExpired
	}

	ct, err := base64.StdEncoding.DecodeString(rec.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("kms: %w", err)
	}
	keyBytes, err := DecryptWithKey(k.masterKey, ct)
	if err != nil {
		return nil, fmt.Errorf("kms: decrypt key %s: %w", keyID, err)
	}

	rec.Consumed = true
	rec.ConsumedAt = time.Now()
	if err := k.write(rec); err != nil {
		return nil, err
	}
	return keyBytes, nil
}

// DeliveredKey is one key handed out by GetNext: its key_ID (to be sent to
// the peer SAE out of band) alongside the decrypted key material
type DeliveredKey struct {
	KeyID string
	Key   []byte
}

// GetNext dequeues up to n oldest available keys FIFO, consuming each --
// ETSI 014's Get Key. Returns fewer than n if the pool is short, not an error
func (k *KMS) GetNext(n int) ([]DeliveredKey, error) {
	if n <= 0 {
		return nil, nil
	}
	k.mu.Lock()
	entries, err := os.ReadDir(k.dir)
	if err != nil {
		k.mu.Unlock()
		return nil, fmt.Errorf("kms: %w", err)
	}

	type candidate struct {
		keyID     string
		createdAt time.Time
	}
	var candidates []candidate
	now := time.Now()
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		keyID := e.Name()[:len(e.Name())-len(".json")]
		rec, err := k.read(keyID)
		if err != nil || rec.Consumed || now.After(rec.ExpiresAt) {
			continue
		}
		candidates = append(candidates, candidate{keyID, rec.CreatedAt})
	}
	k.mu.Unlock() // Get takes its own lock below; re-acquiring here would deadlock

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].createdAt.Before(candidates[j].createdAt) })
	if len(candidates) > n {
		candidates = candidates[:n]
	}

	out := make([]DeliveredKey, 0, len(candidates))
	for _, c := range candidates {
		key, err := k.Get(c.keyID)
		if err != nil {
			continue // consumed/expired between the scan and here; skip, don't fail the batch
		}
		out = append(out, DeliveredKey{KeyID: c.keyID, Key: key})
	}
	return out, nil
}

// GetNextBytes pulls whole pool keys via GetNext until at least n bytes are
// collected, returning the first n plus every key_ID consumed, in order
func (k *KMS) GetNextBytes(n int) ([]byte, []string, error) {
	var buf []byte
	var ids []string
	for len(buf) < n {
		got, err := k.GetNext(1)
		if err != nil {
			return nil, nil, err
		}
		if len(got) == 0 {
			return nil, nil, fmt.Errorf("kms: pool exhausted, got %d of %d bytes needed", len(buf), n)
		}
		buf = append(buf, got[0].Key...)
		ids = append(ids, got[0].KeyID)
	}
	return buf[:n], ids, nil
}

// GetBytes reconstructs the same material GetNextBytes delivered, given
// the same key_IDs in order -- each ID is still single-delivery via Get
func (k *KMS) GetBytes(ids []string, n int) ([]byte, error) {
	var buf []byte
	for _, id := range ids {
		b, err := k.Get(id)
		if err != nil {
			return nil, fmt.Errorf("kms: %s: %w", id, err)
		}
		buf = append(buf, b...)
	}
	if len(buf) < n {
		return nil, fmt.Errorf("kms: reconstructed %d bytes, want %d", len(buf), n)
	}
	return buf[:n], nil
}

// Status is a coarse view of the store's pool, for the ETSI 014 status endpoint
type Status struct {
	StoredKeyCount   int `json:"stored_key_count"` // unconsumed, unexpired
	ConsumedKeyCount int `json:"consumed_key_count"`
	ExpiredKeyCount  int `json:"expired_key_count"`
}

// Status scans and summarizes the store; O(n), fine at this pipeline's key volumes
func (k *KMS) Status() (Status, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	entries, err := os.ReadDir(k.dir)
	if err != nil {
		return Status{}, fmt.Errorf("kms: %w", err)
	}
	var s Status
	now := time.Now()
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		keyID := e.Name()[:len(e.Name())-len(".json")]
		rec, err := k.read(keyID)
		if err != nil {
			continue
		}
		switch {
		case rec.Consumed:
			s.ConsumedKeyCount++
		case now.After(rec.ExpiresAt):
			s.ExpiredKeyCount++
		default:
			s.StoredKeyCount++
		}
	}
	return s, nil
}
