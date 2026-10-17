package qkd

import (
	"math"
	mathbits "math/bits"
	"math/rand"
)

// winnowErrorsPerBlock is the expected number of errors in a first-pass block: low enough that most
// mismatched blocks hold exactly one, high enough that the parities are not mostly wasted
const winnowErrorsPerBlock = 0.65

// winnowMaxFirstBlock caps the first block: larger ones look cheaper but leave errors behind at low QBER
const winnowMaxFirstBlock = 16

// winnowMaxPasses bounds a run; passes stop earlier once one finds every parity equal
const winnowMaxPasses = 24

// WinnowSchedule is the block size of each pass for a public QBER estimate: a power of two holding
// about winnowErrorsPerBlock errors, then doubling, up to a quarter of the bits
func WinnowSchedule(qber float64, n int) []int {
	q := math.Max(qber, 1e-4)
	b := 4
	for b*2 <= winnowMaxFirstBlock && float64(b*2)*q <= winnowErrorsPerBlock {
		b *= 2
	}
	sizes := []int{b}
	for len(sizes) < winnowMaxPasses && b*2 <= max(n/4, 8) {
		b *= 2
		sizes = append(sizes, b)
	}
	return sizes
}

// WinnowConfig is one run: a block size per pass; passes past the schedule repeat its last size while
// mismatches remain. Permutations, if given, are used per pass, else derived from Seed
type WinnowConfig struct {
	BlockSizes   []int
	Permutations [][]int
	Seed         int64
}

// WinnowPassResult summarizes one pass
type WinnowPassResult struct {
	CorrectedBob []byte
	Corrections  int
	Mismatched   int // blocks whose parities differed
	LeakedBits   int // parities, plus a syndrome per mismatched block
}

// blockSyndrome is the XOR of the in-block positions holding a one: the Hamming syndrome of a block of
// up to 2^m bits, position 0 included, which the block parity covers
func blockSyndrome(bits []byte) (syndrome int, parity byte) {
	for j, v := range bits {
		if v&1 == 1 {
			syndrome ^= j
			parity ^= 1
		}
	}
	return syndrome, parity
}

// WinnowPass runs one pass on bits already shuffled: parity per block, syndrome where they differ
func WinnowPass(alice, bob []byte, blockSize int) WinnowPassResult {
	res := WinnowPassResult{CorrectedBob: append([]byte(nil), bob...)}
	for start := 0; start < len(alice); start += blockSize {
		end := min(start+blockSize, len(alice))
		sa, pa := blockSyndrome(alice[start:end])
		sb, pb := blockSyndrome(res.CorrectedBob[start:end])
		res.LeakedBits++ // Alice's parity
		if pa == pb {
			continue // no error, or an even number left for a later pass
		}
		res.Mismatched++
		// the syndrome takes as many bits as the block's last position needs
		res.LeakedBits += mathbits.Len(uint(end - start - 1))
		if pos := sa ^ sb; pos < end-start {
			res.CorrectedBob[start+pos] ^= 1
			res.Corrections++
		}
	}
	return res
}

// WinnowReconcile runs the passes, shuffling before and restoring order after each, and keeps repeating
// the last block size while a pass still finds mismatches, up to winnowMaxPasses
func WinnowReconcile(alice, bob []byte, cfg WinnowConfig) (correctedBob []byte, totalLeaked, totalCorrections int) {
	correctedBob = append([]byte(nil), bob...)
	if len(cfg.BlockSizes) == 0 || len(alice) == 0 {
		return correctedBob, 0, 0
	}
	for p := 0; p < winnowMaxPasses; p++ {
		blockSize := cfg.BlockSizes[min(p, len(cfg.BlockSizes)-1)]
		var perm []int
		if p < len(cfg.Permutations) && len(cfg.Permutations[p]) == len(alice) {
			perm = cfg.Permutations[p]
		} else {
			perm = GeneratePermutation(len(alice), cfg.Seed+1+int64(p))
		}
		res := WinnowPass(permuteBits(alice, perm), permuteBits(correctedBob, perm), blockSize)
		correctedBob = unpermuteBits(res.CorrectedBob, perm)
		totalLeaked += res.LeakedBits
		totalCorrections += res.Corrections
		if p >= len(cfg.BlockSizes)-1 && res.Mismatched == 0 {
			break // the schedule is done and the last pass found every parity equal
		}
	}
	return correctedBob, totalLeaked, totalCorrections
}

// GeneratePermutation derives a Fisher-Yates shuffle of [0,n) from seed
// both sides call this with the same publicly-agreed seed for free sync
func GeneratePermutation(n int, seed int64) []int {
	perm := make([]int, n)
	for i := range perm {
		perm[i] = i
	}
	r := rand.New(rand.NewSource(seed))
	r.Shuffle(n, func(i, j int) { perm[i], perm[j] = perm[j], perm[i] })
	return perm
}

// permuteBits builds dst[i] = data[perm[i]], regrouping bits into blocks
// that cut across the previous pass's block boundaries
func permuteBits(data []byte, perm []int) []byte {
	out := make([]byte, len(data))
	for i, p := range perm {
		out[i] = data[p]
	}
	return out
}

// unpermuteBits reverses permuteBits, restoring dst[perm[i]] = data[i]
func unpermuteBits(data []byte, perm []int) []byte {
	out := make([]byte, len(data))
	for i, p := range perm {
		out[p] = data[i]
	}
	return out
}
