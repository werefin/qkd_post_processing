package qoj_post_processing

import (
	"runtime"
	"sync"
)

// clmul64 computes the 128-bit carry-less (GF(2)) product of a and b
// implemented in toeplitz_clmul_amd64.s via the PCLMULQDQ instruction
func clmul64(a, b uint64) (hi, lo uint64)

// hasCLMUL reports whether this CPU supports PCLMULQDQ
// implemented in toeplitz_clmul_amd64.s via CPUID
func hasCLMUL() bool

func init() {
	if hasCLMUL() {
		toeplitzImpl = toeplitzHashCLMUL
	}
}

// toeplitzHashCLMUL: y_i = coeff of t^(i+n-1) in X(t)*S(t), one schoolbook
// GF(2) poly multiply via hardware CLMUL instead of l popcount-correlations
func toeplitzHashCLMUL(x []byte, seed []byte, l int) []byte {
	n := len(x)
	if l == 0 || n == 0 {
		return make([]byte, l)
	}
	xWords := packBits(x)
	seedWords := packBits(seed)

	prodWords := len(xWords) + len(seedWords)
	product := schoolbookCLMUL(xWords, seedWords, prodWords)

	out := make([]byte, l)
	extractBitsInto(product, n-1, out)
	return out
}

// extractBitsInto reads len(dst) individual 0/1 bits from words starting
// at bitOffset, one bit per output byte
func extractBitsInto(words []uint64, bitOffset int, dst []byte) {
	for i := range dst {
		pos := bitOffset + i
		dst[i] = byte((words[pos>>6] >> uint(pos&63)) & 1)
	}
}

// minParallelCLMULWork is the outer*inner word-product size below which
// goroutine/allocation overhead costs more than parallelizing saves
const minParallelCLMULWork = 16384

// schoolbookCLMUL returns the GF(2) polynomial product of xWords and
// seedWords, split across a worker pool along the larger operand
func schoolbookCLMUL(xWords, seedWords []uint64, prodWords int) []uint64 {
	outer, inner := xWords, seedWords
	if len(seedWords) > len(xWords) {
		outer, inner = seedWords, xWords
	}
	outerLen := len(outer)

	workers := runtime.GOMAXPROCS(0)
	if outerLen*len(inner) < minParallelCLMULWork {
		workers = 1
	}
	workers = max(min(workers, outerLen), 1)
	if workers == 1 {
		product := make([]uint64, prodWords)
		clmulAccumulate(outer, inner, product, 0, outerLen)
		return product
	}

	partials := make([][]uint64, workers)
	var wg sync.WaitGroup
	chunk := (outerLen + workers - 1) / workers
	for w := range workers {
		start := w * chunk
		end := min(start+chunk, outerLen)
		if start >= end {
			continue
		}
		partials[w] = make([]uint64, prodWords)
		wg.Add(1)
		go func(partial []uint64, start, end int) {
			defer wg.Done()
			clmulAccumulate(outer, inner, partial, start, end)
		}(partials[w], start, end)
	}
	wg.Wait()

	product := make([]uint64, prodWords)
	for _, p := range partials {
		if p == nil {
			continue
		}
		for i, v := range p {
			product[i] ^= v
		}
	}
	return product
}

// clmulAccumulate XORs the carry-less product of outer[start:end] against
// every word of inner into dst, positioned by word-index sum
func clmulAccumulate(outer, inner, dst []uint64, start, end int) {
	for a := start; a < end; a++ {
		wa := outer[a]
		for b, wb := range inner {
			hi, lo := clmul64(wa, wb)
			dst[a+b] ^= lo
			dst[a+b+1] ^= hi
		}
	}
}
