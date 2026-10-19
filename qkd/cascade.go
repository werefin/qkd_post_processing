package qkd

import (
	"container/heap"
	"math"
)

// Cascade as optimized by Martinez-Mateo et al., QIC 15 (2015): blocks of 2^ceil(log2(1/QBER)) growing x4,
// no parity disclosed twice, the other half of a split inferred, corrections reopening earlier blocks

// CascadeConfig is one run; zero values take the defaults below
type CascadeConfig struct {
	Passes     int   // passes, default 3
	FirstBlock int   // first-pass block size, default from the QBER (CascadeFirstBlock)
	BlockSizes []int // every pass's block size, overriding Passes and FirstBlock when given
	Seed       int64 // derives the shuffle of every pass after the first; must be agreed publicly
}

// CascadeStats summarizes a run
type CascadeStats struct {
	Passes      int
	BlockSizes  []int
	Parities    int // parities Alice disclosed: the leak
	Inferred    int // parities derived from known ones instead of disclosed
	Corrections int
	Searches    int // binary searches run, the cascade's own included
}

// cascadeTargetErrors is the expected errors in a first-pass block, cascadeGrowth the block growth per pass;
// with 3 passes the best measured, f 1.03 at 1% QBER to 1.07 at 11%, no errors left (cascade_test.go)
const (
	cascadeTargetErrors = 0.75
	cascadeGrowth       = 4
	cascadePasses       = 3
	cascadeMaxPasses    = 24
	cascadeCleanPasses  = 8 // half-frame passes in a row that must find nothing: each halves an error pair's odds
)

// CascadeFirstBlock is the first-pass block size for a public QBER estimate
func CascadeFirstBlock(qber float64, n int) int {
	q := math.Max(qber, 1e-4)
	k := 1 << max(1, int(math.Ceil(math.Log2(cascadeTargetErrors/q))))
	return max(2, min(k, max(2, n/2)))
}

// fenwick keeps Bob's bits in one pass's order, giving the parity of any prefix in log time as bits flip
type fenwick []byte

func newFenwick(bits []byte) fenwick {
	f := make(fenwick, len(bits)+1)
	for i, b := range bits {
		f[i+1] ^= b & 1
		if j := (i + 1) + ((i + 1) & -(i + 1)); j <= len(bits) {
			f[j] ^= f[i+1]
		}
	}
	return f
}

func (f fenwick) flip(i int) {
	for i++; i < len(f); i += i & -i {
		f[i] ^= 1
	}
}

func (f fenwick) prefix(i int) byte { // parity of the first i bits
	var p byte
	for ; i > 0; i -= i & -i {
		p ^= f[i]
	}
	return p
}

type cascadePass struct {
	perm    []int32 // pass position -> bit
	pos     []int32 // bit -> pass position
	block   int
	alice   []byte // prefix parities of Alice's bits in pass order: Alice's side of the protocol
	bob     fenwick
	known   map[int64]byte // Alice parity of [start, end), disclosed or inferred
	checked int            // blocks of this pass already opened
}

func (p *cascadePass) key(start, end int) int64 { return int64(start)<<32 | int64(end) }

type cascadeRun struct {
	alice, bob      []byte
	passes          []*cascadePass
	stats           CascadeStats
	passCorrections int // corrections made in the last pass, its own and those it cascaded
}

// aliceParity is the protocol's one question to Alice: from the cache when known, else disclosed and counted
func (r *cascadeRun) aliceParity(p *cascadePass, start, end int) byte {
	k := p.key(start, end)
	if v, ok := p.known[k]; ok {
		return v
	}
	v := p.alice[end] ^ p.alice[start]
	p.known[k] = v
	r.stats.Parities++
	return v
}

func (r *cascadeRun) bobParity(p *cascadePass, start, end int) byte {
	return p.bob.prefix(end) ^ p.bob.prefix(start)
}

// search halves [start, end) of pass p, whose parities differ, down to the erroneous bit, and flips it
func (r *cascadeRun) search(p *cascadePass, start, end int) int {
	r.stats.Searches++
	whole := r.aliceParity(p, start, end)
	for end-start > 1 {
		mid := start + (end-start)/2
		left := r.aliceParity(p, start, mid)
		// the right half's parity follows from the whole and the left: known, never disclosed
		if k := p.key(mid, end); func() bool { _, ok := p.known[k]; return !ok }() {
			p.known[k] = whole ^ left
			r.stats.Inferred++
		}
		if left != r.bobParity(p, start, mid) {
			end, whole = mid, left
		} else {
			start, whole = mid, whole^left
		}
	}
	bit := int(p.perm[start])
	r.bob[bit] ^= 1
	for _, q := range r.passes {
		q.bob.flip(int(q.pos[bit]))
	}
	r.stats.Corrections++
	return bit
}

