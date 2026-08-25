package qkdpostproc

import (
	"fmt"
	"math/bits"
	"runtime"
	"sync"
)

// step 2 detects errors only, never corrects a bit, each side computes p parity bits per m-bit block

// GeneratorMatrix is the p x m parity matrix G: row i computes parity
// bit i as the XOR of the message bits it selects
type GeneratorMatrix [][]byte

// BlockSize returns m, the number of message bits G expects per block
func (g GeneratorMatrix) BlockSize() int { return len(g[0]) }

// ParityBits returns p, the number of parity bits G produces per block
func (g GeneratorMatrix) ParityBits() int { return len(g) }

// GeneratorMatrixEx1/Ex2/Ex3 are three example matrices with different
// code-rate/detection-rate tradeoffs (m=4/m=4/m=6, p=3 each)
var (
	// GeneratorMatrixEx1: code rate 4/7, detects 100% of 1/2-bit and 75%
	// of 3-bit errors, 100% of 4-bit errors
	GeneratorMatrixEx1 = GeneratorMatrix{
		{1, 0, 1, 0},
		{1, 1, 0, 1},
		{0, 0, 1, 1},
	}
	// GeneratorMatrixEx2: same shape as Ex1, trades 4-bit detection (0%)
	// for 100% 3-bit detection
	GeneratorMatrixEx2 = GeneratorMatrix{
		{1, 1, 1, 1},
		{1, 1, 1, 0},
		{0, 1, 1, 0},
	}
	// GeneratorMatrixEx3: code rate 2/3, 88.9% of all bit errors detected
	// overall
	GeneratorMatrixEx3 = GeneratorMatrix{
		{0, 0, 0, 1, 1, 1},
		{1, 1, 0, 1, 0, 0},
		{1, 0, 1, 1, 1, 0},
	}
)

// CascadeGeneratorMatrices stacks g1's rows on g2's, catching collisions
// unique to g1 alone - needs equal block sizes and p1+p2 < m
func CascadeGeneratorMatrices(g1, g2 GeneratorMatrix) (GeneratorMatrix, error) {
	m := g1.BlockSize()
	if g2.BlockSize() != m {
		return nil, fmt.Errorf("cascade: g1 block size %d != g2 block size %d", m, g2.BlockSize())
	}
	p := g1.ParityBits() + g2.ParityBits()
	if p >= m {
		return nil, fmt.Errorf("cascade: combined parity bits %d >= block size %d, no secret would remain", p, m)
	}
	combined := make(GeneratorMatrix, 0, p)
	combined = append(combined, g1...)
	combined = append(combined, g2...)
	return combined, nil
}

// parityMasks precomputes each row of G as a bit-mask, turning the
// M*G^T parity computation into p AND+POPCNT passes per block
type parityMasks struct {
	words int
	rows  [][]uint64
}

// buildParityMasks derives the bit-masks for one G once, so every block
// reuses them - only the words scratch buffer differs per call
func buildParityMasks(g GeneratorMatrix) parityMasks {
	m := g.BlockSize()
	words := (m + 63) / 64
	rows := make([][]uint64, len(g))
	for i, row := range g {
		mask := make([]uint64, words)
		for j, bit := range row {
			if bit != 0 {
				mask[j>>6] |= 1 << uint(j&63)
			}
		}
		rows[i] = mask
	}
	return parityMasks{words: words, rows: rows}
}

// parityInto packs dataBits into words (zero-padded on both sides for a
// short trailing block, so padding never causes a spurious mismatch)
func (pm *parityMasks) parityInto(dataBits []byte, words []uint64, parity []byte) []byte {
	ws := words[:pm.words]
	for i := range ws {
		ws[i] = 0
	}
	for i, b := range dataBits {
		ws[i>>6] |= uint64(b&1) << uint(i&63)
	}
	pr := parity[:len(pm.rows)]
	for i, mask := range pm.rows {
		var pc int
		for k, w := range ws {
			pc += bits.OnesCount64(w & mask[k])
		}
		pr[i] = byte(pc & 1)
	}
	return pr
}

func parityEqual(a, b []byte) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// maxTableBits bounds the table to 2^16 entries (<=64KB at 1 byte each)
// small enough to stay resident in L1/L2 cache
const maxTableBits = 16

