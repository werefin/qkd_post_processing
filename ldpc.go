package qoj_post_processing

import (
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"runtime"
	"sync"
)

// step 2/3 alternative: one LDPC syndrome reveal per block, decoded by
// min-sum belief propagation, instead of Winnow's multi-pass re-reveal
// or block discard's throw-away-on-mismatch

// LDPCMatrix is a sparse regular-column-weight parity-check matrix H,
// stored as adjacency lists so encode/decode cost is O(edges), not O(n*m)
type LDPCMatrix struct {
	N, M       int       // N variable nodes (bits), M check nodes (parity constraints)
	Wc, Wr     int       // nominal column/row weight; the last row per submatrix may be shorter
	CheckVars  [][]int32 // check c, its connected variable indices
	edgeOffset []int     // len M+1, check c's edges span edgeOffset[c]:edgeOffset[c+1]
	varEdges   [][]int   // variable v, its edge indices into the flat message arrays
}

const minParallelLDPCWork = 2048

// parallelRange splits [0,total) across GOMAXPROCS workers, or runs fn
// inline when total is too small for goroutine dispatch to pay off
func parallelRange(total int, fn func(start, end int)) {
	if total == 0 {
		return
	}
	workers := runtime.GOMAXPROCS(0)
	if total < minParallelLDPCWork {
		workers = 1
	}
	workers = max(min(workers, total), 1)
	if workers == 1 {
		fn(0, total)
		return
	}
	var wg sync.WaitGroup
	chunk := (total + workers - 1) / workers
	for w := range workers {
		start := w * chunk
		end := min(start+chunk, total)
		if start >= end {
			continue
		}
		wg.Add(1)
		go func(s, e int) {
			defer wg.Done()
			fn(s, e)
		}(start, end)
	}
	wg.Wait()
}

// pairKey packs two variable indices into one map key, order-independent
func pairKey(a, b int32) uint64 {
	if a > b {
		a, b = b, a
	}
	return uint64(uint32(a))<<32 | uint64(uint32(b))
}

// markPairs records every pair of variables in row as now sharing a check
func markPairs(used map[uint64]struct{}, row []int32) {
	for i := range row {
		for j := i + 1; j < len(row); j++ {
			used[pairKey(row[i], row[j])] = struct{}{}
		}
	}
}

// countConflicts counts pairs in perm's row grouping that already share a
// check, the 4-cycle signal a candidate permutation should minimize
func countConflicts(used map[uint64]struct{}, perm []int, rowsPerBlock, wr int) int {
	n := len(perm)
	conflicts := 0
	for r := range rowsPerBlock {
		lo, hi := r*wr, min(r*wr+wr, n)
		for i := lo; i < hi; i++ {
			for j := i + 1; j < hi; j++ {
				if _, ok := used[pairKey(int32(perm[i]), int32(perm[j]))]; ok {
					conflicts++
				}
			}
		}
	}
	return conflicts
}

const ldpcConstructionTries = 8

// ldpcCycleAvoidanceMaxPairs bounds the map-tracked pair count 4-cycle
// avoidance is allowed to cost during construction, see BuildRegularLDPC
const ldpcCycleAvoidanceMaxPairs = 4_000_000

