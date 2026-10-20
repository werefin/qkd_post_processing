package qkd

import (
	"fmt"
	"math"
	"sort"
)

// Cascade as optimized by Martinez-Mateo et al., QIC 15 (2015): blocks of 2^ceil(log2(1/QBER)) growing x4,
// no parity disclosed twice, the other half of a split inferred, corrections reopening earlier blocks.
// Bob runs it and asks Alice for parities through a CascadeOracle, in batches: one request per pass for its
// blocks, then one per halving step of all the searches under way, so two processes need few round trips

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
	Rounds      int // requests to Alice, each one round trip between two processes
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

// CascadeOracle answers Bob's one question: the parities of Alice's bits over ranges [start, end) of a pass's
// order; pass 0 is the bits' own order and pass i the permutation GeneratePermutation(n, seed+i)
type CascadeOracle interface {
	Parities(pass int, ranges [][2]int32) ([]byte, error)
}

// cascadePerm is pass i's order, the same on both sides
func cascadePerm(n int, seed int64, i int) []int32 {
	perm := make([]int32, n)
	if i == 0 {
		for j := range perm {
			perm[j] = int32(j)
		}
		return perm
	}
	for j, v := range GeneratePermutation(n, seed+int64(i)) {
		perm[j] = int32(v)
	}
	return perm
}

// CascadeAlice is Alice's side: her bits' prefix parities per pass, built when a pass is first asked about,
// and a count of every parity she disclosed, which is her own account of the leak
type CascadeAlice struct {
	bits      []byte
	seed      int64
	prefix    map[int][]byte
	Disclosed int
}

func NewCascadeAlice(bits []byte, seed int64) *CascadeAlice {
	return &CascadeAlice{bits: bits, seed: seed, prefix: map[int][]byte{}}
}

func (a *CascadeAlice) Parities(pass int, ranges [][2]int32) ([]byte, error) {
	if pass < 0 || pass >= cascadeMaxPasses {
		return nil, fmt.Errorf("cascade: pass %d out of range", pass)
	}
	n := len(a.bits)
	pre, ok := a.prefix[pass]
	if !ok {
		pre = make([]byte, n+1)
		for j, b := range cascadePerm(n, a.seed, pass) {
			pre[j+1] = pre[j] ^ (a.bits[b] & 1)
		}
		a.prefix[pass] = pre
	}
	out := make([]byte, len(ranges))
	for i, r := range ranges {
		if r[0] < 0 || r[1] > int32(n) || r[0] >= r[1] {
			return nil, fmt.Errorf("cascade: range [%d, %d) outside %d bits", r[0], r[1], n)
		}
		out[i] = pre[r[1]] ^ pre[r[0]]
	}
	a.Disclosed += len(ranges)
	return out, nil
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
	perm  []int32 // pass position -> bit
	pos   []int32 // bit -> pass position
	block int
	bob   fenwick
	known map[int64]byte // Alice parity of [start, end), disclosed or inferred
}

func (p *cascadePass) key(start, end int) int64 { return int64(start)<<32 | int64(end) }

func (p *cascadePass) bobParity(start, end int) byte { return p.bob.prefix(end) ^ p.bob.prefix(start) }

type cascadeRun struct {
	n      int
	bob    []byte
	passes []*cascadePass
	alice  CascadeOracle
	stats  CascadeStats
}

// ask discloses the parities of ranges of pass pi not yet known, in one request
func (r *cascadeRun) ask(pi int, ranges [][2]int32) error {
	p := r.passes[pi]
	var need [][2]int32
	for _, rg := range ranges {
		if _, ok := p.known[p.key(int(rg[0]), int(rg[1]))]; !ok {
			need = append(need, rg)
		}
	}
	if len(need) == 0 {
		return nil
	}
	par, err := r.alice.Parities(pi, need)
	if err != nil {
		return err
	}
	if len(par) != len(need) {
		return fmt.Errorf("cascade: %d parities for %d ranges", len(par), len(need))
	}
	for i, rg := range need {
		p.known[p.key(int(rg[0]), int(rg[1]))] = par[i] & 1
	}
	r.stats.Parities += len(need)
	r.stats.Rounds++
	return nil
}

func (r *cascadeRun) flip(bit int) {
	r.bob[bit] ^= 1
	for _, q := range r.passes {
		q.bob.flip(int(q.pos[bit]))
	}
	r.stats.Corrections++
}

// searchAll halves blocks of pass pi whose parities differ, all at once, down to their erroneous bits and flips
// them; the blocks do not overlap, so no two searches can meet on one bit
func (r *cascadeRun) searchAll(pi int, blocks [][2]int) ([]int, error) {
	p := r.passes[pi]
	type search struct {
		start, end int
		whole      byte
	}
	active := make([]search, 0, len(blocks))
	for _, b := range blocks {
		active = append(active, search{b[0], b[1], p.known[p.key(b[0], b[1])]})
	}
	r.stats.Searches += len(blocks)
	var fixed []int
	for len(active) > 0 {
		var ranges [][2]int32
		for _, s := range active {
			if s.end-s.start > 1 {
				ranges = append(ranges, [2]int32{int32(s.start), int32(s.start + (s.end-s.start)/2)})
			}
		}
		if err := r.ask(pi, ranges); err != nil {
			return nil, err
		}
		next := active[:0]
		for _, s := range active {
			if s.end-s.start > 1 {
				mid := s.start + (s.end-s.start)/2
				left := p.known[p.key(s.start, mid)]
				// the right half's parity follows from the whole and the left: known, never disclosed
				if k := p.key(mid, s.end); !hasKey(p.known, k) {
					p.known[k] = s.whole ^ left
					r.stats.Inferred++
				}
				if left != p.bobParity(s.start, mid) {
					s.end, s.whole = mid, left
				} else {
					s.start, s.whole = mid, s.whole^left
				}
			}
			if s.end-s.start == 1 {
				bit := int(p.perm[s.start])
				r.flip(bit)
				fixed = append(fixed, bit)
				continue
			}
			next = append(next, s)
		}
		active = next
	}
	return fixed, nil
}

