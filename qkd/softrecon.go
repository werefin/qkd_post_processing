package qkd

import (
	"fmt"
	"math"
)

// Timing-aware soft reconciliation: each sifted bit's public offset dt=t_bob-t_alice
// predicts its own error rate, accidentals spread across the window, true pairs peak near 0

// TimingErrorModel is q(dt), the error rate per signed-offset bin over [-WindowPS, WindowPS],
// estimated from the sacrificed QBER sample only, so it is public like dt itself
type TimingErrorModel struct {
	WindowPS  int64
	ErrorRate []float64 // per bin, Laplace-smoothed, clamped to [qMin, 0.5]
	Counts    []int     // sample bits per bin
	Errors    []int     // sample errors per bin
}

// qMinTiming floors a bin's rate so a clean-looking bin never claims certainty it can't back up
const qMinTiming = 1e-4

// EstimateTimingErrorModel bins the sample by dt; (errors+1)/(count+2) keeps a sparse bin
// near 0.5 (no information) instead of a confident 0 or 1
func EstimateTimingErrorModel(sampleAlice, sampleBob []byte, sampleDt []int64, windowPS int64, bins int) (TimingErrorModel, error) {
	if bins < 1 || windowPS <= 0 {
		return TimingErrorModel{}, fmt.Errorf("timing model: need bins>=1 and windowPS>0, got %d, %d", bins, windowPS)
	}
	if len(sampleAlice) != len(sampleBob) || len(sampleAlice) != len(sampleDt) {
		return TimingErrorModel{}, fmt.Errorf("timing model: sample slices differ in length")
	}
	m := TimingErrorModel{WindowPS: windowPS, ErrorRate: make([]float64, bins), Counts: make([]int, bins), Errors: make([]int, bins)}
	for i, dt := range sampleDt {
		b := m.bin(dt)
		m.Counts[b]++
		if sampleAlice[i] != sampleBob[i] {
			m.Errors[b]++
		}
	}
	for b := range bins {
		q := (float64(m.Errors[b]) + 1) / (float64(m.Counts[b]) + 2)
		m.ErrorRate[b] = math.Min(0.5, math.Max(qMinTiming, q))
	}
	return m, nil
}

// bin maps dt onto [0,bins), clamping anything outside the window to the edge bins
func (m TimingErrorModel) bin(dt int64) int {
	bins := len(m.ErrorRate)
	b := int(float64(dt+m.WindowPS) / float64(2*m.WindowPS) * float64(bins))
	return max(0, min(bins-1, b))
}

// Rate is the model's error probability for a bit with offset dt
func (m TimingErrorModel) Rate(dt int64) float64 {
	return m.ErrorRate[m.bin(dt)]
}

// Rates maps every bit's offset to its own error probability
func (m TimingErrorModel) Rates(dt []int64) []float64 {
	q := make([]float64, len(dt))
	for i, d := range dt {
		q[i] = m.Rate(d)
	}
	return q
}

// SoftLLRs gives each received bit its own BSC(q_i) log-likelihood ratio, log(P(x=0|y)/P(x=1|y));
// q_i=0.5 gives 0, the decoder's "no information" value
func SoftLLRs(receivedBits []byte, q []float64) []float64 {
	out := make([]float64, len(receivedBits))
	for i, b := range receivedBits {
		if q[i] >= 0.5 {
			continue // exactly 0: the bit carries no information about Alice's
		}
		p := clampProb(q[i])
		l := math.Log((1 - p) / p)
		if b == 1 {
			l = -l
		}
		out[i] = l
	}
	return out
}

// SlepianWolfBounds returns the minimum disclosure per bit with and without timing:
// mean h2(q_i) versus h2(mean q_i); by Jensen (h2 concave) soft never exceeds flat
func SlepianWolfBounds(q []float64) (soft, flat float64) {
	if len(q) == 0 {
		return 0, 0
	}
	var sumH, sumQ float64
	for _, qi := range q {
		sumH += binaryEntropy(qi)
		sumQ += qi
	}
	n := float64(len(q))
	return sumH / n, binaryEntropy(sumQ / n)
}

// polarAdaptive* set rate-adaptive retries: a tag per attempt, the margin raised by a step
const (
	polarAdaptiveTagBits  = 32
	polarAdaptiveStep     = 0.02
	polarAdaptiveMaxSteps = 15
)

