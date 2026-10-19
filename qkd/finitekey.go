// Non-asymptotic replacement for SecureKeyLength: see
// doc/finite_key_procedure.md and the formula's own doc comment below
package qkd

import "math"

// PhaseErrorEstimate is one PA block's privacy-amplification input
type PhaseErrorEstimate struct {
	Mu           float64 // statistical fluctuation term
	Phi          float64 // phase error bound to feed FiniteKeySecureKeyLength
	Conservative bool
}

// phaseErrorMu: mu = sqrt((ns+k)/(ns*k) * (k+1)/k * ln(1/epsPE)), the
// Tomamichel et al. fluctuation term, phase error estimated at confidence epsPE
func phaseErrorMu(ns, k int, epsPE float64) float64 {
	if ns <= 0 || k <= 0 || epsPE <= 0 || epsPE >= 1 {
		return 1 // no usable bound; caller should treat the block as insecure
	}
	nsF, kF := float64(ns), float64(k)
	return math.Sqrt((nsF + kF) / (nsF * kF) * (kF + 1) / kF * math.Log(1/epsPE))
}

// EstimatePhaseError computes phi from q, the measured error rate in the
// sacrificed conjugate-basis sample (not the kept bits)
func EstimatePhaseError(q float64, ns, k int, epsPE float64, nSifted, nSurvivingEC int, conservative bool) PhaseErrorEstimate {
	mu := phaseErrorMu(ns, k, epsPE)
	phi := q + mu
	if conservative {
		if nSurvivingEC <= 0 {
			phi = 0.5 // no surviving bits: no key, and not a divide-by-zero
		} else {
			phi *= float64(nSifted) / float64(nSurvivingEC)
		}
	}
	// 0.5, not 1: h2(1)=0 would make 1-h2(phi)=1 and certify the whole block as secret
	phi = min(phi, 0.5)
	return PhaseErrorEstimate{Mu: mu, Phi: phi, Conservative: conservative}
}

// FiniteKeySecureKeyLength: l = n*(1-h2(phi)) - leakedBits - log2(2/epsCor) - 2*log2(1/(2*epsPA)); leakedBits is
// error correction's disclosure only, since log2(2/epsCor) already covers the ceil(log2(1/epsCor))-bit tag
func FiniteKeySecureKeyLength(n int, phi float64, leakedBits int, epsCor, epsPA float64) int {
	if n <= 0 || epsCor <= 0 || epsCor >= 1 || epsPA <= 0 || epsPA >= 1 || !(phi < 0.5) {
		return 0 // phi>=0.5 bounds nothing; h2 falls again past 0.5, so it must not reach the formula
	}
	rate := 1 - binaryEntropy(phi)
	l := rate*float64(n) - float64(leakedBits) - math.Log2(2/epsCor) - 2*math.Log2(1/(2*epsPA))
	if l < 0 {
		return 0
	}
	li := int(math.Floor(l))
	if li > n {
		li = n
	}
	return li
}

// SecurityBudget is the composable security parameters for one PA block
type SecurityBudget struct {
	EpsPE  float64 // parameter-estimation confidence
	EpsPA  float64 // privacy-amplification (leftover hash) failure probability
	EpsCor float64 // correctness: probability the verification step accepts unequal strings
}

// SplitSecurityBudget: epsSec = 2*epsPE + epsPA; epsCor is tracked
// separately, since correctness and secrecy compose additively but distinctly
func SplitSecurityBudget(epsSec, epsCor float64) SecurityBudget {
	return SecurityBudget{EpsPE: epsSec / 3, EpsPA: epsSec / 3, EpsCor: epsCor}
}

// FiniteKeyConfig is the composable security target for one privacy-amplification block
type FiniteKeyConfig struct {
	EpsSec float64 // secrecy failure probability, default 1e-10
	EpsCor float64 // correctness failure probability, default 1e-15
	// byCorrection: every bit is key and errors are counted by correction, so each basis's key is bounded
	// with the other basis's rounds as its test and the two joined by the chain rule (doc/security_proof.md)
	byCorrection bool
}

func (c FiniteKeyConfig) withDefaults() FiniteKeyConfig {
	if c.EpsSec <= 0 {
		c.EpsSec = 1e-10
	}
	if c.EpsCor <= 0 {
		c.EpsCor = 1e-15
	}
	return c
}

