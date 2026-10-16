// 5G NR LDPC reconciliation: the protocol and its accounting live here, the matrix work (3GPP TS
// 38.212 base graph 1 by py3gpp, belief propagation by the ldpc library) behind LDPC5GBackend
package qkd

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	mrand "math/rand"
	"runtime"
	"slices"
	"sync"
)

// LDPC5GBackend does the matrix work for the first m rows of base graph 1 lifted by z; one is
// shared by concurrent frames, so implementations must be safe for concurrent use
type LDPC5GBackend interface {
	Syndrome(m, z int, x []byte) ([]byte, error)
	Rank(m, z int, cols []int) (int, error)
	// Decode returns the string nearest y, given each position's error probability, whose syndrome
	// is s; converged reports whether it satisfies s exactly
	Decode(m, z int, y []byte, probs []float64, s []byte, maxIter int) (x []byte, converged bool, err error)
}

const (
	ldpc5gMaxZ     = 384
	ldpc5gKB       = 22
	ldpc5gTagBits  = 32 // per-frame hash: a satisfied syndrome is not proof while positions stay punctured
	ldpc5gSteps    = 40 // reveals per frame before it is given up
	ldpc5gStepIter = 25 // iterations of an attempt that more checks may still follow
	ldpc5gMaxIter  = 100
	ldpc5gFLow     = 1.0 // efficiency of the first attempt, every modulated position punctured
	ldpc5gFHigh    = 2.0 // efficiency once every modulated position is revealed
	ldpc5gKnown    = 1e-12
)

// LDPC5GPlan is the public frame layout for one QBER: base rows, punctured positions in reveal
// order, key positions, whether the punctured columns earn syndrome credit, and the reveal step
type LDPC5GPlan struct {
	M, Z, N, Checks int
	Punctured       []int
	Key             []int
	Credit          bool
	Step            int
}

var ldpc5gPlans sync.Map

// ldpc5gLiftings are the 51 lifting sizes of TS 38.212 Table 5.3.2-1, ascending
var ldpc5gLiftings = func() []int {
	var zs []int
	for _, a := range []int{2, 3, 5, 7, 9, 11, 13, 15} {
		for z := a; z <= ldpc5gMaxZ; z *= 2 {
			zs = append(zs, z)
		}
	}
	slices.Sort(zs)
	return zs
}()

// ldpc5gShape is the smallest m whose disclosure spans FLow to FHigh times the Shannon limit at
// lifting z, and the number of positions it punctures
func ldpc5gShape(h float64, z int) (m, d int) {
	// below row 7 the four core rows alone hold the core, which needs 4/24 >= 1.6 h2(QBER)
	m0 := 7
	if 4.0/float64(ldpc5gKB+2) >= 1.6*h {
		m0 = 4
	}
	for mm := m0; mm <= 46; mm++ {
		n, checks := (ldpc5gKB+mm)*z, mm*z
		dd := max(2*z, int((float64(checks)-ldpc5gFLow*h*float64(n))/(1-ldpc5gFLow*h)))
		if dd < checks && float64(checks) >= ldpc5gFHigh*h*float64(n-dd) {
			return mm, dd
		}
	}
	return 0, 0
}