// pending is a block awaiting a check, ordered shortest first
type pending struct {
	pass, start, end int
}
type pendingHeap []pending

func (h pendingHeap) Len() int            { return len(h) }
func (h pendingHeap) Less(i, j int) bool  { return h[i].end-h[i].start < h[j].end-h[j].start }
func (h pendingHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *pendingHeap) Push(x interface{}) { *h = append(*h, x.(pending)) }
func (h *pendingHeap) Pop() interface{} {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// cascade reopens, after a correction, the block holding the bit in every pass already opened; blocks whose
// parities now differ are searched, shortest first, and their corrections reopen blocks in turn
func (r *cascadeRun) cascade(bit int, upto int) {
	h := &pendingHeap{}
	push := func(bit int) {
		for qi := 0; qi <= upto; qi++ {
			q := r.passes[qi]
			at := int(q.pos[bit])
			start := (at / q.block) * q.block
			if start/q.block >= q.checked {
				continue // not opened yet: its own turn will check it
			}
			heap.Push(h, pending{qi, start, min(start+q.block, len(r.alice))})
		}
	}
	push(bit)
	for h.Len() > 0 {
		b := heap.Pop(h).(pending)
		q := r.passes[b.pass]
		// only a block whose disclosed parity is known can be compared; every opened block's is
		if r.aliceParity(q, b.start, b.end) != r.bobParity(q, b.start, b.end) {
			push(r.search(q, b.start, b.end))
		}
	}
}

// CascadeReconcile corrects bob toward alice; it returns Bob's corrected bits and the run's statistics, whose
// Parities is everything disclosed about Alice's bits
func CascadeReconcile(alice, bob []byte, qber float64, cfg CascadeConfig) ([]byte, CascadeStats) {
	n := len(alice)
	r := &cascadeRun{alice: alice, bob: append([]byte(nil), bob...)}
	if n == 0 {
		return r.bob, r.stats
	}
	sizes := cfg.BlockSizes
	if len(sizes) == 0 {
		passes := cfg.Passes
		if passes <= 0 {
			passes = cascadePasses
		}
		k := cfg.FirstBlock
		if k <= 0 {
			k = CascadeFirstBlock(qber, n)
		}
		for range passes {
			sizes = append(sizes, min(k, max(2, n/2)))
			k *= cascadeGrowth
		}
	}
	addPass := func(i, k int) {
		var perm []int32
		if i == 0 {
			perm = make([]int32, n)
			for j := range perm {
				perm[j] = int32(j)
			}
		} else {
			full := GeneratePermutation(n, cfg.Seed+int64(i))
			perm = make([]int32, n)
			for j, v := range full {
				perm[j] = int32(v)
			}
		}
		p := &cascadePass{perm: perm, pos: make([]int32, n), block: max(2, min(k, n)), alice: make([]byte, n+1), known: map[int64]byte{}}
		ordered := make([]byte, n)
		for j, b := range perm {
			p.pos[b] = int32(j)
			p.alice[j+1] = p.alice[j] ^ (alice[b] & 1)
			ordered[j] = r.bob[b] & 1
		}
		p.bob = newFenwick(ordered)
		r.passes = append(r.passes, p)
		r.stats.BlockSizes = append(r.stats.BlockSizes, p.block)
	}
	// the planned passes, then half-frame passes until enough in a row find nothing: two parities each, and a
	// pair of errors sharing every block so far is split by a new shuffle with probability one half
	clean := 0
	for pi := 0; pi < cascadeMaxPasses; pi++ {
		if pi >= len(sizes) {
			if r.passCorrections == 0 {
				clean++
			} else {
				clean = 0
			}
			if clean >= cascadeCleanPasses {
				break
			}
			sizes = append(sizes, min(sizes[len(sizes)-1]*cascadeGrowth, max(2, n/2)))
		}
		addPass(pi, sizes[pi])
		before := r.stats.Corrections
		p := r.passes[pi]
		for start := 0; start < n; start += p.block {
			end := min(start+p.block, n)
			p.checked++
			if r.aliceParity(p, start, end) != r.bobParity(p, start, end) {
				bit := r.search(p, start, end)
				if pi > 0 {
					r.cascade(bit, pi)
				}
			}
		}
		r.passCorrections = r.stats.Corrections - before
	}
	passes := len(r.passes)
	r.stats.Passes = passes
	return r.bob, r.stats
}