// BuildRegularLDPC constructs a Gallager-style regular LDPC code over n
// bits, column weight wc, nominal row weight wr, deterministic given seed
func BuildRegularLDPC(n, wc, wr int, seed int64) (*LDPCMatrix, error) {
	if n <= 0 || wc <= 0 || wr <= 0 {
		return nil, fmt.Errorf("ldpc: n, wc, wr must be positive")
	}
	if wc >= wr {
		return nil, fmt.Errorf("ldpc: wc must be less than wr for a positive code rate")
	}

	rowsPerBlock := (n + wr - 1) / wr
	m := wc * rowsPerBlock
	checkVars := make([][]int32, m)

	// 4-cycle avoidance costs O(m*wr^2) map traffic; past
	// ldpcCycleAvoidanceMaxPairs that swamps construction time for no real
	// gain, since a large random bipartite graph is locally tree-like with
	// high probability anyway -- skip straight to a single random draw
	estimatedPairs := m * wr * (wr - 1) / 2
	tries := ldpcConstructionTries
	avoidCycles := estimatedPairs <= ldpcCycleAvoidanceMaxPairs
	if !avoidCycles {
		tries = 1
	}
	var used map[uint64]struct{}
	if avoidCycles {
		used = make(map[uint64]struct{}, estimatedPairs)
	}

	buildRow := func(cols []int) []int32 {
		row := make([]int32, len(cols))
		for i, c := range cols {
			row[i] = int32(c)
		}
		return row
	}

	// submatrix 0: row r covers columns [r*wr, min(r*wr+wr, n))
	for r := range rowsPerBlock {
		lo, hi := r*wr, min(r*wr+wr, n)
		cols := make([]int, hi-lo)
		for i := range cols {
			cols[i] = lo + i
		}
		checkVars[r] = buildRow(cols)
		if avoidCycles {
			markPairs(used, checkVars[r])
		}
	}

	rng := rand.New(rand.NewSource(seed))
	perm := make([]int, n)
	for s := 1; s < wc; s++ {
		base := s * rowsPerBlock
		var bestPerm []int
		bestConflicts := -1
		for try := 0; try < tries; try++ {
			for i := range perm {
				perm[i] = i
			}
			rng.Shuffle(n, func(i, j int) { perm[i], perm[j] = perm[j], perm[i] })
			conflicts := 0
			if avoidCycles {
				conflicts = countConflicts(used, perm, rowsPerBlock, wr)
			}
			if bestConflicts == -1 || conflicts < bestConflicts {
				bestConflicts = conflicts
				bestPerm = append(bestPerm[:0:0], perm...)
				if conflicts == 0 {
					break
				}
			}
		}
		for r := range rowsPerBlock {
			lo, hi := r*wr, min(r*wr+wr, n)
			checkVars[base+r] = buildRow(bestPerm[lo:hi])
			if avoidCycles {
				markPairs(used, checkVars[base+r])
			}
		}
	}

	return finalizeLDPCMatrix(n, m, wc, wr, checkVars), nil
}

// pegDegreeBuckets tracks each check's current degree in O(1)-amortized
// buckets, so PEG's min-degree tie-break never needs to rescan every check
type pegDegreeBuckets struct {
	buckets [][]int32
	pos     []int32
	deg     []int32
	minDeg  int
}

func newPEGDegreeBuckets(m int) *pegDegreeBuckets {
	b := &pegDegreeBuckets{buckets: [][]int32{make([]int32, m)}, pos: make([]int32, m), deg: make([]int32, m)}
	for c := range m {
		b.buckets[0][c] = int32(c)
		b.pos[c] = int32(c)
	}
	return b
}

// increment moves check c up one degree bucket and advances minDeg past
// any bucket that just emptied, amortized O(1) over the whole construction
func (b *pegDegreeBuckets) increment(c int32) {
	d := int(b.deg[c])
	bucket := b.buckets[d]
	p := b.pos[c]
	last := int32(len(bucket) - 1)
	bucket[p] = bucket[last]
	b.pos[bucket[p]] = p
	b.buckets[d] = bucket[:last]

	b.deg[c] = int32(d + 1)
	if d+1 == len(b.buckets) {
		b.buckets = append(b.buckets, nil)
	}
	b.buckets[d+1] = append(b.buckets[d+1], c)
	b.pos[c] = int32(len(b.buckets[d+1]) - 1)

	if d == b.minDeg && len(b.buckets[d]) == 0 {
		for b.minDeg < len(b.buckets)-1 && len(b.buckets[b.minDeg]) == 0 {
			b.minDeg++
		}
	}
}

func pegPickFromBucket(bucket []int32, rng *rand.Rand) int32 {
	return bucket[rng.Intn(len(bucket))]
}

// pegPickUnvisitedMinDegree scans degree buckets upward for the lowest
// degree check this BFS never reached, PEG's rule when the search ran out
// of new checks before any depth or touched-node cap was hit
func pegPickUnvisitedMinDegree(b *pegDegreeBuckets, checkGen []int32, gen int32, rng *rand.Rand) int32 {
	for d := b.minDeg; d < len(b.buckets); d++ {
		var candidates []int32
		for _, c := range b.buckets[d] {
			if checkGen[c] != gen {
				candidates = append(candidates, c)
			}
		}
		if len(candidates) > 0 {
			return candidates[rng.Intn(len(candidates))]
		}
	}
	// every check was reached (a small, already-dense graph): fall back to
	// the plain global minimum, same as the k==0 rule
	return pegPickFromBucket(b.buckets[b.minDeg], rng)
}