// PlanLDPC5G lays out total key bits in as few frames as lifting 384 allows, each with the smallest
// lifting that holds its share; punctured positions come from Go's seeded generator, on every node
func PlanLDPC5G(b LDPC5GBackend, qber float64, seed int64, total int) (*LDPC5GPlan, error) {
	h := binaryEntropy(max(qber, 1e-4))
	m, d := ldpc5gShape(h, ldpc5gMaxZ)
	if m == 0 {
		return nil, fmt.Errorf("ldpc5g: no 5G code fits QBER %.4f", qber)
	}
	frames := max(1, (total+(ldpc5gKB+m)*ldpc5gMaxZ-d-1)/((ldpc5gKB+m)*ldpc5gMaxZ-d))
	z := ldpc5gMaxZ
	for _, zz := range ldpc5gLiftings {
		if mm, dd := ldpc5gShape(h, zz); mm > 0 && frames*((ldpc5gKB+mm)*zz-dd) >= total {
			z, m, d = zz, mm, dd
			break
		}
	}
	key := [4]int64{int64(m), int64(d), int64(z), seed}
	if p, ok := ldpc5gPlans.Load(key); ok {
		return p.(*LDPC5GPlan), nil
	}
	n := (ldpc5gKB + m) * z
	p := &LDPC5GPlan{M: m, Z: z, N: n, Checks: m * z, Step: (d + ldpc5gSteps - 1) / ldpc5gSteps}
	rest := mrand.New(mrand.NewSource(seed)).Perm(n - 2*z)
	punct := make([]bool, n)
	// extra punctured positions are revealed first, the base graph's own two columns last
	for _, i := range rest[:d-2*z] {
		p.Punctured = append(p.Punctured, 2*z+i)
	}
	for i := range 2 * z {
		p.Punctured = append(p.Punctured, i)
	}
	for _, i := range p.Punctured {
		punct[i] = true
	}
	for i := range n {
		if !punct[i] {
			p.Key = append(p.Key, i)
		}
	}
	r, err := b.Rank(m, z, p.Punctured)
	if err != nil {
		return nil, err
	}
	p.Credit = r == d
	ldpc5gPlans.Store(key, p)
	return p, nil
}

// Digest fixes the layout, so the two sites can check they use the same one
func (p *LDPC5GPlan) Digest() []byte {
	h := sha256.New()
	binary.Write(h, binary.LittleEndian, [5]int64{int64(p.M), int64(p.Z), int64(p.Step), int64(len(p.Punctured)), map[bool]int64{true: 1}[p.Credit]})
	for _, i := range p.Punctured {
		binary.Write(h, binary.LittleEndian, int32(i))
	}
	return h.Sum(nil)
}

// Leak is one frame's disclosure: the syndrome and the tag, less the punctured positions still
// hidden when they earn credit, plus one verdict bit per attempt
func (p *LDPC5GPlan) Leak(revealed, attempts int) int {
	leak := p.Checks + ldpc5gTagBits + attempts
	if p.Credit {
		leak -= len(p.Punctured) - revealed
	}
	return leak
}

// ldpc5gAlice is Alice's frame: her bits in the key positions, fresh random punctured values
type ldpc5gAlice struct {
	x, syndrome, tagSeed, tag []byte
	revealed                  int
}

func (p *LDPC5GPlan) aliceFrame(b LDPC5GBackend, key []byte) (*ldpc5gAlice, error) {
	f := &ldpc5gAlice{x: make([]byte, p.N)}
	rnd := make([]byte, len(p.Punctured)+len(key)+ldpc5gTagBits-1)
	if _, err := rand.Read(rnd); err != nil {
		return nil, err
	}
	for i, pos := range p.Punctured {
		f.x[pos] = rnd[i] & 1
	}
	for k, pos := range p.Key[:len(key)] {
		f.x[pos] = key[k]
	}
	f.tagSeed = make([]byte, len(key)+ldpc5gTagBits-1)
	for i := range f.tagSeed {
		f.tagSeed[i] = rnd[len(p.Punctured)+i] & 1
	}
	f.tag = ComputeUniversalHashTag(key, f.tagSeed, ldpc5gTagBits)
	var err error
	f.syndrome, err = b.Syndrome(p.M, p.Z, f.x)
	return f, err
}

// reveal is the next slice of punctured values, in the plan's order
func (p *LDPC5GPlan) reveal(f *ldpc5gAlice) []byte {
	next := p.Punctured[f.revealed:min(f.revealed+p.Step, len(p.Punctured))]
	vals := make([]byte, len(next))
	for i, pos := range next {
		vals[i] = f.x[pos]
	}
	f.revealed += len(next)
	return vals
}

// ldpc5gBob is Bob's frame: his bits, what Alice has revealed, and his latest decoded string
type ldpc5gBob struct {
	y        []byte
	probs    []float64
	syndrome []byte
	tagSeed  []byte
	tag      []byte
	keyLen   int
	revealed int
	attempts int
	out      []byte
	ok       bool
}