func hasKey(m map[int64]byte, k int64) bool {
	_, ok := m[k]
	return ok
}

// resolve searches the given blocks of pass pi, then the blocks every correction reopens in the passes opened
// so far, in waves of one pass at a time, shortest blocks first, until every opened block's parities agree
func (r *cascadeRun) resolve(pi int, odd [][2]int) error {
	queue := make([]map[int]bool, len(r.passes)) // per pass, block starts to check
	for i := range queue {
		queue[i] = map[int]bool{}
	}
	for _, b := range odd {
		queue[pi][b[0]] = true
	}
	for {
		qi := -1
		for i := range queue {
			if len(queue[i]) > 0 {
				qi = i
				break
			}
		}
		if qi < 0 {
			return nil
		}
		q := r.passes[qi]
		starts := make([]int, 0, len(queue[qi]))
		for s := range queue[qi] {
			starts = append(starts, s)
		}
		queue[qi] = map[int]bool{}
		sort.Ints(starts)
		var blocks [][2]int
		for _, s := range starts {
			e := min(s+q.block, r.n)
			// every opened block's parity is known; only those that now differ are searched
			if q.known[q.key(s, e)] != q.bobParity(s, e) {
				blocks = append(blocks, [2]int{s, e})
			}
		}
		fixed, err := r.searchAll(qi, blocks)
		if err != nil {
			return err
		}
		for _, bit := range fixed {
			for qj := 0; qj <= pi; qj++ {
				q := r.passes[qj]
				queue[qj][(int(q.pos[bit])/q.block)*q.block] = true
			}
		}
	}
}

// CascadeReconcile corrects bob toward alice in one process; it returns Bob's corrected bits and the run's
// statistics, whose Parities is everything disclosed about Alice's bits
func CascadeReconcile(alice, bob []byte, qber float64, cfg CascadeConfig) ([]byte, CascadeStats) {
	out, stats, err := CascadeBob(bob, qber, cfg, NewCascadeAlice(alice, cfg.Seed))
	if err != nil {
		panic(err) // the local oracle fails only on a malformed request, which would be a bug here
	}
	return out, stats
}

// CascadeBob runs Bob's side against Alice's oracle, local or across the network
func CascadeBob(bob []byte, qber float64, cfg CascadeConfig, alice CascadeOracle) ([]byte, CascadeStats, error) {
	n := len(bob)
	r := &cascadeRun{n: n, bob: append([]byte(nil), bob...), alice: alice}
	if n == 0 {
		return r.bob, r.stats, nil
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
	// the planned passes, then half-frame passes until enough in a row find nothing: two parities each, and a
	// pair of errors sharing every block so far is split by a new shuffle with probability one half
	clean, passCorrections := 0, 0
	for pi := 0; pi < cascadeMaxPasses; pi++ {
		if pi >= len(sizes) {
			if passCorrections == 0 {
				clean++
			} else {
				clean = 0
			}
			if clean >= cascadeCleanPasses {
				break
			}
			sizes = append(sizes, min(sizes[len(sizes)-1]*cascadeGrowth, max(2, n/2)))
		}
		perm := cascadePerm(n, cfg.Seed, pi)
		p := &cascadePass{perm: perm, pos: make([]int32, n), block: max(2, min(sizes[pi], n)), known: map[int64]byte{}}
		ordered := make([]byte, n)
		for j, b := range perm {
			p.pos[b] = int32(j)
			ordered[j] = r.bob[b] & 1
		}
		p.bob = newFenwick(ordered)
		r.passes = append(r.passes, p)
		r.stats.BlockSizes = append(r.stats.BlockSizes, p.block)

		before := r.stats.Corrections
		var ranges [][2]int32
		for start := 0; start < n; start += p.block {
			ranges = append(ranges, [2]int32{int32(start), int32(min(start+p.block, n))})
		}
		if err := r.ask(pi, ranges); err != nil {
			return nil, r.stats, err
		}
		var odd [][2]int
		for _, rg := range ranges {
			s, e := int(rg[0]), int(rg[1])
			if p.known[p.key(s, e)] != p.bobParity(s, e) {
				odd = append(odd, [2]int{s, e})
			}
		}
		if err := r.resolve(pi, odd); err != nil {
			return nil, r.stats, err
		}
		passCorrections = r.stats.Corrections - before
	}
	r.stats.Passes = len(r.passes)
	return r.bob, r.stats, nil
}