// FiniteKeyReport records how one block's finite-key length was derived
type FiniteKeyReport struct {
	PhiPoint   float64 // cross-basis phase error point estimate from the sample
	Phi        float64 // bound fed to the key length: point + fluctuation, post-selection corrected
	TagBits    int     // universal-hash verification tag length, log2(1/epsCor)
	Bits       int     // surviving bits entering verification and hashing
	Verified   bool    // both sides' surviving strings hashed equal
	LeakedBits int     // all classical disclosure charged, tag included
	ChainBits  int     // the chain rule's cost when errors are counted by correction, 0 otherwise
	Length     int     // final key length in bits
}

// logChoose is ln C(n,k)
func logChoose(n, k int) float64 {
	a, _ := math.Lgamma(float64(n + 1))
	b, _ := math.Lgamma(float64(k + 1))
	c, _ := math.Lgamma(float64(n - k + 1))
	return a - b - c
}

// hypergeomCDF is P[X <= e] for X ~ Hypergeometric(population, successes, draws), summed
// in log space so large populations neither overflow nor underflow
func hypergeomCDF(e, population, successes, draws int) float64 {
	lo := max(0, draws-(population-successes))
	hi := min(e, successes, draws)
	if hi < lo {
		return 0
	}
	base := logChoose(population, draws)
	terms := make([]float64, 0, hi-lo+1)
	peak := math.Inf(-1)
	for x := lo; x <= hi; x++ {
		t := logChoose(successes, x) + logChoose(population-successes, draws-x) - base
		terms = append(terms, t)
		peak = math.Max(peak, t)
	}
	var s float64
	for _, t := range terms {
		s += math.Exp(t - peak)
	}
	return math.Min(1, math.Exp(peak)*s)
}

// estimationTail: the failure probability each basis's bound may have; Tomamichel et al. 2012 (S14) and
// Tomamichel-Leverrier 2017 (81) need the estimate to fail with probability epsPE^2, split over the two bases
func estimationTail(epsPE float64) float64 {
	return epsPE * epsPE / 2
}

// chainRuleBits: f(eps) = log2(2/(1 - sqrt(1 - eps^2))) of the smooth min-entropy chain rule (Vitanov et al.
// 2013, Theorem 13), with 1 - sqrt(1 - x) written as x/(1 + sqrt(1 - x)) to stay exact for tiny eps
func chainRuleBits(eps float64) int {
	e2 := eps * eps
	return int(math.Ceil(math.Log2(2 * (1 + math.Sqrt(1-e2)) / e2)))
}

// HypergeomUpperRate bounds the error rate of n unsampled bits from e errors seen in k bits
// drawn uniformly from the n+k: exact, so tighter than Serfling, failing with probability <= eps
func HypergeomUpperRate(n, k, e int, eps float64) float64 {
	if n <= 0 || k <= 0 || eps <= 0 || eps >= 1 {
		return 0.5
	}
	lo, hi := 0, n
	for lo < hi {
		m := (lo + hi + 1) / 2
		if hypergeomCDF(e, n+k, e+m, k) >= eps {
			lo = m
		} else {
			hi = m - 1
		}
	}
	return float64(lo) / float64(n)
}

// crossBasisPhaseError bounds the phase error of the kept bits from the other basis's sample errors, scaled
// for CRC drops; each basis's bound fails with probability tail
func crossBasisPhaseError(sample, rem timedBits, nSurviving int, tail float64) (phi, point float64) {
	var k, e, n [2]int
	for i, b := range sample.Basis {
		if b > 1 {
			continue
		}
		k[b]++
		if sample.Alice[i] != sample.Bob[i] {
			e[b]++
		}
	}
	for _, b := range rem.Basis {
		if b <= 1 {
			n[b]++
		}
	}
	total := n[0] + n[1]
	if total == 0 || nSurviving <= 0 {
		return 0.5, 0.5
	}
	for b := range 2 {
		if n[b] == 0 {
			continue
		}
		other := 1 - b
		if k[other] == 0 {
			return 0.5, 0.5 // nothing sampled in the conjugate basis bounds these key bits
		}
		q := float64(e[other]) / float64(k[other])
		w := float64(n[b]) / float64(total)
		point += w * q
		phi += w * min(0.5, HypergeomUpperRate(n[b], k[other], e[other], tail))
	}
	phi *= float64(total) / float64(nSurviving)
	return min(0.5, phi), point
}

// correctionSample is the sample error correction gives for free: every key bit with Bob's original value where
// its chunk verified, so its error is known exactly, and counted as an error where the CRC dropped its chunk
func correctionSample(rem timedBits, correctedBob []byte, chunkSize int) timedBits {
	ac, bc := ComputeChunkCRC(rem.Alice, chunkSize), ComputeChunkCRC(correctedBob, chunkSize)
	s := timedBits{Alice: rem.Alice, Bob: make([]byte, len(rem.Bob)), Dt: rem.Dt, Basis: rem.Basis}
	for i := range rem.Bob {
		if ac[i/chunkSize] == bc[i/chunkSize] {
			s.Bob[i] = rem.Bob[i]
		} else {
			s.Bob[i] = rem.Alice[i] ^ 1
		}
	}
	return s
}

