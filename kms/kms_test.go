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