func pegPickMinDegreeAmong(candidates []int32, b *pegDegreeBuckets, rng *rand.Rand) int32 {
	best := candidates[0]
	bestDeg := b.deg[best]
	ties := []int32{best}
	for _, c := range candidates[1:] {
		d := b.deg[c]
		if d < bestDeg {
			bestDeg = d
			ties = ties[:0]
			ties = append(ties, c)
		} else if d == bestDeg {
			ties = append(ties, c)
		}
	}
	return ties[rng.Intn(len(ties))]
}

// pegMaxBFSDepth/pegMaxBFSTouched bound each edge's BFS cost so
// construction stays tractable; PEG's classic operating range is small to
// moderate block lengths, BuildRegularLDPC is the fast option past that
const pegMaxBFSDepth = 20
const pegMaxBFSTouched = 4000

// BuildPEGLDPC constructs a column-regular LDPC parity-check matrix via
// Progressive Edge Growth (Hu, Eleftheriou, Arnold, 2001): each edge picks
// whichever check is currently farthest from its variable in the Tanner
// graph, directly maximizing local girth instead of leaving it to random
// chance. Same (n, m, wc) as BuildRegularLDPC for the same wr, so any
// difference in decoding is purely from better edge placement, not rate
func BuildPEGLDPC(n, wc, wr int, seed int64) (*LDPCMatrix, error) {
	if n <= 0 || wc <= 0 || wr <= 0 {
		return nil, fmt.Errorf("ldpc: n, wc, wr must be positive")
	}
	if wc >= wr {
		return nil, fmt.Errorf("ldpc: wc must be less than wr for a positive code rate")
	}

	rowsPerBlock := (n + wr - 1) / wr
	m := wc * rowsPerBlock

	checkVars := make([][]int32, m)
	varChecks := make([][]int32, n)
	buckets := newPEGDegreeBuckets(m)

	rng := rand.New(rand.NewSource(seed))
	order := rng.Perm(n)

	checkGen := make([]int32, m)
	varGen := make([]int32, n)
	var gen int32
	var frontierVar, frontierVarNext, frontierCheck, lastLevel []int32

	for _, v := range order {
		for k := range wc {
			var chosen int32
			if k == 0 {
				chosen = pegPickFromBucket(buckets.buckets[buckets.minDeg], rng)
			} else {
				gen++
				varGen[v] = gen
				frontierVar = append(frontierVar[:0], int32(v))
				touched := 1
				// cappedByBudget is true only when depth/touched forced the
				// loop to stop while it was still actively discovering new
				// checks -- any other exit (no new checks, or no further
				// variables to expand through) means unreached checks exist
				// elsewhere, cheaper and better found via the degree buckets
				cappedByBudget := false
				depth := 0
				for ; depth < pegMaxBFSDepth && touched < pegMaxBFSTouched; depth++ {
					frontierCheck = frontierCheck[:0]
					for _, fv := range frontierVar {
						for _, c := range varChecks[fv] {
							if checkGen[c] != gen {
								checkGen[c] = gen
								frontierCheck = append(frontierCheck, c)
								touched++
							}
						}
					}
					if len(frontierCheck) == 0 {
						break
					}
					lastLevel = append(lastLevel[:0:0], frontierCheck...)

					frontierVarNext = frontierVarNext[:0]
					for _, fc := range frontierCheck {
						for _, fv2 := range checkVars[fc] {
							if varGen[fv2] != gen {
								varGen[fv2] = gen
								frontierVarNext = append(frontierVarNext, fv2)
								touched++
							}
						}
					}
					frontierVar, frontierVarNext = frontierVarNext, frontierVar
					if len(frontierVar) == 0 {
						break
					}
					if depth+1 >= pegMaxBFSDepth || touched >= pegMaxBFSTouched {
						cappedByBudget = true
					}
				}
				if cappedByBudget {
					chosen = pegPickMinDegreeAmong(lastLevel, buckets, rng)
				} else {
					chosen = pegPickUnvisitedMinDegree(buckets, checkGen, gen, rng)
				}
			}

			checkVars[chosen] = append(checkVars[chosen], int32(v))
			varChecks[v] = append(varChecks[v], chosen)
			buckets.increment(chosen)
		}
	}

	return finalizeLDPCMatrix(n, m, wc, wr, checkVars), nil
}

