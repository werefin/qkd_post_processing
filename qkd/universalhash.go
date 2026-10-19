// Universal-hash correctness check: unlike a fixed CRC, a random-seed
// Toeplitz hash bounds epsCor exactly (doc/finite_key_procedure.md)
package qkd

import (
	"crypto/subtle"
	"math"
)

// UniversalHashTagBits: tagBits = ceil(log2(1/epsCor)), the collision bound needed
func UniversalHashTagBits(epsCor float64) int {
	if epsCor <= 0 || epsCor >= 1 {
		return 0
	}
	return int(math.Ceil(math.Log2(1 / epsCor)))
}

// ComputeUniversalHashTag hashes bits to tagBits bits under seed; both
// sides must use the identical seed, agreed over the authenticated channel
func ComputeUniversalHashTag(bits []byte, seed []byte, tagBits int) []byte {
	return toeplitzHashFast(bits, seed, tagBits)
}

// UniversalHashSeed draws a fresh seed; never reuse the privacy-amplification seed here
func UniversalHashSeed(tagBits, n int) ([]byte, error) {
	return toeplitzSeed(tagBits, n)
}

// VerifyUniversalHash: a match gives correctness 1-2^-tagBits; a mismatch
// means the two sides differ and the block must be discarded before privacy amplification
func VerifyUniversalHash(ownBits []byte, peerTag []byte, seed []byte, tagBits int) bool {
	ownTag := ComputeUniversalHashTag(ownBits, seed, tagBits)
	if len(ownTag) != len(peerTag) {
		return false
	}
	return subtle.ConstantTimeCompare(ownTag, peerTag) == 1
}
