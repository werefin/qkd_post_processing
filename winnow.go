package qoj_post_processing

import (
	"fmt"
	mathbits "math/bits"
	"math/rand"
)

// HammingGeneratorMatrix builds the minimal single-error-locating matrix
// Winnow needs: column j is binary(j+1), decoding a syndrome directly
func HammingGeneratorMatrix(blockSize int) (GeneratorMatrix, error) {
	if blockSize <= 0 {
		return nil, fmt.Errorf("hamming: blockSize must be > 0")
	}
	p := mathbits.Len(uint(blockSize))
	rows := make([][]byte, p)
	for i := range rows {
		rows[i] = make([]byte, blockSize)
	}
	for j := 0; j < blockSize; j++ {
		col := j + 1
		for i := range p {
			if col&(1<<uint(i)) != 0 {
				rows[i][j] = 1
			}
		}
	}
	return GeneratorMatrix(rows), nil
}

// syndromeToPosition reads a p-bit syndrome as an integer and returns the
// 0-indexed column it names, or -1 for the all-zero (no error) syndrome
func syndromeToPosition(syndrome []byte) int {
	v := 0
	for i, b := range syndrome {
		v |= int(b&1) << uint(i)
	}
	return v - 1
}

// WinnowCorrect flips at most one bit per block wherever its own syndrome
// disagrees with referenceSyndrome --> the reference side must stay untouched
func WinnowCorrect(ownBits []byte, g GeneratorMatrix, referenceSyndrome [][]byte) (corrected []byte, flipped int) {
	if len(g[0]) == 0 {
		panic("generator matrix must have at least one column")
	}
	blockSize := g.BlockSize()
	ownSyndrome := ComputeBlockParity(ownBits, g)
	if len(ownSyndrome) != len(referenceSyndrome) {
		panic("ownSyndrome and referenceSyndrome must cover the same number of blocks")
	}

	corrected = append([]byte(nil), ownBits...)
	n := len(corrected)
	diff := make([]byte, g.ParityBits())
	for bi := range ownSyndrome {
		for i := range diff {
			diff[i] = ownSyndrome[bi][i] ^ referenceSyndrome[bi][i]
		}
		pos := syndromeToPosition(diff)
		if pos < 0 || pos >= blockSize {
			continue // no error, or a syndrome outside this shortened block's real columns
		}
		idx := bi*blockSize + pos
		if idx < n {
			corrected[idx] ^= 1
			flipped++
		}
	}
	return corrected, flipped
}

// WinnowPassResult summarizes one single-pass correction attempt
type WinnowPassResult struct {
	CorrectedBob []byte
	Corrections  int
	LeakedBits   int // classical-channel bits spent announcing Alice's syndrome
}

// WinnowPass is a convenience wrapper for simulation and testing
// use ComputeBlockParity + WinnowCorrect directly to run each side apart
func WinnowPass(alice, bob []byte, g GeneratorMatrix) WinnowPassResult {
	aliceSyndrome := ComputeBlockParity(alice, g)
	correctedBob, flipped := WinnowCorrect(bob, g, aliceSyndrome)
	return WinnowPassResult{
		CorrectedBob: correctedBob,
		Corrections:  flipped,
		LeakedBits:   len(aliceSyndrome) * g.ParityBits(),
	}
}

// WinnowConfig is one multi-pass run: one block size and permutation
// per pass, so each regroups bits differently than the last
type WinnowConfig struct {
	BlockSizes   []int
	Permutations [][]int
}

// WinnowReconcile runs every configured pass, permuting before and
// restoring order after each one, correcting Bob's copy toward Alice's
func WinnowReconcile(alice, bob []byte, cfg WinnowConfig) (correctedBob []byte, totalLeaked, totalCorrections int) {
	if len(cfg.BlockSizes) != len(cfg.Permutations) {
		panic("WinnowConfig: one permutation required per pass")
	}
	correctedBob = append([]byte(nil), bob...)
	for p, blockSize := range cfg.BlockSizes {
		g, err := HammingGeneratorMatrix(blockSize)
		if err != nil {
			panic(err)
		}
		perm := cfg.Permutations[p]
		res := WinnowPass(permuteBits(alice, perm), permuteBits(correctedBob, perm), g)
		correctedBob = unpermuteBits(res.CorrectedBob, perm)
		totalLeaked += res.LeakedBits
		totalCorrections += res.Corrections
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