// finalizeLDPCMatrix derives the flat edge indexing every decode iteration
// walks, computed once so it never touches the construction logic again
func finalizeLDPCMatrix(n, m, wc, wr int, checkVars [][]int32) *LDPCMatrix {
	edgeOffset := make([]int, m+1)
	for c := range m {
		edgeOffset[c+1] = edgeOffset[c] + len(checkVars[c])
	}
	varDeg := make([]int, n)
	for c := range m {
		for _, v := range checkVars[c] {
			varDeg[v]++
		}
	}
	varEdgesFlat := make([]int, edgeOffset[m])
	varEdges := make([][]int, n)
	pos := 0
	for v := range n {
		varEdges[v] = varEdgesFlat[pos : pos : pos+varDeg[v]]
		pos += varDeg[v]
	}
	for c := range m {
		for k, v := range checkVars[c] {
			edge := edgeOffset[c] + k
			varEdges[v] = append(varEdges[v], edge)
		}
	}
	return &LDPCMatrix{N: n, M: m, Wc: wc, Wr: wr, CheckVars: checkVars, edgeOffset: edgeOffset, varEdges: varEdges}
}

// Syndrome computes H*bits mod 2, using only this side's own bits
func (h *LDPCMatrix) Syndrome(bits []byte) []byte {
	out := make([]byte, h.M)
	h.syndromeInto(bits, out)
	return out
}

// syndromeInto reuses dst instead of allocating, for the decode loop's
// per-iteration convergence check
func (h *LDPCMatrix) syndromeInto(bits []byte, dst []byte) {
	parallelRange(h.M, func(cs, ce int) {
		for c := cs; c < ce; c++ {
			var parity byte
			for _, v := range h.CheckVars[c] {
				parity ^= bits[v]
			}
			dst[c] = parity & 1
		}
	})
}

// LDPCDecodeConfig tunes the min-sum belief-propagation decoder
type LDPCDecodeConfig struct {
	MaxIterations int // 0 defaults to 50
}

const defaultLDPCMaxIterations = 50
const infMag float32 = 1e30

// ldpcNormFactor scales check-to-variable magnitudes, plain min-sum is
// known to overstate confidence and diverge, normalized min-sum near this
// value tracks true sum-product decoding closely at a fraction of the cost
const ldpcNormFactor float32 = 0.75

// clampProb keeps a QBER estimate away from 0 or 0.5, where the LLR blows
// up or the channel carries no information at all
func clampProb(p float64) float64 {
	const eps = 1e-6
	if p < eps {
		return eps
	}
	if p > 0.5-eps {
		return 0.5 - eps
	}
	return p
}

