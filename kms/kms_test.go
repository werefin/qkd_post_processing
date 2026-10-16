package kms

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestKMS(t *testing.T) *KMS {
	t.Helper()
	master := make([]byte, 32)
	if _, err := rand.Read(master); err != nil {
		t.Fatal(err)
	}
	k, err := NewKMS(t.TempDir(), master)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestKMSPutGetRoundTrip
func TestKMSPutGetRoundTrip(t *testing.T) {
	k := newTestKMS(t)
	want := []byte("0123456789abcdef0123456789abcdef")

	keyID, err := k.Put(want, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got, err := k.Get(keyID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %x, want %x", got, want)
	}
}

// TestKMSGetConsumesKeyExactlyOnce: core ETSI 014 property -- a key_ID is single-delivery,
// so a replayed or retried request can never hand the same key material to two callers
func TestKMSGetConsumesKeyExactlyOnce(t *testing.T) {
	k := newTestKMS(t)
	keyID, err := k.Put([]byte("some key material, 32 bytes....."), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Get(keyID); err != nil {
		t.Fatalf("first Get: unexpected error: %v", err)
	}
	if _, err := k.Get(keyID); err != ErrKeyConsumed {
		t.Fatalf("second Get: got %v, want ErrKeyConsumed", err)
	}
}

// TestKMSGetUnknownKeyID
func TestKMSGetUnknownKeyID(t *testing.T) {
	k := newTestKMS(t)
	if _, err := k.Get("00000000-0000-4000-8000-000000000000"); err != ErrKeyNotFound {
		t.Fatalf("got %v, want ErrKeyNotFound", err)
	}
}

// TestKMSGetExpiredKey
func TestKMSGetExpiredKey(t *testing.T) {
	k := newTestKMS(t)
	keyID, err := k.Put([]byte("expiring key material, 32 bytes."), -time.Second) // already expired
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Get(keyID); err != ErrKeyExpired {
		t.Fatalf("got %v, want ErrKeyExpired", err)
	}
}

// TestKMSPutWithIDRejectsDuplicate: a key_ID must never bind to two different pieces of
// key material, which would let the master and slave sides silently disagree on the key
func TestKMSPutWithIDRejectsDuplicate(t *testing.T) {
	k := newTestKMS(t)
	keyID, err := k.Put([]byte("first key material, 32 bytes...."), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.PutWithID(keyID, []byte("second key material, 32 bytes..."), time.Hour); err == nil {
		t.Fatal("expected an error binding an existing key_ID to different material, got nil")
	}
}

// TestKMSKeyMaterialNotStoredInPlaintext: the whole point of at-rest
// encryption is that the raw key bytes never appear in the stored file
func TestKMSKeyMaterialNotStoredInPlaintext(t *testing.T) {
	dir := t.TempDir()
	master := make([]byte, 32)
	if _, err := rand.Read(master); err != nil {
		t.Fatal(err)
	}
	k, err := NewKMS(dir, master)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("THIS_SHOULD_NEVER_APPEAR_ON_DISK_UNENCRYPTED___")
	keyID, err := k.Put(secret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, keyID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, secret) {
		t.Fatal("key material appears in plaintext in the on-disk record")
	}
}

// TestKMSWrongMasterKeyFailsToDecrypt: a KMS opened with the wrong master
// key must not be able to recover key material it didn't encrypt
func TestKMSWrongMasterKeyFailsToDecrypt(t *testing.T) {
	dir := t.TempDir()
	master1 := make([]byte, 32)
	master2 := make([]byte, 32)
	rand.Read(master1)
	rand.Read(master2)

	k1, err := NewKMS(dir, master1)
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := k1.Put([]byte("some key material, 32 bytes....."), time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	k2, err := NewKMS(dir, master2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k2.Get(keyID); err == nil {
		t.Fatal("expected decryption to fail under the wrong master key, got nil error")
	}
}

// TestKMSNewKMSRejectsWrongMasterKeyLength
func TestKMSNewKMSRejectsWrongMasterKeyLength(t *testing.T) {
	if _, err := NewKMS(t.TempDir(), make([]byte, 16)); err == nil {
		t.Fatal("expected an error for a 16-byte master key, want exactly 32")
	}
}

// TestKMSGetNextDequeuesFIFOAndConsumes: order must match insertion order,
// and every returned key must be marked consumed, same as a direct Get
func TestKMSGetNextDequeuesFIFOAndConsumes(t *testing.T) {
	k := newTestKMS(t)
	var ids []string
	for i := 0; i < 5; i++ {
		id, err := k.Put([]byte(bytes.Repeat([]byte{byte('a' + i)}, 32)), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		time.Sleep(time.Millisecond) // force distinct CreatedAt ordering
	}

	got, err := k.GetNext(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d keys, want 3", len(got))
	}
	for i, dk := range got {
		if dk.KeyID != ids[i] {
			t.Fatalf("position %d: got key_ID %s, want %s (FIFO order)", i, dk.KeyID, ids[i])
		}
	}
	// those 3 are now consumed; a direct Get must refuse them
	if _, err := k.Get(ids[0]); err != ErrKeyConsumed {
		t.Fatalf("got %v, want ErrKeyConsumed for a key GetNext already delivered", err)
	}

	rest, err := k.GetNext(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 2 {
		t.Fatalf("got %d remaining keys, want 2", len(rest))
	}
}

// TestKMSStatusCounts
func TestKMSStatusCounts(t *testing.T) {
	k := newTestKMS(t)
	avail, _ := k.Put([]byte("available key material, 32 bytes"), time.Hour)
	consumed, _ := k.Put([]byte("consumed key material, 32 bytes."), time.Hour)
	k.Put([]byte("expired key material, 32 bytes.."), -time.Second)
	k.Get(consumed)

	st, err := k.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.StoredKeyCount != 1 || st.ConsumedKeyCount != 1 || st.ExpiredKeyCount != 1 {
		t.Fatalf("got %+v, want 1 stored, 1 consumed, 1 expired", st)
	}
	_ = avail
}

// TestKMSReservedKeyOnlyForItsPair: a reserved key is invisible to the pool and goes only to its pair
func TestKMSReservedKeyOnlyForItsPair(t *testing.T) {
	k := newTestKMS(t)
	key := bytes.Repeat([]byte{5}, 32)
	if err := k.PutReserved("k1", key, "alice", "bob", time.Hour); err != nil {
		t.Fatal(err)
	}
	if got, _ := k.GetNext(5); len(got) != 0 {
		t.Fatal("a reserved key was handed out by GetNext")
	}
	for _, pair := range [][2]string{{"", ""}, {"eve", "bob"}, {"alice", "eve"}, {"bob", "alice"}} {
		if _, err := k.GetReserved("k1", pair[0], pair[1]); err != ErrKeyReserved {
			t.Fatalf("pair %v: got %v, want ErrKeyReserved", pair, err)
		}
	}
	got, err := k.GetReserved("k1", "alice", "bob")
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("got %x, %v", got, err)
	}
	if _, err := k.GetReserved("k1", "alice", "bob"); err != ErrKeyConsumed {
		t.Fatalf("second delivery: got %v", err)
	}
}

// TestKMSDeliveredAndExpiredKeysAreWiped: no ciphertext survives delivery or a sweep, and the
// tombstone keeps the key_ID from ever being stored again
func TestKMSDeliveredAndExpiredKeysAreWiped(t *testing.T) {
	k := newTestKMS(t)
	k.PutWithID("used", bytes.Repeat([]byte{1}, 32), time.Hour)
	k.PutWithID("old", bytes.Repeat([]byte{2}, 32), -time.Second)
	k.Get("used")
	if n, err := k.Sweep(); err != nil || n != 1 {
		t.Fatalf("sweep wiped %d, %v", n, err)
	}
	for _, id := range []string{"used", "old"} {
		data, _ := os.ReadFile(filepath.Join(k.dir, id+".json"))
		if bytes.Contains(data, []byte("ciphertext")) {
			t.Fatalf("%s still holds ciphertext: %s", id, data)
		}
		if err := k.PutWithID(id, []byte("again"), time.Hour); err == nil {
			t.Fatalf("%s could be stored a second time", id)
		}
	}
}

// TestKMSGetBytesAllOrNothing: one bad key_ID in the list consumes none of the others
func TestKMSGetBytesAllOrNothing(t *testing.T) {
	k := newTestKMS(t)
	k.PutWithID("a", bytes.Repeat([]byte{1}, 32), time.Hour)
	k.PutWithID("b", bytes.Repeat([]byte{2}, 32), time.Hour)
	if _, err := k.GetBytes([]string{"a", "missing"}, 64); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := k.GetBytes([]string{"a", "a"}, 64); err == nil {
		t.Fatal("a repeated key_ID must be refused")
	}
	got, err := k.GetBytes([]string{"a", "b"}, 64)
	if err != nil || len(got) != 64 {
		t.Fatalf("a was consumed by the failed call: %v", err)
	}
}

// TestKMSIndexSurvivesReopen: a restarted store knows what was delivered and what is reserved
func TestKMSIndexSurvivesReopen(t *testing.T) {
	k := newTestKMS(t)
	k.PutWithID("free", bytes.Repeat([]byte{1}, 32), time.Hour)
	k.PutWithID("gone", bytes.Repeat([]byte{2}, 32), time.Hour)
	k.PutReserved("held", bytes.Repeat([]byte{3}, 32), "m", "s", time.Hour)
	k.Get("gone")
	k2, err := NewKMS(k.dir, k.masterKey)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := k2.Status()
	if st.StoredKeyCount != 1 || st.ConsumedKeyCount != 1 || st.ReservedKeyCount != 1 {
		t.Fatalf("got %+v", st)
	}
	if _, err := k2.GetReserved("held", "m", "s"); err != nil {
		t.Fatal(err)
	}
}

// TestOriginatorOfSplitsThePool: for any key_ID exactly one end of a link may originate with it
func TestOriginatorOfSplitsThePool(t *testing.T) {
	a, b := OriginatorOf("A", "B"), OriginatorOf("B", "A")
	nA := 0
	for i := 0; i < 1000; i++ {
		id, _ := newKeyID()
		if a(id) == b(id) {
			t.Fatalf("%s: both or neither end may use it", id)
		}
		if a(id) {
			nA++
		}
	}
	if nA < 400 || nA > 600 {
		t.Fatalf("A owns %d of 1000, want about half", nA)
	}
}
