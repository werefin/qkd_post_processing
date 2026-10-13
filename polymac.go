// Poly1305 (RFC 8439), the standard audited constant-time implementation,
// keyed with a single-use QKD-derived key so the MAC stays information-theoretic
package qkd_post_processing

import (
	"crypto/subtle"
	"fmt"

	"golang.org/x/crypto/poly1305"
)

// PolyMACKeyBytes: the 32-byte one-time key RFC 8439 Poly1305 takes
const PolyMACKeyBytes = 32

// polyMACTagBytes is Poly1305's fixed output size
const polyMACTagBytes = poly1305.TagSize

// ComputePolyMAC tags message under a single-use 32-byte key
func ComputePolyMAC(message, key []byte) ([]byte, error) {
	if len(key) != PolyMACKeyBytes {
		return nil, fmt.Errorf("polymac: key must be %d bytes, got %d", PolyMACKeyBytes, len(key))
	}
	var k [PolyMACKeyBytes]byte
	copy(k[:], key)
	var tag [polyMACTagBytes]byte
	poly1305.Sum(&tag, message, &k)
	return tag[:], nil
}

// VerifyPolyMAC recomputes the tag and compares in constant time
func VerifyPolyMAC(message, tag, key []byte) bool {
	expected, err := ComputePolyMAC(message, key)
	if err != nil || len(tag) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare(expected, tag) == 1
}