func (p *LDPC5GPlan) bobFrame(key []byte, q []float64, syndrome, tagSeed, tag []byte) (*ldpc5gBob, error) {
	if len(syndrome) != p.Checks || len(tagSeed) != len(key)+ldpc5gTagBits-1 {
		return nil, errors.New("ldpc5g: malformed frame from Alice")
	}
	f := &ldpc5gBob{y: make([]byte, p.N), probs: make([]float64, p.N), syndrome: syndrome, tagSeed: tagSeed, tag: tag, keyLen: len(key)}
	for i := range f.probs {
		f.probs[i] = ldpc5gKnown // positions past a short last frame are known zeros
	}
	for _, pos := range p.Punctured {
		f.probs[pos] = 0.5
	}
	for k, pos := range p.Key[:len(key)] {
		f.y[pos], f.probs[pos] = key[k], min(max(q[k], 1e-6), 0.5)
	}
	return f, nil
}

// attempt decodes once; the frame is accepted only if the syndrome holds and the tag matches
func (p *LDPC5GPlan) attempt(b LDPC5GBackend, f *ldpc5gBob) error {
	f.attempts++
	iters := ldpc5gStepIter
	if f.revealed == len(p.Punctured) {
		iters = ldpc5gMaxIter
	}
	x, converged, err := b.Decode(p.M, p.Z, f.y, f.probs, f.syndrome, iters)
	if err != nil {
		return err
	}
	if len(x) != p.N {
		return fmt.Errorf("ldpc5g: decoder returned %d bits, expected %d", len(x), p.N)
	}
	f.out = make([]byte, f.keyLen)
	for k, pos := range p.Key[:f.keyLen] {
		f.out[k] = x[pos]
	}
	f.ok = converged && VerifyUniversalHash(f.out, f.tag, f.tagSeed, ldpc5gTagBits)
	return nil
}

// apply pins revealed punctured values for the next attempt
func (p *LDPC5GPlan) apply(f *ldpc5gBob, vals []byte) error {
	next := p.Punctured[f.revealed:min(f.revealed+p.Step, len(p.Punctured))]
	if len(vals) != len(next) {
		return fmt.Errorf("ldpc5g: %d revealed values, expected %d", len(vals), len(next))
	}
	for i, pos := range next {
		f.y[pos], f.probs[pos] = vals[i]&1, ldpc5gKnown
	}
	f.revealed += len(next)
	return nil
}

// LDPC5GResult summarizes one reconciliation; both sites of a two-party run get the same numbers
type LDPC5GResult struct {
	Frames, Failed, Attempts, Revealed, LeakedBits, BitsCorrected int
	M, Z, FrameKeyBits                                            int
	Credit                                                        bool
}

// LDPC5GReconcile corrects bob onto alice in one process, frame by frame on every core, with
// exactly the messages and accounting of the two-party protocol
func LDPC5GReconcile(b LDPC5GBackend, alice, bob []byte, q []float64, qber float64, seed int64) ([]byte, LDPC5GResult, error) {
	var res LDPC5GResult
	p, err := PlanLDPC5G(b, qber, seed, len(alice))
	if err != nil {
		return nil, res, err
	}
	res.M, res.Z, res.FrameKeyBits, res.Credit = p.M, p.Z, len(p.Key), p.Credit
	kl := len(p.Key)
	frames := (len(alice) + kl - 1) / kl
	corrected := make([]byte, len(bob))
	type out struct {
		attempts, revealed, corrected int
		ok                            bool
		err                           error
	}
	outs := make([]out, frames)
	next := make(chan int, frames)
	for f := range frames {
		next <- f
	}
	close(next)
	var wg sync.WaitGroup
	for range min(runtime.NumCPU(), frames) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range next {
				lo, hi := f*kl, min((f+1)*kl, len(alice))
				af, err := p.aliceFrame(b, alice[lo:hi])
				if err != nil {
					outs[f].err = err
					continue
				}
				bf, err := p.bobFrame(bob[lo:hi], q[lo:hi], af.syndrome, af.tagSeed, af.tag)
				if err != nil {
					outs[f].err = err
					continue
				}
				for {
					if err := p.attempt(b, bf); err != nil {
						outs[f].err = err
						break
					}
					if bf.ok || bf.revealed == len(p.Punctured) {
						break
					}
					if err := p.apply(bf, p.reveal(af)); err != nil {
						outs[f].err = err
						break
					}
				}
				copy(corrected[lo:hi], bf.out)
				o := out{attempts: bf.attempts, revealed: bf.revealed, ok: bf.ok}
				for i := range bf.out {
					if bf.out[i] != bob[lo+i] {
						o.corrected++
					}
				}
				outs[f] = o
			}
		}()
	}
	wg.Wait()
	for _, o := range outs {
		if o.err != nil {
			return nil, res, o.err
		}
		res.Frames++
		res.Attempts += o.attempts
		res.Revealed += o.revealed
		res.LeakedBits += p.Leak(o.revealed, o.attempts)
		res.BitsCorrected += o.corrected
		if !o.ok {
			res.Failed++
		}
	}
	return corrected, res, nil
}

