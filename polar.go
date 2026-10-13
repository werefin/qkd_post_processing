package qkd_post_processing

import (
	"fmt"
	"math"
	mathbits "math/bits"
	"sort"
)

// Polar-code reconciliation (Arikan 2009; Jouguet & Kunz-Jacques 2014; SCL per Yan et al. 2018):
// Alice reveals her transform's frozen-position values, Bob list-decodes from his noisy bits

// PolarTransform applies G_N=F^{⊗n}, F=[[1,0],[1,1]], no bit reversal, so it is self-inverse
// and x=(T(u1)^T(u2), T(u2)); bits must have a power-of-two length
func PolarTransform(bits []byte) ([]byte, error) {
	n := len(bits)
	if n == 0 || n&(n-1) != 0 {
		return nil, fmt.Errorf("polar: block length must be a power of two, got %d", n)
	}
	x := append([]byte(nil), bits...)
	for width := n; width > 1; width /= 2 {
		half := width / 2
		for start := 0; start < n; start += width {
			for i := start; i < start+half; i++ {
				x[i] ^= x[i+half]
			}
		}
	}
	return x, nil
}

// gaPhi is Chung's approximation of the Gaussian-approximation phi function,
// clamped to [0,1] since the small-x branch overshoots 1 near zero
func gaPhi(x float64) float64 {
	if x <= 0 {
		return 1
	}
	if x < 10 {
		return math.Min(1, math.Exp(-0.4527*math.Pow(x, 0.86)+0.0218))
	}
	return math.Sqrt(math.Pi/x) * math.Exp(-x/4) * (1 - 10/(7*x))
}

// gaPhiBranchEdge is the small-x branch's value at x=10, where gaPhiInv switches branch
var gaPhiBranchEdge = math.Exp(-0.4527*math.Pow(10, 0.86) + 0.0218)

