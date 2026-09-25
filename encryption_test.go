package qoj_post_processing

import (
	"bytes"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	plaintext := []byte("the quick brown fox jumps over the lazy dog")

	ct, err := EncryptWithKey(key, plaintext)
	if err != nil {
		t.Fatalf("EncryptWithKey: %v", err)
	}
	if bytes.Contains(ct, plaintext) {
		t.Fatal("ciphertext contains the plaintext verbatim")
	}
	pt, err := DecryptWithKey(key, ct)
	if err != nil {
		t.Fatalf("DecryptWithKey: %v", err)
	}
	if !bytes.Equal(pt, plaintext) {
		t.Fatalf("round trip mismatch: got %q, want %q", pt, plaintext)
	}
}

func TestDecryptFailsWithWrongKey(t *testing.T) {
	key := make([]byte, 32)
	wrongKey := make([]byte, 32)
	wrongKey[0] = 1
	ct, err := EncryptWithKey(key, []byte("secret"))
	if err != nil {
		t.Fatalf("EncryptWithKey: %v", err)
	}
	if _, err := DecryptWithKey(wrongKey, ct); err == nil {
		t.Fatal("expected decryption to fail with the wrong key")
	}
}

func TestDecryptFailsOnTamperedCiphertext(t *testing.T) {
	key := make([]byte, 32)
	ct, err := EncryptWithKey(key, []byte("secret"))
	if err != nil {
		t.Fatalf("EncryptWithKey: %v", err)
	}
	ct[len(ct)-1] ^= 1
	if _, err := DecryptWithKey(key, ct); err == nil {
		t.Fatal("expected decryption to fail on tampered ciphertext")
	}
}

func TestEncryptWithKeyRejectsShortKey(t *testing.T) {
	_, err := EncryptWithKey(make([]byte, 8), []byte("secret"))
	if err == nil {
		t.Fatal("expected an error for key material shorter than the minimum")
	}
}
