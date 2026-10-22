package qkd

import "math/rand"

// QBERResult is an error count over the bits it was measured on
type QBERResult struct {
	ErrorBits  int
	SampleBits int
	QBER       float64
}

// siftSampleOrder is the public random permutation choosing which sifted bits are sacrificed
func siftSampleOrder(n int, seed int64) []int {
	return rand.New(rand.NewSource(seed)).Perm(n)
}