// PolarReconcileSoft builds each block's code and starting rate from its own per-bit q (public),
// decodes with per-bit LLRs, and raises the rate step by step until a block hash matches
func PolarReconcileSoft(alice, bob []byte, q []float64, blockLength int, margin float64, cfg PolarDecodeConfig) (correctedBob []byte, totalBlocks, bitsCorrected, leakedBits int, err error) {
	n := blockLength
	if n < 2 || n&(n-1) != 0 {
		return nil, 0, 0, 0, fmt.Errorf("polar: block length must be a power of two >= 2, got %d", n)
	}
	total := len(alice)
	if total == 0 {
		return nil, 0, 0, 0, nil
	}
	numBlocks := (total + n - 1) / n
	correctedBob = make([]byte, total)

	aliceBlock, bobBlock := make([]byte, n), make([]byte, n)
	qBlock, means := make([]float64, n), make([]float64, n)
	for bi := range numBlocks {
		bs := bi * n
		be := min(bs+n, total)
		blen := be - bs
		copy(aliceBlock, alice[bs:be])
		copy(bobBlock, bob[bs:be])
		copy(qBlock, q[bs:be])
		for k := blen; k < n; k++ {
			aliceBlock[k], bobBlock[k], qBlock[k] = 0, 0, qMinTiming // padding is known 0 on both sides
		}

		capacity := 0.0
		for i, qi := range qBlock {
			means[i] = gaInitialMean(qi)
			capacity += 1 - binaryEntropy(qi)
		}
		uMeans := gaMeansFrom(means)
		aliceU, terr := PolarTransform(aliceBlock)
		if terr != nil {
			return nil, 0, 0, 0, terr
		}
		llr := SoftLLRs(bobBlock, qBlock)

		// rate-adaptive: on a hash mismatch, freeze the next least reliable positions and retry;
		// frozen sets are nested, so leakage is the final frozen count plus one tag per attempt
		var fixed []byte
		var k int
		for attempt := 0; attempt <= polarAdaptiveMaxSteps; attempt++ {
			m := margin + float64(attempt)*polarAdaptiveStep
			k = max(1, min(n-1, int(capacity-m*float64(blen)))) // margin on real bits; padding is known
			code := codeFromMeans(uMeans, k)
			frozenVals := make([]byte, len(code.FrozenIdx))
			for i, idx := range code.FrozenIdx {
				frozenVals[i] = aliceU[idx]
			}
			bobU, derr := PolarDecodeLLR(code, llr, frozenVals, cfg)
			if derr != nil {
				return nil, 0, 0, 0, derr
			}
			fixed, _ = PolarTransform(bobU)
			leakedBits += polarAdaptiveTagBits
			seed, serr := UniversalHashSeed(polarAdaptiveTagBits, blen)
			if serr != nil {
				return nil, 0, 0, 0, serr
			}
			tag := ComputeUniversalHashTag(aliceBlock[:blen], seed, polarAdaptiveTagBits)
			if VerifyUniversalHash(fixed[:blen], tag, seed, polarAdaptiveTagBits) || k == 1 {
				break
			}
		}
		for i := range blen {
			if fixed[i] != bobBlock[i] {
				bitsCorrected++
			}
		}
		copy(correctedBob[bs:be], fixed[:blen])
		leakedBits += n - k
	}
	return correctedBob, numBlocks, bitsCorrected, leakedBits, nil
}

// LDPCReconcileSoft is LDPCReconcile with per-bit LLRs from q; the rate is fixed by h,
// so timing shows up as fewer unconverged blocks rather than fewer leaked bits
func LDPCReconcileSoft(h *LDPCMatrix, alice, bob []byte, q []float64, cfg LDPCDecodeConfig) (correctedBob []byte, convergedBlocks, totalBlocks, leakedBits int) {
	n := h.N
	total := len(alice)
	if total == 0 {
		return nil, 0, 0, 0
	}
	numBlocks := (total + n - 1) / n
	correctedBob = make([]byte, total)

	aliceBlock, bobBlock := make([]byte, n), make([]byte, n)
	qBlock := make([]float64, n)
	aliceSynd := make([]byte, h.M)
	st := newLDPCDecodeScratch(h)
	for bi := range numBlocks {
		bs := bi * n
		be := min(bs+n, total)
		blen := be - bs
		copy(aliceBlock, alice[bs:be])
		copy(bobBlock, bob[bs:be])
		copy(qBlock, q[bs:be])
		for k := blen; k < n; k++ {
			aliceBlock[k], bobBlock[k], qBlock[k] = 0, 0, qMinTiming
		}
		h.syndromeInto(aliceBlock, aliceSynd)
		corrected, converged, _ := h.decodeWith(st, bobBlock, aliceSynd, SoftLLRs(bobBlock, qBlock), 0, cfg)
		copy(correctedBob[bs:be], corrected[:blen])
		leakedBits += h.M
		if converged {
			convergedBlocks++
		}
	}
	return correctedBob, convergedBlocks, numBlocks, leakedBits
}