// LDPC5GRunConfig configures RunLDPC5G, the 5G NR LDPC sibling of RunPolar
type LDPC5GRunConfig struct {
	Backend             LDPC5GBackend
	CoincidenceWindowPS int64
	SampleFraction      float64
	Seed                int64 // public: the sample and the punctured positions
	ChunkSize           int
	KeyAlignBits        int
	TimingBins          int // >0: per-bit error rates from public timing for Bob's decoder
	Asymptotic          bool
	FiniteKey           FiniteKeyConfig
	ExpectedQBER        float64
}

// LDPC5GRunResult mirrors PolarRunResult
type LDPC5GRunResult struct {
	SiftedBits    int
	QBER          QBERResult
	Timing        *TimingReport
	FiniteKey     *FiniteKeyReport
	LDPC          LDPC5GResult
	CRCVerify     CRCVerifyResult
	LeakedBits    int
	FinalKeyBits  []byte
	FinalKeyBytes []byte
	Seed          []byte
}

// RunLDPC5G sifts, sacrifices a sample, reconciles with 5G NR LDPC, CRC-verifies and
// privacy-amplifies, the same stages around reconciliation as RunPolar
func RunLDPC5G(aliceEvents, bobEvents []DetectionEvent, cfg LDPC5GRunConfig) (LDPC5GRunResult, error) {
	var res LDPC5GRunResult
	if cfg.Backend == nil {
		return res, errors.New("ldpc5g: no backend")
	}
	sifted, sample, rem, qber := siftSample(aliceEvents, bobEvents, cfg.CoincidenceWindowPS, cfg.SampleFraction, cfg.ExpectedQBER, cfg.FiniteKey, cfg.Seed)
	res.SiftedBits, res.QBER = sifted, qber
	q := make([]float64, len(rem.Alice))
	for i := range q {
		q[i] = qber.QBER
	}
	if cfg.TimingBins > 0 {
		tq, report, err := timingRates(sample, rem, cfg.CoincidenceWindowPS, cfg.TimingBins)
		if err != nil {
			return res, err
		}
		q, res.Timing = tq, report
	}
	corrected, lr, err := LDPC5GReconcile(cfg.Backend, rem.Alice, rem.Bob, q, qber.QBER, cfg.Seed)
	if err != nil {
		return res, err
	}
	res.LDPC = lr
	res.CRCVerify = CRCVerify(rem.Alice, corrected, cfg.ChunkSize)
	res.LeakedBits = lr.LeakedBits + res.CRCVerify.LeakedBits
	key, seed, fkr, err := distill(res.CRCVerify, sample, rem, res.LeakedBits, qber.QBER, cfg.Asymptotic, cfg.FiniteKey, cfg.KeyAlignBits)
	res.FiniteKey = fkr
	if err != nil {
		return res, err
	}
	res.FinalKeyBits, res.FinalKeyBytes, res.Seed = key, packToBytes(key), seed
	return res, nil
}