// LDPCDecode runs min-sum belief propagation, estimating the bits that
// satisfy syndrome under h and are closest to receivedBits over a BSC(qber)
func LDPCDecode(h *LDPCMatrix, receivedBits, syndrome []byte, qber float64, cfg LDPCDecodeConfig) (corrected []byte, converged bool, iterations int) {
	n, m := h.N, h.M
	maxIter := cfg.MaxIterations
	if maxIter <= 0 {
		maxIter = defaultLDPCMaxIterations
	}

	p := clampProb(qber)
	llr0 := float32(math.Log((1 - p) / p))
	intrinsic := make([]float32, n)
	for v := range intrinsic {
		if receivedBits[v] == 0 {
			intrinsic[v] = llr0
		} else {
			intrinsic[v] = -llr0
		}
	}

	numEdges := h.edgeOffset[m]
	msgC2V := make([]float32, numEdges)
	msgV2C := make([]float32, numEdges)
	for v := range n {
		for _, e := range h.varEdges[v] {
			msgV2C[e] = intrinsic[v]
		}
	}

	checkSign := make([]float32, m)
	for c := range checkSign {
		if syndrome[c]&1 == 1 {
			checkSign[c] = -1
		} else {
			checkSign[c] = 1
		}
	}

	corrected = make([]byte, n)
	scratch := make([]byte, m)

	for iter := range maxIter {
		// check-to-variable pass (min-sum): each check's outgoing message
		// is the product of the other edges' signs times the smaller of
		// the min/second-min magnitude, excluding the edge it targets
		parallelRange(m, func(cs, ce int) {
			for c := cs; c < ce; c++ {
				start, end := h.edgeOffset[c], h.edgeOffset[c+1]
				min1, min2 := infMag, infMag
				min1Edge := -1
				signProd := float32(1)
				for e := start; e < end; e++ {
					msg := msgV2C[e]
					mag := msg
					sign := float32(1)
					if mag < 0 {
						sign = -1
						mag = -mag
					}
					signProd *= sign
					if mag < min1 {
						min2 = min1
						min1 = mag
						min1Edge = e
					} else if mag < min2 {
						min2 = mag
					}
				}
				for e := start; e < end; e++ {
					msg := msgV2C[e]
					sign := float32(1)
					if msg < 0 {
						sign = -1
					}
					mag := min1
					if e == min1Edge {
						mag = min2
					}
					msgC2V[e] = ldpcNormFactor * checkSign[c] * signProd * sign * mag
				}
			}
		})

		// variable-to-check pass: total belief is the intrinsic LLR plus
		// every incoming check message, extrinsic message excludes its own
		parallelRange(n, func(vs, ve int) {
			for v := vs; v < ve; v++ {
				sum := intrinsic[v]
				for _, e := range h.varEdges[v] {
					sum += msgC2V[e]
				}
				for _, e := range h.varEdges[v] {
					msgV2C[e] = sum - msgC2V[e]
				}
				if sum < 0 {
					corrected[v] = 1
				} else {
					corrected[v] = 0
				}
			}
		})

		h.syndromeInto(corrected, scratch)
		if bytes.Equal(scratch, syndrome) {
			return corrected, true, iter + 1
		}
	}
	return corrected, false, maxIter
}

// LDPCReconcileResult summarizes one block's correction attempt
type LDPCReconcileResult struct {
	CorrectedBob []byte
	Converged    bool
	Iterations   int
	LeakedBits   int // classical-channel bits spent announcing Alice's syndrome
}

// LDPCPass reconciles one h.N-bit block: Alice's syndrome is the only
// thing that crosses the channel, Bob decodes toward it locally
func LDPCPass(h *LDPCMatrix, alice, bob []byte, qber float64, cfg LDPCDecodeConfig) LDPCReconcileResult {
	syndrome := h.Syndrome(alice)
	corrected, converged, iterations := LDPCDecode(h, bob, syndrome, qber, cfg)
	return LDPCReconcileResult{
		CorrectedBob: corrected,
		Converged:    converged,
		Iterations:   iterations,
		LeakedBits:   h.M,
	}
}

// LDPCReconcile runs LDPCPass over every h.N-bit block of alice/bob,
// zero-padding a short final block identically on both sides
func LDPCReconcile(h *LDPCMatrix, alice, bob []byte, qber float64, cfg LDPCDecodeConfig) (correctedBob []byte, convergedBlocks, totalBlocks, leakedBits int) {
	n := h.N
	total := len(alice)
	if total == 0 {
		return nil, 0, 0, 0
	}
	numBlocks := (total + n - 1) / n
	correctedBob = make([]byte, total)

	aliceBlock := make([]byte, n)
	bobBlock := make([]byte, n)
	for bi := range numBlocks {
		bs := bi * n
		be := min(bs+n, total)
		blen := be - bs
		copy(aliceBlock, alice[bs:be])
		copy(bobBlock, bob[bs:be])
		for k := blen; k < n; k++ {
			aliceBlock[k] = 0
			bobBlock[k] = 0
		}
		res := LDPCPass(h, aliceBlock, bobBlock, qber, cfg)
		copy(correctedBob[bs:be], res.CorrectedBob[:blen])
		leakedBits += res.LeakedBits
		if res.Converged {
			convergedBlocks++
		}
	}
	return correctedBob, convergedBlocks, numBlocks, leakedBits
}