// parityTable precomputes P=M*G^T for every possible m-bit block value
// turns per-block parity into one array lookup instead of p AND+POPCNT
type parityTable struct {
	entries []uint8 // one packed byte per possible block value, bit i = parity bit i
	m       int
}

// buildParityTable enumerates every 2^m block value once; ok is false
// when the matrix is too large for a table (m>maxTableBits or p>8)
func buildParityTable(g GeneratorMatrix) (t parityTable, ok bool) {
	m := g.BlockSize()
	p := g.ParityBits()
	if m > maxTableBits || m == 0 || p > 8 {
		return parityTable{}, false
	}
	rowMasks := make([]uint64, p)
	for i, row := range g {
		var mask uint64
		for j, bit := range row {
			if bit != 0 {
				mask |= 1 << uint(j)
			}
		}
		rowMasks[i] = mask
	}
	size := 1 << uint(m)
	entries := make([]uint8, size)
	for idx := range size {
		var pr uint8
		for i, mask := range rowMasks {
			pr |= byte(bits.OnesCount64(uint64(idx)&mask)&1) << uint(i)
		}
		entries[idx] = pr
	}
	return parityTable{entries: entries, m: m}, true
}

// minParallelBlockWork is the block-count*blockSize below which
// goroutine dispatch costs more than the (very cheap) table lookups save
const minParallelBlockWork = 262144

// ComputeBlockParity computes the parity vector for every m-bit block,
// using only this side's own bits --> a peer only ever needs the result
func ComputeBlockParity(bits []byte, g GeneratorMatrix) [][]byte {
	blockSize := g.BlockSize()
	if blockSize <= 0 {
		panic("generator matrix must have at least one column")
	}
	n := len(bits)
	numBlocks := (n + blockSize - 1) / blockSize
	p := g.ParityBits()

	// one flat backing array for every block's parity bits, instead of
	// numBlocks separate small heap allocations
	flat := make([]byte, numBlocks*p)
	parities := make([][]byte, numBlocks)
	for bi := range numBlocks {
		parities[bi] = flat[bi*p : bi*p+p]
	}

	workers := runtime.GOMAXPROCS(0)
	if n < minParallelBlockWork {
		workers = 1
	}
	workers = max(min(workers, numBlocks), 1)

	table, useTable := buildParityTable(g)

	var wg sync.WaitGroup
	chunk := (numBlocks + workers - 1) / workers
	if useTable {
		entries := table.entries
		for w := range workers {
			start := w * chunk
			end := min(start+chunk, numBlocks)
			if start >= end {
				continue
			}
			wg.Add(1)
			// index built straight from the byte slice, same cost as the
			// POPCNT path's packing loop --> only the lookup itself is new
			go func(startBlk, endBlk int) {
				defer wg.Done()
				for bi := startBlk; bi < endBlk; bi++ {
					bs := bi * blockSize
					be := min(bs+blockSize, n)
					var idx uint64
					for j, b := range bits[bs:be] {
						idx |= uint64(b&1) << uint(j)
					}
					pr := entries[idx]
					dst := parities[bi]
					for i := range p {
						dst[i] = byte(pr>>uint(i)) & 1
					}
				}
			}(start, end)
		}
		wg.Wait()
		return parities
	}

	masks := buildParityMasks(g)
	for w := range workers {
		start := w * chunk
		end := min(start+chunk, numBlocks)
		if start >= end {
			continue
		}
		wg.Add(1)
		go func(startBlk, endBlk int) {
			defer wg.Done()
			words := make([]uint64, masks.words)
			for bi := startBlk; bi < endBlk; bi++ {
				bs := bi * blockSize
				be := min(bs+blockSize, n)
				masks.parityInto(bits[bs:be], words, parities[bi])
			}
		}(start, end)
	}
	wg.Wait()
	return parities
}

// BlockReconcileResult summarizes one side's outcome of comparing its own
// parity against a peer's parity, without ever needing the peer's bits
type BlockReconcileResult struct {
	Surviving     []byte
	Discarded     []byte
	BlocksTotal   int
	BlocksKept    int
	BlocksDropped int
	LeakedBits    int // classical-channel bits spent announcing parity of surviving blocks
}