// sampleEcEfficiency is the declared error-correction efficiency f (disclosure / Shannon
// limit) OptimalSampleFraction plans with; a planning constant, not a measurement
const sampleEcEfficiency = 1.2

// OptimalSampleFraction picks the sacrificed share of nSifted bits maximizing the predicted
// finite key at a declared expected QBER; every input is public, so the choice leaks nothing
func OptimalSampleFraction(nSifted int, expectedQBER float64, fk FiniteKeyConfig) float64 {
	fk = fk.withDefaults()
	budget := SplitSecurityBudget(fk.EpsSec, fk.EpsCor)
	best, bestLen := 0.1, 0
	for pct := 1; pct <= 60; pct++ {
		frac := float64(pct) / 100
		k := int(frac * float64(nSifted) / 2) // per basis
		nb := int((1 - frac) * float64(nSifted) / 2)
		if k < 1 || nb < 1 {
			continue
		}
		phi := HypergeomUpperRate(nb, k, int(math.Round(expectedQBER*float64(k))), estimationTail(budget.EpsPE))
		leak := int(sampleEcEfficiency * binaryEntropy(expectedQBER) * float64(2*nb))
		if l := FiniteKeySecureKeyLength(2*nb, phi, leak, budget.EpsCor, budget.EpsPA); l > bestLen {
			best, bestLen = frac, l
		}
	}
	return best
}

// distillFiniteKey is step 5 under a finite-key bound: verify correctness with a universal
// hash, then hash Alice's surviving bits to FiniteKeySecureKeyLength's length
func distillFiniteKey(alice, bob []byte, sample, rem timedBits, leaked int, fk FiniteKeyConfig, alignBits int) (key, seed []byte, rep FiniteKeyReport, err error) {
	fk = fk.withDefaults()
	budget := SplitSecurityBudget(fk.EpsSec, fk.EpsCor)
	n := len(alice)
	rep.Bits = n
	tail := estimationTail(budget.EpsPE)
	if fk.byCorrection {
		// a quarter of the smoothing for each basis's bound and one for the chain rule, epsPE in all
		s := budget.EpsPE / 4
		tail, rep.ChainBits = s*s, chainRuleBits(s)
	}
	rep.Phi, rep.PhiPoint = crossBasisPhaseError(sample, rem, n, tail)
	rep.TagBits = UniversalHashTagBits(budget.EpsCor)
	rep.LeakedBits = leaked + rep.TagBits
	if n == 0 {
		return nil, nil, rep, nil
	}

	tagSeed, err := UniversalHashSeed(rep.TagBits, n)
	if err != nil {
		return nil, nil, rep, err
	}
	rep.Verified = VerifyUniversalHash(bob, ComputeUniversalHashTag(alice, tagSeed, rep.TagBits), tagSeed, rep.TagBits)
	if !rep.Verified {
		return nil, nil, rep, nil // the strings differ: no key, rather than a key the sides disagree on
	}

	// the report counts every disclosed bit, the tag included; the formula's epsCor term is the tag's cost
	l := FiniteKeySecureKeyLength(n, rep.Phi, leaked+rep.ChainBits, budget.EpsCor, budget.EpsPA)
	if alignBits > 0 {
		l -= l % alignBits
	}
	rep.Length = l
	if l == 0 {
		return nil, nil, rep, nil
	}
	seed, err = toeplitzSeed(l, n)
	if err != nil {
		return nil, nil, rep, err
	}
	return toeplitzHashFast(alice, seed, l), seed, rep, nil
}

// distill is step 5 for the sample-based strategies: finite-key unless asymptotic is set
func distill(cv CRCVerifyResult, sample, rem timedBits, leaked int, qber float64, asymptotic bool, fk FiniteKeyConfig, alignBits int) (key, seed []byte, rep *FiniteKeyReport, err error) {
	if asymptotic {
		key, seed, err = PrivacyAmplify(cv.SurvivingAlice, qber, leaked, alignBits)
		return key, seed, nil, err
	}
	key, seed, r, err := distillFiniteKey(cv.SurvivingAlice, cv.SurvivingBob, sample, rem, leaked, fk, alignBits)
	return key, seed, &r, err
}
