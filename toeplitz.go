package qoj_post_processing

import (
	"crypto/rand"
	"math/bits"
	"runtime"
	"sync"
)

// bits are packed LSB-first: bit i lives in word[i/64] at
// position i%64 (bit 0 = word's LSB)

// packBits shifts by b&1 instead of branching on b!=0
// keeps random 0/1 input branch-free through the CPU pipeline
func packBits(bitsSlice []byte) []uint64 {
	words := make([]uint64, (len(bitsSlice)+63)/64)
	for i, b := range bitsSlice {
		words[i>>6] |= uint64(b&1) << uint(i&63)
	}
	return words
}

// packReversedBits packs x with its index order reversed in one pass
// avoids the separate O(n) reversedBits copy the naive version needed
func packReversedBits(bitsSlice []byte) []uint64 {
	n := len(bitsSlice)
	words := make([]uint64, (n+63)/64)
	for i, b := range bitsSlice {
		j := n - 1 - i
		words[j>>6] |= uint64(b&1) << uint(j&63)
	}
	return words
}

// wordAt returns words[idx], or 0 when idx is out of range
// lets the window extraction run past the seed without special-casing
func wordAt(words []uint64, idx int) uint64 {
	if idx < 0 || idx >= len(words) {
		return 0
	}
	return words[idx]
}

// extractWindowInto word-aligned-shifts len(dst) words starting at
// bitOffset --> only the zero-padded tail past words' end uses wordAt
func extractWindowInto(words []uint64, bitOffset int, dst []uint64) {
	wordIdx := bitOffset >> 6
	bitShift := uint(bitOffset & 63)
	nw := len(words)
	ndst := len(dst)

	if bitShift == 0 {
		safe := max(min(nw-wordIdx, ndst), 0)
		copy(dst[:safe], words[wordIdx:wordIdx+safe])
		for k := safe; k < ndst; k++ {
			dst[k] = wordAt(words, wordIdx+k)
		}
		return
	}

	safe := max(min(nw-wordIdx-1, ndst), 0)
	for k := range safe {
		dst[k] = (words[wordIdx+k] >> bitShift) | (words[wordIdx+k+1] << (64 - bitShift))
	}
	for k := safe; k < ndst; k++ {
		lo := wordAt(words, wordIdx+k)
		hi := wordAt(words, wordIdx+k+1)
		dst[k] = (lo >> bitShift) | (hi << (64 - bitShift))
	}
}

// toeplitzImpl is toeplitzHashPopcount everywhere, replaced with a hardware
// CLMUL implementation by init() in toeplitz_clmul_amd64.go when available
var toeplitzImpl = toeplitzHashPopcount

// toeplitzHashFast dispatches to the fastest implementation this CPU has
func toeplitzHashFast(x []byte, seed []byte, l int) []byte {
	return toeplitzImpl(x, seed, l)
}

// toeplitzHashPopcount: y_i = parity(popcount(seed[i:i+n] AND reverse(x)))
// word-packed POPCNT per output bit, l bits split across a worker pool
func toeplitzHashPopcount(x []byte, seed []byte, l int) []byte {
	n := len(x)
	if l == 0 || n == 0 {
		return make([]byte, l)
	}
	xWords := packReversedBits(x)
	seedWords := packBits(seed)
	numWords := len(xWords)

	out := make([]byte, l)

	// below this many word-comparisons, goroutine overhead costs more
	// than the parallel loop saves
	const minParallelPopcountWork = 4000000

	workers := runtime.GOMAXPROCS(0)
	if l*numWords < minParallelPopcountWork {
		workers = 1
	}
	workers = max(min(workers, l), 1)

	var wg sync.WaitGroup
	chunk := (l + workers - 1) / workers
	for w := range workers {
		start := w * chunk
		end := min(start+chunk, l)
		if start >= end {
			continue
		}
		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			win := make([]uint64, numWords)
			for i := start; i < end; i++ {
				extractWindowInto(seedWords, i, win)
				// 4 accumulators break the add-chain so out-of-order
				// execution overlaps POPCNT latencies, not just throughput
				var pc0, pc1, pc2, pc3 int
				k := 0
				for ; k+4 <= numWords; k += 4 {
					pc0 += bits.OnesCount64(win[k] & xWords[k])
					pc1 += bits.OnesCount64(win[k+1] & xWords[k+1])
					pc2 += bits.OnesCount64(win[k+2] & xWords[k+2])
					pc3 += bits.OnesCount64(win[k+3] & xWords[k+3])
				}
				pc := pc0 + pc1 + pc2 + pc3
				for ; k < numWords; k++ {
					pc += bits.OnesCount64(win[k] & xWords[k])
				}
				out[i] = byte(pc & 1)
			}
		}(start, end)
	}
	wg.Wait()
	return out
}

// toeplitzHashNaive is the O(l*n) reference oracle for tests only
// never use it in production, use toeplitzHashFast instead
func toeplitzHashNaive(x []byte, seed []byte, l int) []byte {
	n := len(x)
	out := make([]byte, l)
	for i := range l {
		var acc byte
		for j := range n {
			if x[j] == 0 {
				continue
			}
			acc ^= seed[i-j+n-1]
		}
		out[i] = acc
	}
	return out
}

// toeplitzSeed returns l+n-1 crypto-random 0/1 bytes
// shared with Bob over an authenticated classical channel
func toeplitzSeed(l, n int) ([]byte, error) {
	total := l + n - 1
	if total <= 0 {
		return nil, nil
	}
	raw := make([]byte, total)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	seed := make([]byte, total)
	for i, v := range raw {
		seed[i] = v & 1
	}
	return seed, nil
}