// gaPhiInv inverts gaPhi: closed form on the small-x branch, bisection on the
// large-x one; both branches are decreasing so this is well defined
func gaPhiInv(y float64) float64 {
	if y >= 1 {
		return 0
	}
	if y <= 0 {
		return gaMaxMean
	}
	if y >= gaPhiBranchEdge {
		return math.Pow((0.0218-math.Log(y))/0.4527, 1/0.86)
	}
	lo, hi := 10.0, gaMaxMean
	for range 50 {
		mid := (lo + hi) / 2
		if gaPhi(mid) > y {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// gaMaxMean caps a mean LLR; beyond it a channel is effectively noiseless
const gaMaxMean = 1e4

// gaMinus is the check-node (worse) combine of two channel means
func gaMinus(a, b float64) float64 {
	return gaPhiInv(1 - (1-gaPhi(a))*(1-gaPhi(b)))
}

// gaMeansFrom propagates per-input means down the decoder's own recursion to
// per-position means; inputs may differ, as with timing-aware soft reconciliation
func gaMeansFrom(m []float64) []float64 {
	n := len(m)
	if n == 1 {
		return []float64{m[0]}
	}
	half := n / 2
	left, right := make([]float64, half), make([]float64, half)
	for j := range half {
		left[j] = gaMinus(m[j], m[half+j])
		right[j] = math.Min(gaMaxMean, m[j]+m[half+j])
	}
	return append(gaMeansFrom(left), gaMeansFrom(right)...)
}

// gaInitialMean is the LLR mean of a BI-AWGN channel with BSC(p)'s capacity, via
// Brannstrom's J-function inverse; m=sigma^2/2 for a consistent Gaussian LLR
func gaInitialMean(p float64) float64 {
	const h1, h2, h3 = 0.3073, 0.8935, 1.1064
	capacity := 1 - binaryEntropy(p)
	if capacity <= 0 {
		return 0
	}
	if capacity >= 1 {
		return gaMaxMean
	}
	sigma := math.Pow(-math.Log2(1-math.Pow(capacity, 1/h3))/h1, 1/(2*h2))
	return math.Min(gaMaxMean, sigma*sigma/2)
}

// gaMeans gives each synthetic channel's mean LLR (higher is more reliable) for 2^n
// identical BSC(p) inputs; left half is the minus (worse) branch at each level
func gaMeans(n int, p float64) []float64 {
	m := make([]float64, 1<<n)
	m0 := gaInitialMean(p)
	for i := range m {
		m[i] = m0
	}
	return gaMeansFrom(m)
}

// PolarCode fixes which of N synthetic channels are frozen, for one (N,K,designQBER)
// design point; both sides derive this independently, no secret/random draw involved
type PolarCode struct {
	N, K      int
	Frozen    []bool // length N, Frozen[i] marks a disclosed (not information) position
	FrozenIdx []int  // ascending indices where Frozen[i], length N-K
}

// BuildPolarCode freezes the N-K least reliable synthetic channels of a BSC(designQBER),
// ranked by capacity-matched Gaussian approximation (Trifonov 2012), keeping K information channels
func BuildPolarCode(n, k int, designQBER float64) (*PolarCode, error) {
	if n <= 0 || n&(n-1) != 0 {
		return nil, fmt.Errorf("polar: N must be a power of two, got %d", n)
	}
	if k <= 0 || k > n {
		return nil, fmt.Errorf("polar: K must be in (0,N], got %d of %d", k, n)
	}
	levels := mathbits.Len(uint(n)) - 1
	return codeFromMeans(gaMeans(levels, clampProb(designQBER)), k), nil
}

// codeFromMeans freezes the len(mean)-k positions with the lowest mean LLR
func codeFromMeans(mean []float64, k int) *PolarCode {
	n := len(mean)
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	// Most reliable (largest mean LLR) first; ties broken by index for determinism
	sort.Slice(idx, func(a, b int) bool {
		if mean[idx[a]] != mean[idx[b]] {
			return mean[idx[a]] > mean[idx[b]]
		}
		return idx[a] < idx[b]
	})

	frozen := make([]bool, n)
	for _, i := range idx[k:] {
		frozen[i] = true
	}
	frozenIdx := make([]int, 0, n-k)
	for i := range n {
		if frozen[i] {
			frozenIdx = append(frozenIdx, i)
		}
	}
	return &PolarCode{N: n, K: k, Frozen: frozen, FrozenIdx: frozenIdx}
}

// channelLLRs converts receivedBits through a BSC(qber) model into log-likelihood
// ratios L=log(P(x=0|y)/P(x=1|y)), the same sign convention LDPCDecode's intrinsic uses
func channelLLRs(receivedBits []byte, qber float64) []float64 {
	p := clampProb(qber)
	l0 := math.Log((1 - p) / p)
	out := make([]float64, len(receivedBits))
	for i, b := range receivedBits {
		if b == 0 {
			out[i] = l0
		} else {
			out[i] = -l0
		}
	}
	return out
}

// llrF is the min-sum SC "check" combine sign(a)sign(b)min(|a|,|b|), the standard hardware
// approximation of 2atanh(tanh(a/2)tanh(b/2)); polar_test.go checks it still tracks ML
func llrF(a, b float64) float64 {
	m := math.Min(math.Abs(a), math.Abs(b))
	if (a < 0) != (b < 0) {
		return -m
	}
	return m
}

// llrG is the exact SC "bit" combine, g(a,b,u)=b+(-1)^u*a, given u already decided
func llrG(a, b float64, u byte) float64 {
	if u == 0 {
		return b + a
	}
	return b - a
}

// hardDecision maps an LLR to its most likely bit, ties to 0
func hardDecision(llr float64) byte {
	if llr < 0 {
		return 1
	}
	return 0
}

// bitPenalty is one bit's SCL path-metric cost (Balatsoukas-Stimming et al. 2015, approximate
// form): 0 when u agrees with the LLR's hard decision, |LLR| when it goes against it
func bitPenalty(llr float64, u byte) float64 {
	if u == hardDecision(llr) {
		return 0
	}
	return math.Abs(llr)
}

// scDecodeFast is plain SC (list size 1) with no per-node allocation: scratch[d] holds
// depth d's combined LLRs, bits/code receive U and its re-encoding T(U)
func scDecodeFast(llr []float64, frozen []bool, frozenVals, bits, code []byte, scratch [][]float64, depth int) {
	n := len(llr)
	if n == 1 {
		u := frozenVals[0]
		if !frozen[0] {
			u = hardDecision(llr[0])
		}
		bits[0], code[0] = u, u
		return
	}
	half := n / 2
	lv := scratch[depth][:half]
	for j := range half {
		lv[j] = llrF(llr[j], llr[half+j])
	}
	scDecodeFast(lv, frozen[:half], frozenVals[:half], bits[:half], code[:half], scratch, depth+1)
	for j := range half {
		lv[j] = llrG(llr[j], llr[half+j], code[j])
	}
	scDecodeFast(lv, frozen[half:], frozenVals[half:], bits[half:], code[half:], scratch, depth+1)
	for j := range half {
		code[j] ^= code[half+j]
	}
}

// leafBits are shared read-only one-bit slices, so SCL leaves allocate no per-candidate bits
var leafBits = [2][]byte{{0}, {1}}

// scPath is one live SCL candidate at a recursive node: LLR is its input, Bits/Code its
// decoded U-domain bits and their re-encoding T(Bits), Parent the incoming path it extends
type scPath struct {
	LLR    []float64
	Bits   []byte
	Code   []byte
	Metric float64
	Parent int
}

// prunePaths keeps the listSize lowest-metric candidates; insertion sort, since
// at most 2L entries and it allocates nothing, stable so ties keep their order
func prunePaths(paths []scPath, listSize int) []scPath {
	for i := 1; i < len(paths); i++ {
		for j := i; j > 0 && paths[j].Metric < paths[j-1].Metric; j-- {
			paths[j], paths[j-1] = paths[j-1], paths[j]
		}
	}
	if len(paths) > listSize {
		paths = paths[:listSize]
	}
	return paths
}

// sclDecoder holds per-depth arenas reused across the whole tree; results get two slots
// per depth so a left child's output survives while its right sibling decodes
type sclDecoder struct {
	listSize int
	llr      [][]float64   // depth d: child-input LLRs, listSize*(N>>(d+1))
	in       [][]scPath    // depth d: child-input paths, listSize entries
	out      [2][][]scPath // [side][depth]: result paths, 2*listSize entries
	bits     [2][][]byte   // [side][depth]: result Bits+Code, 2*listSize*(N>>d) bytes
}

func newSCLDecoder(n, listSize int) *sclDecoder {
	levels := mathbits.Len(uint(n))
	d := &sclDecoder{listSize: listSize}
	d.llr = make([][]float64, levels)
	d.in = make([][]scPath, levels)
	for s := range 2 {
		d.out[s] = make([][]scPath, levels)
		d.bits[s] = make([][]byte, levels)
	}
	for dep := range levels {
		size := n >> dep
		d.llr[dep] = make([]float64, listSize*max(size/2, 1))
		d.in[dep] = make([]scPath, listSize)
		for s := range 2 {
			d.out[s][dep] = make([]scPath, 0, 2*listSize)
			d.bits[s][dep] = make([]byte, 2*listSize*size)
		}
	}
	return d
}

// decode list-decodes one node; since x=(T(u1)^T(u2), T(u2)), u1 decodes from
// f(L_left,L_right), then u2 from g(L_left,L_right,T(u1hat))
func (d *sclDecoder) decode(paths []scPath, frozen []bool, frozenVals []byte, depth, side int) []scPath {
	n := len(paths[0].LLR)
	next := d.out[side][depth][:0]
	if n == 1 {
		for i, p := range paths {
			if frozen[0] {
				u := frozenVals[0]
				next = append(next, scPath{Bits: leafBits[u], Code: leafBits[u], Metric: p.Metric + bitPenalty(p.LLR[0], u), Parent: i})
				continue
			}
			for u := range byte(2) {
				next = append(next, scPath{Bits: leafBits[u], Code: leafBits[u], Metric: p.Metric + bitPenalty(p.LLR[0], u), Parent: i})
			}
		}
		return prunePaths(next, d.listSize)
	}

	half := n / 2
	buf, in := d.llr[depth], d.in[depth][:len(paths)]
	for i, p := range paths {
		lv := buf[i*half : (i+1)*half]
		for j := range lv {
			lv[j] = llrF(p.LLR[j], p.LLR[half+j])
		}
		in[i] = scPath{LLR: lv, Metric: p.Metric}
	}
	leftOut := d.decode(in, frozen[:half], frozenVals[:half], depth+1, 0)

	// leftIn is dead once leftOut exists (Parent indexes paths 1:1), so reuse its arenas
	in = d.in[depth][:len(leftOut)]
	for k, lp := range leftOut {
		parent := paths[lp.Parent]
		lv := buf[k*half : (k+1)*half]
		for j := range lv {
			lv[j] = llrG(parent.LLR[j], parent.LLR[half+j], lp.Code[j])
		}
		in[k] = scPath{LLR: lv, Metric: lp.Metric}
	}
	rightOut := d.decode(in, frozen[half:], frozenVals[half:], depth+1, 1)

	store := d.bits[side][depth]
	for m, rp := range rightOut {
		lp := leftOut[rp.Parent]
		bits := store[2*m*n : (2*m+1)*n]
		code := store[(2*m+1)*n : (2*m+2)*n]
		copy(bits, lp.Bits)
		copy(bits[half:], rp.Bits)
		for j := range half {
			code[j] = lp.Code[j] ^ rp.Code[j]
			code[half+j] = rp.Code[j]
		}
		next = append(next, scPath{Bits: bits, Code: code, Metric: rp.Metric, Parent: lp.Parent})
	}
	return next
}

// PolarDecodeConfig tunes successive-cancellation list decoding
type PolarDecodeConfig struct {
	ListSize int // SCL path count L, 0 defaults to 1 (plain SC, a single greedy path)
}

const defaultPolarListSize = 1

// PolarSCDecode SC/SCL-decodes Bob's raw bits plus Alice's revealed frozen values (one per
// code.FrozenIdx, in order) into her full transformed vector U
func PolarSCDecode(code *PolarCode, receivedBits, frozenVals []byte, qber float64, cfg PolarDecodeConfig) ([]byte, error) {
	if len(receivedBits) != code.N {
		return nil, fmt.Errorf("polar: receivedBits has %d bits, code expects %d", len(receivedBits), code.N)
	}
	return PolarDecodeLLR(code, channelLLRs(receivedBits, qber), frozenVals, cfg)
}

// PolarDecodeLLR is PolarSCDecode over caller-supplied per-bit LLRs, e.g. SoftLLRs'
// timing-aware ones instead of one flat QBER for the whole block
func PolarDecodeLLR(code *PolarCode, llr []float64, frozenVals []byte, cfg PolarDecodeConfig) ([]byte, error) {
	if len(llr) != code.N {
		return nil, fmt.Errorf("polar: llr has %d entries, code expects %d", len(llr), code.N)
	}
	if len(frozenVals) != len(code.FrozenIdx) {
		return nil, fmt.Errorf("polar: frozenVals has %d entries, code expects %d", len(frozenVals), len(code.FrozenIdx))
	}
	listSize := cfg.ListSize
	if listSize <= 0 {
		listSize = defaultPolarListSize
	}

	frozenValsFull := make([]byte, code.N)
	for i, idx := range code.FrozenIdx {
		frozenValsFull[idx] = frozenVals[i]
	}

	if listSize == 1 {
		levels := mathbits.Len(uint(code.N)) - 1
		scratch := make([][]float64, levels)
		for d := range scratch {
			scratch[d] = make([]float64, code.N>>(d+1))
		}
		bits, enc := make([]byte, code.N), make([]byte, code.N)
		scDecodeFast(llr, code.Frozen, frozenValsFull, bits, enc, scratch, 0)
		return bits, nil
	}
	paths := newSCLDecoder(code.N, listSize).decode([]scPath{{LLR: llr}}, code.Frozen, frozenValsFull, 0, 0)
	best := 0
	for i := range paths {
		if paths[i].Metric < paths[best].Metric {
			best = i
		}
	}
	return append([]byte(nil), paths[best].Bits...), nil
}

// PolarReconcileResult summarizes one block's correction attempt
type PolarReconcileResult struct {
	CorrectedBob  []byte
	BitsCorrected int // bits that differed between Bob's original and corrected block
	LeakedBits    int // classical-channel bits spent disclosing Alice's frozen values
}

// PolarPass reconciles one code.N-bit block: Alice transforms her block, discloses its
// frozen-position values, Bob SC/SCL-decodes toward them, both invert the transform
func PolarPass(code *PolarCode, alice, bob []byte, qber float64, cfg PolarDecodeConfig) (PolarReconcileResult, error) {
	aliceU, err := PolarTransform(alice)
	if err != nil {
		return PolarReconcileResult{}, err
	}
	frozenVals := make([]byte, len(code.FrozenIdx))
	for i, idx := range code.FrozenIdx {
		frozenVals[i] = aliceU[idx]
	}

	bobU, err := PolarSCDecode(code, bob, frozenVals, qber, cfg)
	if err != nil {
		return PolarReconcileResult{}, err
	}
	correctedBob, err := PolarTransform(bobU) // self-inverse: undoes Alice's transform
	if err != nil {
		return PolarReconcileResult{}, err
	}

	corrections := 0
	for i := range bob {
		if bob[i] != correctedBob[i] {
			corrections++
		}
	}
	return PolarReconcileResult{
		CorrectedBob:  correctedBob,
		BitsCorrected: corrections,
		LeakedBits:    len(code.FrozenIdx),
	}, nil
}

// PolarReconcile reconciles every code.N-bit block of alice/bob like PolarPass, zero-padding
// a short final block the same way LDPCReconcile does
func PolarReconcile(code *PolarCode, alice, bob []byte, qber float64, cfg PolarDecodeConfig) (correctedBob []byte, totalBlocks, bitsCorrected, leakedBits int, err error) {
	n := code.N
	total := len(alice)
	if total == 0 {
		return nil, 0, 0, 0, nil
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
		res, perr := PolarPass(code, aliceBlock, bobBlock, qber, cfg)
		if perr != nil {
			return nil, 0, 0, 0, perr
		}
		copy(correctedBob[bs:be], res.CorrectedBob[:blen])
		bitsCorrected += res.BitsCorrected
		leakedBits += res.LeakedBits
	}
	return correctedBob, numBlocks, bitsCorrected, leakedBits, nil
}