// ReconcileBlocks drops (never flips) any block whose parity disagrees
// a prefix sum gives each block its own write offset, no peer bits needed
func ReconcileBlocks(ownBits []byte, ownParity, peerParity [][]byte, blockSize int) BlockReconcileResult {
	if len(ownParity) != len(peerParity) {
		panic("ownParity and peerParity must cover the same number of blocks")
	}
	numBlocks := len(ownParity)
	n := len(ownBits)

	var res BlockReconcileResult
	res.BlocksTotal = numBlocks
	if numBlocks == 0 {
		return res
	}

	kept := make([]bool, numBlocks)
	workers := runtime.GOMAXPROCS(0)
	if n < minParallelBlockWork {
		workers = 1
	}
	workers = max(min(workers, numBlocks), 1)

	var wg sync.WaitGroup
	chunk := (numBlocks + workers - 1) / workers
	for w := range workers {
		start := w * chunk
		end := min(start+chunk, numBlocks)
		if start >= end {
			continue
		}
		wg.Add(1)
		go func(startBlk, endBlk int) {
			defer wg.Done()
			for bi := startBlk; bi < endBlk; bi++ {
				kept[bi] = parityEqual(ownParity[bi], peerParity[bi])
			}
		}(start, end)
	}
	wg.Wait()

	// cheap serial pass: pure arithmetic, no byte copying, turns kept[]
	// into a destination offset per block for the parallel copy below
	offset := make([]int, numBlocks)
	var survivingLen, discardedLen int
	for bi := range numBlocks {
		blen := min(blockSize, n-bi*blockSize)
		if kept[bi] {
			offset[bi] = survivingLen
			survivingLen += blen
			res.BlocksKept++
			res.LeakedBits += len(ownParity[bi])
		} else {
			offset[bi] = discardedLen
			discardedLen += blen
			res.BlocksDropped++
		}
	}
	res.Surviving = make([]byte, survivingLen)
	res.Discarded = make([]byte, discardedLen)

	surviving, discarded := res.Surviving, res.Discarded
	for w := range workers {
		start := w * chunk
		end := min(start+chunk, numBlocks)
		if start >= end {
			continue
		}
		wg.Add(1)
		go func(startBlk, endBlk int) {
			defer wg.Done()
			for bi := startBlk; bi < endBlk; bi++ {
				bs := bi * blockSize
				be := min(bs+blockSize, n)
				if kept[bi] {
					copy(surviving[offset[bi]:], ownBits[bs:be])
				} else {
					copy(discarded[offset[bi]:], ownBits[bs:be])
				}
			}
		}(start, end)
	}
	wg.Wait()
	return res
}

// BlockDetectResult summarizes the outcome of running step 2 over the
// full sifted key
type BlockDetectResult struct {
	SurvivingAlice []byte // concatenated bits from blocks with matching parity
	SurvivingBob   []byte
	DiscardedAlice []byte // concatenated bits from blocks flagged as errored (for QBER use)
	DiscardedBob   []byte
	BlocksTotal    int
	BlocksKept     int
	BlocksDropped  int
	LeakedBits     int // classical-channel bits spent announcing parity of surviving blocks
}

// BlockErrorDetect is a convenience wrapper for simulation and testing
// use ComputeBlockParity + ReconcileBlocks directly to run each side apart
func BlockErrorDetect(alice, bob []byte, g GeneratorMatrix) BlockDetectResult {
	blockSize := g.BlockSize()
	aliceParity := ComputeBlockParity(alice, g)
	bobParity := ComputeBlockParity(bob, g)
	aliceRes := ReconcileBlocks(alice, aliceParity, bobParity, blockSize)
	bobRes := ReconcileBlocks(bob, bobParity, aliceParity, blockSize)
	return BlockDetectResult{
		SurvivingAlice: aliceRes.Surviving,
		SurvivingBob:   bobRes.Surviving,
		DiscardedAlice: aliceRes.Discarded,
		DiscardedBob:   bobRes.Discarded,
		BlocksTotal:    aliceRes.BlocksTotal,
		BlocksKept:     aliceRes.BlocksKept,
		BlocksDropped:  aliceRes.BlocksDropped,
		LeakedBits:     aliceRes.LeakedBits, // same p-bit-per-kept-block count on both sides
	}
}
