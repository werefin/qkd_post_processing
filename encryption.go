package qoj_post_processing

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
)

const minKeyBytesForEncryption = 16 // 128 bits, a floor on usable entropy

// deriveAESKey expands the distilled secret key to a 32-byte AES-256 key
// via HKDF-SHA256, since its raw length rarely matches an AES key size
func deriveAESKey(key []byte) ([]byte, error) {
	if len(key) < minKeyBytesForEncryption {
		return nil, fmt.Errorf("key material too short for secure derivation: got %d bytes, want at least %d", len(key), minKeyBytesForEncryption)
	}
	return hkdf.Key(sha256.New, key, nil, "qoj_post_processing-aes-key", 32)
}

// EncryptWithKey encrypts plaintext under the distilled secret key with
// AES-256-GCM, returning nonce||ciphertext ready to send over any channel
func EncryptWithKey(key, plaintext []byte) ([]byte, error) {
	gcm, err := newGCMFromKey(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// DecryptWithKey reverses EncryptWithKey using the same distilled secret key
func DecryptWithKey(key, ciphertext []byte) ([]byte, error) {
	gcm, err := newGCMFromKey(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, fmt.Errorf("ciphertext shorter than nonce size")
	}
	nonce, ct := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, nil)
}

func newGCMFromKey(key []byte) (cipher.AEAD, error) {
	aesKey, err := deriveAESKey(key)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