// TimingReport is the timing model a run used and the two per-bit disclosure bounds it implies
type TimingReport struct {
	Model     TimingErrorModel
	SoftBound float64 // mean h2(q_i) over the reconciled bits, the bound with timing
	FlatBound float64 // h2(mean q_i), the bound a flat-QBER code is held to
}

// timingRates fits q(dt) on the sacrificed sample and gives every remaining bit its own rate
func timingRates(sample, rem timedBits, windowPS int64, bins int) ([]float64, *TimingReport, error) {
	if len(sample.Alice) == 0 {
		return nil, nil, fmt.Errorf("timing model: needs a non-empty QBER sample (SampleFraction > 0)")
	}
	m, err := EstimateTimingErrorModel(sample.Alice, sample.Bob, sample.Dt, windowPS, bins)
	if err != nil {
		return nil, nil, err
	}
	q := m.Rates(rem.Dt)
	soft, flat := SlepianWolfBounds(q)
	return q, &TimingReport{Model: m, SoftBound: soft, FlatBound: flat}, nil
}

// siftSampleWithTiming is siftAndSampleQBER keeping each bit's dt; same seed gives the
// same sample and order, so flat and soft runs see identical bits
func siftSampleWithTiming(aliceEvents, bobEvents []DetectionEvent, windowPS int64, sampleFraction float64, seed int64) (siftedBits int, sample, rem timedBits, qber QBERResult) {
	return siftSample(aliceEvents, bobEvents, windowPS, sampleFraction, 0, FiniteKeyConfig{}, seed)
}

// siftSample is siftSampleWithTiming that, given expectedQBER>0, replaces sampleFraction with
// OptimalSampleFraction for the sifted count, before any sample bit is looked at
func siftSample(aliceEvents, bobEvents []DetectionEvent, windowPS int64, sampleFraction, expectedQBER float64, fk FiniteKeyConfig, seed int64) (siftedBits int, sample, rem timedBits, qber QBERResult) {
	sifted := Sift(aliceEvents, bobEvents, windowPS)
	if expectedQBER > 0 {
		sampleFraction = OptimalSampleFraction(len(sifted), expectedQBER, fk)
	}
	order := siftSampleOrder(len(sifted), seed)
	sampleN := max(0, min(int(float64(len(sifted))*sampleFraction), len(sifted)))

	pick := func(idxs []int) timedBits {
		t := timedBits{Alice: make([]byte, len(idxs)), Bob: make([]byte, len(idxs)), Dt: make([]int64, len(idxs)), Basis: make([]Basis, len(idxs))}
		for i, idx := range idxs {
			s := sifted[idx]
			t.Alice[i], t.Bob[i] = s.AliceBit, s.BobBit
			t.Basis[i] = aliceEvents[s.AliceIdx].Basis
			t.Dt[i] = bobEvents[s.BobIdx].TimestampPS - aliceEvents[s.AliceIdx].TimestampPS
		}
		return t
	}
	sample, rem = pick(order[:sampleN]), pick(order[sampleN:])

	errBits := 0
	for i := range sample.Alice {
		if sample.Alice[i] != sample.Bob[i] {
			errBits++
		}
	}
	rate := 0.0
	if sampleN > 0 {
		rate = float64(errBits) / float64(sampleN)
	}
	return len(sifted), sample, rem, QBERResult{ErrorBits: errBits, SampleBits: sampleN, QBER: rate}
}

// timedBits is a set of sifted bit pairs with their public offsets and bases, index-aligned
type timedBits struct {
	Alice, Bob []byte
	Dt         []int64
	Basis      []Basis
}
