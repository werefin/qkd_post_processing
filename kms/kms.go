// One encrypted key store (a link's pool or a node's delivery store): single delivery, expiry,
// per-key reservation for one SAE pair, at-rest encryption. One process per directory only
package kms

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	// ErrKeyReserved: the key belongs to one SAE pair and only that pair may take it
	ErrKeyReserved = errors.New("kms: key reserved for another SAE pair")
)

// kmsRecord is one key_ID's on-disk record; Ciphertext is the key under the master key and is
// wiped on delivery or expiry, leaving a tombstone so the key_ID can never be stored again
type kmsRecord struct {
	KeyID      string    `json:"key_ID"`
	Ciphertext string    `json:"ciphertext,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Consumed   bool      `json:"consumed"`
	ConsumedAt time.Time `json:"consumed_at,omitzero"`
	MasterSAE  string    `json:"master_SAE_ID,omitempty"`
	SlaveSAE   string    `json:"slave_SAE_ID,omitempty"`
}

// entry is the in-memory index of one record: everything but the key material
type entry struct {
	created, expires time.Time
	consumed, wiped  bool
	master, slave    string
}

func (e *entry) available(now time.Time) bool {
	return !e.consumed && !e.wiped && now.Before(e.expires)
}

// KMS is one store; Put mints a key_ID, PutWithID stores a key under an ID agreed with the peer
type KMS struct {
	dir       string
	masterKey []byte // 32-byte AES-256 key, operator-provisioned, not QKD-derived
	mu        sync.Mutex
	idx       map[string]*entry
}

// NewKMS opens a store at dir, encrypting records under masterKey (32 bytes; keep it outside this
// repo, losing it loses every stored key) and indexing what is already there
func NewKMS(dir string, masterKey []byte) (*KMS, error) {
	if len(masterKey) != 32 {
		return nil, fmt.Errorf("kms: master key must be exactly 32 bytes, got %d", len(masterKey))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("kms: %w", err)
	}
	k := &KMS{dir: dir, masterKey: masterKey, idx: map[string]*entry{}}
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("kms: %w", err)
	}
	for _, f := range files {
		if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
			continue
		}
		rec, err := k.read(strings.TrimSuffix(f.Name(), ".json"))
		if err != nil {
			return nil, fmt.Errorf("kms: %s: %w", f.Name(), err)
		}
		k.idx[rec.KeyID] = &entry{created: rec.CreatedAt, expires: rec.ExpiresAt, consumed: rec.Consumed,
			wiped: rec.Ciphertext == "", master: rec.MasterSAE, slave: rec.SlaveSAE}
	}
	return k, nil
}

func (k *KMS) path(keyID string) string {
	return filepath.Join(k.dir, keyID+".json")
}

// validKeyID keeps key_IDs usable as file names: no separators, no dot files
func validKeyID(id string) bool {
	if id == "" || len(id) > 128 || id[0] == '.' {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// Put mints a fresh key_ID for keyBytes; this side is "master" for this key
func (k *KMS) Put(keyBytes []byte, ttl time.Duration) (keyID string, err error) {
	keyID, err = newKeyID()
	if err != nil {
		return "", err
	}
	return keyID, k.PutWithID(keyID, keyBytes, ttl)
}

// PutWithID stores keyBytes under a peer-chosen key_ID; errors if that key_ID was ever stored,
// since one ID must never map to two different keys
func (k *KMS) PutWithID(keyID string, keyBytes []byte, ttl time.Duration) error {
	return k.PutReserved(keyID, keyBytes, "", "", ttl)
}

// PutReserved stores a key only master and slave may take, through GetReserved
func (k *KMS) PutReserved(keyID string, keyBytes []byte, master, slave string, ttl time.Duration) error {
	if !validKeyID(keyID) {
		return fmt.Errorf("kms: invalid key_ID %q", keyID)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.idx[keyID]; ok {
		return fmt.Errorf("kms: key_ID %s already exists", keyID)
	}
	ct, err := EncryptWithKey(k.masterKey, keyBytes)
	if err != nil {
		return fmt.Errorf("kms: encrypt key %s: %w", keyID, err)
	}
	now := time.Now()
	rec := kmsRecord{KeyID: keyID, Ciphertext: base64.StdEncoding.EncodeToString(ct), CreatedAt: now,
		ExpiresAt: now.Add(ttl), MasterSAE: master, SlaveSAE: slave}
	if err := k.write(rec); err != nil {
		return err
	}
	k.idx[keyID] = &entry{created: now, expires: rec.ExpiresAt, master: master, slave: slave}
	return nil
}

// write replaces the record atomically, so a crash leaves the old or the new record, never half
func (k *KMS) write(rec kmsRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("kms: %w", err)
	}
	tmp, err := os.CreateTemp(k.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("kms: %w", err)
	}
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), k.path(rec.KeyID))
	}
	if err != nil {
		os.Remove(tmp.Name())
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

// check reports why keyID cannot be taken by this master/slave pair, nil if it can; k.mu held
func (k *KMS) check(keyID, master, slave string) error {
	e, ok := k.idx[keyID]
	switch {
	case !ok:
		return ErrKeyNotFound
	case e.consumed:
		return ErrKeyConsumed
	case e.wiped || !time.Now().Before(e.expires):
		return ErrKeyExpired
	case e.master != master || e.slave != slave:
		return ErrKeyReserved
	}
	return nil
}

// take decrypts keyID, then wipes its material from disk; k.mu held and check passed
func (k *KMS) take(keyID string) ([]byte, error) {
	rec, err := k.read(keyID)
	if err != nil {
		return nil, err
	}
	ct, err := base64.StdEncoding.DecodeString(rec.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("kms: %w", err)
	}
	keyBytes, err := DecryptWithKey(k.masterKey, ct)
	if err != nil {
		return nil, fmt.Errorf("kms: decrypt key %s: %w", keyID, err)
	}
	rec.Ciphertext, rec.Consumed, rec.ConsumedAt = "", true, time.Now()
	if err := k.write(rec); err != nil {
		clear(keyBytes)
		return nil, err
	}
	e := k.idx[keyID]
	e.consumed, e.wiped = true, true
	return keyBytes, nil
}

// Get delivers an unreserved key once; a second Get returns ErrKeyConsumed
func (k *KMS) Get(keyID string) ([]byte, error) {
	return k.GetReserved(keyID, "", "")
}

// GetReserved delivers keyID once, only to the SAE pair it was reserved for
func (k *KMS) GetReserved(keyID, master, slave string) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.check(keyID, master, slave); err != nil {
		return nil, err
	}
	return k.take(keyID)
}

// CheckReserved is GetReserved without delivering, so a batch can be refused whole
func (k *KMS) CheckReserved(keyID, master, slave string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.check(keyID, master, slave)
}

// DeliveredKey is one key handed out by GetNext: its key_ID alongside the key material
type DeliveredKey struct {
	KeyID string
	Key   []byte
}

// GetNext dequeues up to n oldest unreserved available keys, consuming each; fewer than n if
// the pool is short, which is not an error
func (k *KMS) GetNext(n int) ([]DeliveredKey, error) {
	return k.GetNextMatching(n, nil)
}

// GetNextMatching is GetNext restricted to key_IDs match accepts (nil accepts all)
func (k *KMS) GetNextMatching(n int, match func(string) bool) ([]DeliveredKey, error) {
	if n <= 0 {
		return nil, nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	ids := k.availableIDs(match)
	if len(ids) > n {
		ids = ids[:n]
	}
	out := make([]DeliveredKey, 0, len(ids))
	for _, id := range ids {
		key, err := k.take(id)
		if err != nil {
			return out, err
		}
		out = append(out, DeliveredKey{KeyID: id, Key: key})
	}
	return out, nil
}

// availableIDs lists unreserved available keys oldest first; k.mu held
func (k *KMS) availableIDs(match func(string) bool) []string {
	now := time.Now()
	var ids []string
	for id, e := range k.idx {
		if e.available(now) && e.master == "" && e.slave == "" && (match == nil || match(id)) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := k.idx[ids[i]], k.idx[ids[j]]
		if !a.created.Equal(b.created) {
			return a.created.Before(b.created)
		}
		return ids[i] < ids[j]
	})
	return ids
}

// GetNextBytes takes whole pool keys until at least n bytes are collected, returning the first n
// plus every key_ID consumed in order; nothing is consumed if the pool cannot cover n
func (k *KMS) GetNextBytes(n int) ([]byte, []string, error) {
	return k.GetNextBytesMatching(n, nil)
}

// GetNextBytesMatching is GetNextBytes restricted to key_IDs match accepts
func (k *KMS) GetNextBytesMatching(n int, match func(string) bool) ([]byte, []string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	var buf []byte
	var ids []string
	for _, id := range k.availableIDs(match) {
		if len(buf) >= n {
			break
		}
		key, err := k.take(id)
		if err != nil {
			return nil, nil, err
		}
		buf = append(buf, key...)
		ids = append(ids, id)
	}
	if len(buf) < n {
		return nil, nil, fmt.Errorf("kms: pool exhausted, got %d of %d bytes needed", len(buf), n)
	}
	return buf[:n], ids, nil
}

// GetBytes reconstructs the material GetNextBytes delivered from the same key_IDs in order;
// every ID is checked before any is consumed, so a bad list consumes nothing
func (k *KMS) GetBytes(ids []string, n int) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return nil, fmt.Errorf("kms: key_ID %s listed twice", id)
		}
		seen[id] = true
		if err := k.check(id, "", ""); err != nil {
			return nil, fmt.Errorf("kms: %s: %w", id, err)
		}
	}
	var buf []byte
	for _, id := range ids {
		b, err := k.take(id)
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

// Status is a coarse view of the store, for the ETSI 014 status endpoint and the dashboard
type Status struct {
	StoredKeyCount   int `json:"stored_key_count"` // unreserved, unconsumed, unexpired
	ReservedKeyCount int `json:"reserved_key_count"`
	ConsumedKeyCount int `json:"consumed_key_count"`
	ExpiredKeyCount  int `json:"expired_key_count"`
}

// Status summarizes the store from the index
func (k *KMS) Status() (Status, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	var s Status
	now := time.Now()
	for _, e := range k.idx {
		switch {
		case e.consumed:
			s.ConsumedKeyCount++
		case e.wiped || !now.Before(e.expires):
			s.ExpiredKeyCount++
		case e.master != "" || e.slave != "":
			s.ReservedKeyCount++
		default:
			s.StoredKeyCount++
		}
	}
	return s, nil
}

// AvailableIDs lists unreserved available key_IDs match accepts, oldest first; IDs only
func (k *KMS) AvailableIDs(match func(string) bool) []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.availableIDs(match)
}

// CountAvailable counts unreserved available keys match accepts
func (k *KMS) CountAvailable(match func(string) bool) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.availableIDs(match))
}

// Sweep wipes the material of every expired key still on disk and returns how many it wiped;
// the tombstones stay, so an expired key_ID can never be stored again
func (k *KMS) Sweep() (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	now, n := time.Now(), 0
	for id, e := range k.idx {
		if e.consumed || e.wiped || now.Before(e.expires) {
			continue
		}
		rec, err := k.read(id)
		if err != nil {
			return n, err
		}
		rec.Ciphertext = ""
		if err := k.write(rec); err != nil {
			return n, err
		}
		e.wiped = true
		n++
	}
	return n, nil
}

// OriginatorOf splits a link's pool between its two ends: self may originate with a key_ID only
// if this returns true, and the peer's own filter is its exact complement, so no key is used twice
func OriginatorOf(self, peer string) func(string) bool {
	lower := self < peer
	return func(keyID string) bool {
		if keyID == "" {
			return false
		}
		return (hexNibble(keyID[0])%2 == 0) == lower
	}
}

func hexNibble(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	}
	return c
}
