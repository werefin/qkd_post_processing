package qkd_post_processing

import (
	"crypto/rand"
	"testing"
)

// TestPolyMACRejectsWrongKeyLength: a key of any other size must be refused
func TestPolyMACRejectsWrongKeyLength(t *testing.T) {
	if _, err := ComputePolyMAC([]byte("msg"), make([]byte, 31)); err == nil {
		t.Fatal("expected an error for a 31-byte key")
	}
}

// TestPolyMACRoundTrips: the same message and key must verify
func TestPolyMACRoundTrips(t *testing.T) {
	key := make([]byte, PolyMACKeyBytes)
	rand.Read(key)
	msg := []byte("relay packet metadata plus ciphertext bytes")

	tag, err := ComputePolyMAC(msg, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(tag) != polyMACTagBytes {
		t.Fatalf("tag is %d bytes, want %d", len(tag), polyMACTagBytes)
	}
	if !VerifyPolyMAC(msg, tag, key) {
		t.Fatal("expected a matching tag to verify")
	}
}

// TestPolyMACHandlesEmptyAndMultiBlockMessages exercises both the zero-block
// path (tag = mask only) and the multi-block Horner loop
func TestPolyMACHandlesEmptyAndMultiBlockMessages(t *testing.T) {
	key := make([]byte, PolyMACKeyBytes)
	rand.Read(key)
	for _, n := range []int{0, 1, 15, 16, 17, 33, 200} {
		msg := make([]byte, n)
		rand.Read(msg)
		tag, err := ComputePolyMAC(msg, key)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if !VerifyPolyMAC(msg, tag, key) {
			t.Fatalf("n=%d: expected matching tag to verify", n)
		}
	}
}

// TestPolyMACDetectsMessageTampering: flipping one bit anywhere must fail
func TestPolyMACDetectsMessageTampering(t *testing.T) {
	key := make([]byte, PolyMACKeyBytes)
	rand.Read(key)
	msg := []byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x10, 0x11}

	tag, err := ComputePolyMAC(msg, key)
	if err != nil {
		t.Fatal(err)
	}
	for i := range msg {
		tampered := append([]byte(nil), msg...)
		tampered[i] ^= 0x01
		if VerifyPolyMAC(tampered, tag, key) {
			t.Fatalf("byte %d: tampered message verified under the original tag", i)
		}
	}
}

// TestPolyMACDetectsTagTampering: a corrupted tag must fail even though the
// message and key are untouched
func TestPolyMACDetectsTagTampering(t *testing.T) {
	key := make([]byte, PolyMACKeyBytes)
	rand.Read(key)
	msg := []byte("relay packet")
	tag, err := ComputePolyMAC(msg, key)
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), tag...)
	tampered[0] ^= 0x01
	if VerifyPolyMAC(msg, tampered, key) {
		t.Fatal("expected a corrupted tag to fail verification")
	}
}

// TestPolyMACRejectsWrongLengthTag
func TestPolyMACRejectsWrongLengthTag(t *testing.T) {
	key := make([]byte, PolyMACKeyBytes)
	rand.Read(key)
	if VerifyPolyMAC([]byte("msg"), []byte{0, 1, 2}, key) {
		t.Fatal("expected a short tag to be rejected")
	}
}

// TestPolyMACForgeryIsExtremelyRare: Poly1305's forgery bound is below
// 2^-100 per message, so a few thousand trials should see zero collisions
func TestPolyMACForgeryIsExtremelyRare(t *testing.T) {
	const trials = 5000
	msgA := []byte("original relayed key material, hop A to hop B")
	msgB := []byte("forged relayed key material, hop A to hop B!!!")

	collisions := 0
	for i := 0; i < trials; i++ {
		key := make([]byte, PolyMACKeyBytes)
		rand.Read(key)
		tagA, err := ComputePolyMAC(msgA, key)
		if err != nil {
			t.Fatal(err)
		}
		if VerifyPolyMAC(msgB, tagA, key) {
			collisions++
		}
	}
	if collisions != 0 {
		t.Fatalf("got %d collisions in %d trials, want 0", collisions, trials)
	}
}

// TestPolyMACDifferentKeysGiveDifferentTags: the same message under two
// independent keys should not produce the same tag (sanity, not a proof)
func TestPolyMACDifferentKeysGiveDifferentTags(t *testing.T) {
	msg := []byte("same message, different one-time keys")
	keyA := make([]byte, PolyMACKeyBytes)
	keyB := make([]byte, PolyMACKeyBytes)
	rand.Read(keyA)
	rand.Read(keyB)

	tagA, err := ComputePolyMAC(msg, keyA)
	if err != nil {
		t.Fatal(err)
	}
	tagB, err := ComputePolyMAC(msg, keyB)
	if err != nil {
		t.Fatal(err)
	}
	if string(tagA) == string(tagB) {
		t.Fatal("two independent keys produced the same tag (suspicious, re-run)")
	}
}
