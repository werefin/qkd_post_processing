package qoj_post_processing

import "math"

// binaryEntropy is h2(p), the Shannon binary entropy function, clamped to
// [0,1] input and returning 0 at the boundaries
func binaryEntropy(p float64) float64 {
	if p <= 0 || p >= 1 {
		return 0
	}
	return -p*math.Log2(p) - (1-p)*math.Log2(1-p)
}

// SecureKeyLength: Devetak-Winter rate 1-2*h2(QBER) applied to n bits,
// minus leakedBits already revealed on the classical channel
func SecureKeyLength(n int, qber float64, leakedBits int) int {
	if n <= 0 {
		return 0
	}
	rate := 1 - 2*binaryEntropy(qber)
	l := rate*float64(n) - float64(leakedBits)
	if l < 0 {
		return 0
	}
	li := int(math.Floor(l))
	if li > n {
		li = n
	}
	return li
}

// PrivacyAmplify runs step 5: distills the error-free stream into the
// final secret key via Toeplitz hashing, returning the key and seed used
func PrivacyAmplify(errorFreeBits []byte, qber float64, leakedBits int) (key []byte, seed []byte, err error) {
	n := len(errorFreeBits)
	l := SecureKeyLength(n, qber, leakedBits)
	if l == 0 {
		return nil, nil, nil
	}
	seed, err = toeplitzSeed(l, n)
	if err != nil {
		return nil, nil, err
	}
	key = toeplitzHashFast(errorFreeBits, seed, l)
	return key, seed, nil
}

// packToBytes turns 0/1 bit-per-byte slices into a compact []byte (MSB
// first), convenient for e.g. printing the final key as hex
func packToBytes(bitsSlice []byte) []byte {
	return bitsToBytes(bitsSlice)
}
