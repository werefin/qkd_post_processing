package qkd

import (
	"math"
	"math/rand"
	"testing"
)

// phaseErrorMu: mu = sqrt((ns+k)/(ns*k) * (k+1)/k * ln(1/epsPE)), the Serfling-style fluctuation term of
// Tomamichel et al.; a reference the exact hypergeometric bound must beat, not used to size keys
func phaseErrorMu(ns, k int, epsPE float64) float64 {
	nsF, kF := float64(ns), float64(k)
	return math.Sqrt((nsF + kF) / (nsF * kF) * (kF + 1) / kF * math.Log(1/epsPE))
}

// TestFiniteKeySecureKeyLengthMonotonicInN: more input bits at the same
// phi and leakage must never decrease the secure output length
func TestFiniteKeySecureKeyLengthMonotonicInN(t *testing.T) {
	phi, leaked, epsCor, epsPA := 0.03, 100, 1e-15, 1e-10/3
	prev := -1
	for _, n := range []int{1_000, 10_000, 100_000, 1_000_000, 10_000_000} {
		l := FiniteKeySecureKeyLength(n, phi, leaked, epsCor, epsPA)
		if l < prev {
			t.Fatalf("n=%d: key length %d is less than previous %d", n, l, prev)
		}
		prev = l
	}
	if prev <= 0 {
		t.Fatal("expected a nonzero key length at n=1e7")
	}
}

// TestFiniteKeySecureKeyLengthConvergesToAsymptotic: as n grows, the fixed per-block
// epsilon cost becomes negligible and the rate should approach 1-h2(phi), not 1-2h2
func TestFiniteKeySecureKeyLengthConvergesToAsymptotic(t *testing.T) {
	phi := 0.03
	n := 100_000_000
	leaked := int(float64(n) * (3.0/7 + 16.0/256)) // review's EC+EV leakage ratio
	l := FiniteKeySecureKeyLength(n, phi, leaked, 1e-15, 1e-10/3)

	asymptoticRate := 1 - binaryEntropy(phi) - (3.0/7 + 16.0/256)
	want := asymptoticRate * float64(n)
	gotRate := float64(l) / float64(n)
	wantRate := want / float64(n)
	if math.Abs(gotRate-wantRate) > 0.001 {
		t.Fatalf("at n=1e8 expected finite-key rate (%v) close to asymptotic rate (%v)", gotRate, wantRate)
	}
}

// TestFiniteKeySecureKeyLengthZeroBelowMinimumBlockSize: a block too small
// to pay the fixed epsilon cost must yield zero, not a negative-length panic
func TestFiniteKeySecureKeyLengthZeroBelowMinimumBlockSize(t *testing.T) {
	l := FiniteKeySecureKeyLength(100, 0.02, 10, 1e-15, 1e-10/3)
	if l != 0 {
		t.Fatalf("expected 0 for a tiny block (fixed epsilon cost should dominate), got %d", l)
	}
}

// TestFiniteKeySecureKeyLengthRejectsInvalidEpsilons
func TestFiniteKeySecureKeyLengthRejectsInvalidEpsilons(t *testing.T) {
	cases := []struct{ epsCor, epsPA float64 }{
		{0, 1e-10}, {1, 1e-10}, {1e-10, 0}, {1e-10, 1},
	}
	for _, c := range cases {
		if l := FiniteKeySecureKeyLength(1_000_000, 0.02, 0, c.epsCor, c.epsPA); l != 0 {
			t.Fatalf("epsCor=%v epsPA=%v: expected 0 for an invalid epsilon, got %d", c.epsCor, c.epsPA, l)
		}
	}
}

// TestSplitSecurityBudgetMatchesReviewConvention: epsSec = 2*epsPE + epsPA
func TestSplitSecurityBudgetMatchesReviewConvention(t *testing.T) {
	epsSec, epsCor := 1e-10, 1e-15
	b := SplitSecurityBudget(epsSec, epsCor)
	sum := 2*b.EpsPE + b.EpsPA
	if math.Abs(sum-epsSec) > 1e-25 {
		t.Fatalf("2*epsPE+epsPA = %v, want epsSec = %v", sum, epsSec)
	}
	if b.EpsCor != epsCor {
		t.Fatalf("EpsCor = %v, want %v", b.EpsCor, epsCor)
	}
}

// TestFiniteKeyRoughlyMatchesReviewLinkAB: rough reconstruction of the review's Table 6,
// link AB case; checks ballpark match to a published number, not exact reproduction
func TestFiniteKeyRoughlyMatchesReviewLinkAB(t *testing.T) {
	const qber = 0.0204
	const nSifPerMin = 6111.5 * 60
	const necOverNsif = 0.866
	nSifted := int(nSifPerMin / 2) // per basis, roughly half of sifted bits
	nSurvivingEC := int(float64(nSifted) * necOverNsif)
	epsPE := (1e-10) / 3

	// the review's conservative bound: the sample rate plus the fluctuation term, scaled by sifted over surviving
	phi := (qber + phaseErrorMu(nSifted/2, nSifted/2, epsPE)) * float64(nSifted) / float64(nSurvivingEC)
	leaked := int(float64(nSurvivingEC) * (3.0/7 + 16.0/256) / necOverNsif) // EC+EV cost relative to Nsif, review's convention
	l := FiniteKeySecureKeyLength(nSurvivingEC, phi, leaked, 1e-15, epsPE)

	// per basis rate, both bases combined, per second
	gotRatePerSec := float64(l) * 2 / 60
	wantRatePerSec := 1347.0 // review's Table 6, link AB, 1 min, conservative

	ratio := gotRatePerSec / wantRatePerSec
	if ratio < 0.3 || ratio > 3 {
		t.Fatalf("link AB 1-min conservative: got %.0f bit/s, review reports %.0f bit/s (ratio %.2f, expected same order of magnitude)",
			gotRatePerSec, wantRatePerSec, ratio)
	}
	t.Logf("link AB 1-min conservative: got %.0f bit/s vs review's %.0f bit/s (ratio %.2f)", gotRatePerSec, wantRatePerSec, ratio)
}

// TestFiniteKeyNoKeyAtOrAboveHalf is the regression test for the old clamp at 1: h2(1)=0
// made 1-h2(phi)=1, so phi=1 would have certified the whole block as secret
func TestFiniteKeyNoKeyAtOrAboveHalf(t *testing.T) {
	for _, phi := range []float64{0.5, 0.7, 1.0} {
		if l := FiniteKeySecureKeyLength(1_000_000, phi, 0, 1e-15, 1e-10/3); l != 0 {
			t.Fatalf("phi=%.1f gave %d key bits, want 0", phi, l)
		}
	}
}

// TestDistillFiniteKeyAbortsOnMismatch: if the two surviving strings differ, the universal-hash
// check must fail and no key be produced, rather than two keys that disagree
func TestDistillFiniteKeyAbortsOnMismatch(t *testing.T) {
	alice := make([]byte, 20000)
	bob := append([]byte(nil), alice...)
	bob[1234] ^= 1
	key, _, rep, err := distillFiniteKey(alice, bob, timedBits{}, timedBits{}, 0, FiniteKeyConfig{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verified || key != nil {
		t.Fatalf("mismatched strings must not verify or produce a key: verified=%v key=%d bits", rep.Verified, len(key))
	}
}

// TestRunCascadeFiniteKeyAtScale: at a realistic block size the finite-key bound must yield a key,
// smaller than the asymptotic figure, with the phase-error bound above its point estimate
func TestRunCascadeFiniteKeyAtScale(t *testing.T) {
	t.Parallel() // heavy and independent: runs beside the other heavy tests
	alice, bob := simulateClicks(1_200_000, 0.02, 21)
	cfg := CascadeRunConfig{CoincidenceWindowPS: 500, Seed: 22, ChunkSize: 128, ExpectedQBER: 0.02, SacrificeSample: true}
	finite, err := RunCascade(alice, bob, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Asymptotic = true
	asym, err := RunCascade(alice, bob, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := finite.FiniteKey
	t.Logf("sifted=%d QBER=%.4f phi point=%.4f bound=%.4f tag=%d verified=%v | finite key=%d bits, asymptotic=%d bits",
		finite.SiftedBits, finite.QBER.QBER, r.PhiPoint, r.Phi, r.TagBits, r.Verified, len(finite.FinalKeyBits), len(asym.FinalKeyBits))
	if !r.Verified || len(finite.FinalKeyBits) == 0 {
		t.Fatalf("expected a verified, nonzero finite key at this scale")
	}
	if len(finite.FinalKeyBits) >= len(asym.FinalKeyBits) || r.Phi <= r.PhiPoint {
		t.Fatalf("finite key must be below asymptotic and phi bound above its point estimate")
	}
}

// TestHypergeomCDFMatchesBruteForce: enumerate every k-subset of a small population and count
func TestHypergeomCDFMatchesBruteForce(t *testing.T) {
	const population, successes, draws = 12, 5, 6
	counts := make([]int, draws+1)
	total := 0
	var rec func(start, picked, hits int)
	rec = func(start, picked, hits int) {
		if picked == draws {
			counts[hits]++
			total++
			return
		}
		for i := start; i < population; i++ {
			h := hits
			if i < successes {
				h++
			}
			rec(i+1, picked+1, h)
		}
	}
	rec(0, 0, 0)
	cum := 0
	for e := 0; e <= draws; e++ {
		cum += counts[e]
		want := float64(cum) / float64(total)
		if got := hypergeomCDF(e, population, successes, draws); math.Abs(got-want) > 1e-12 {
			t.Fatalf("P[X<=%d] = %v, want %v", e, got, want)
		}
	}
}

// TestHypergeomUpperRateCoverage: over many random samples the true unsampled error rate may
// exceed the bound at most eps of the time, the property the finite-key proof relies on
func TestHypergeomUpperRateCoverage(t *testing.T) {
	const n, k, trueErrors, trials, eps = 2000, 300, 160, 4000, 0.05
	r := rand.New(rand.NewSource(31))
	pop := make([]bool, n+k)
	for i := range trueErrors {
		pop[i] = true
	}
	violations := 0
	for range trials {
		r.Shuffle(len(pop), func(i, j int) { pop[i], pop[j] = pop[j], pop[i] })
		e, rest := 0, 0
		for i, bad := range pop {
			if bad && i < k {
				e++
			} else if bad {
				rest++
			}
		}
		if float64(rest)/n > HypergeomUpperRate(n, k, e, eps) {
			violations++
		}
	}
	rate := float64(violations) / trials
	t.Logf("bound violated in %.4f of trials at eps=%.2f", rate, eps)
	if rate > eps {
		t.Fatalf("violation rate %.4f exceeds eps %.2f", rate, eps)
	}
}

// TestHypergeomUpperRateTighterThanSerfling at the sizes the real recordings produce
func TestHypergeomUpperRateTighterThanSerfling(t *testing.T) {
	const eps = 1e-10 / 6
	for _, c := range []struct{ n, k int }{{2000, 100}, {8000, 1800}, {100000, 10000}} {
		e := int(0.068 * float64(c.k))
		h := HypergeomUpperRate(c.n, c.k, e, eps)
		s := float64(e)/float64(c.k) + phaseErrorMu(c.n, c.k, eps)
		t.Logf("n=%d k=%d e=%d: hypergeometric %.4f, Serfling %.4f", c.n, c.k, e, h, s)
		if h > s {
			t.Fatalf("exact bound %.4f looser than Serfling %.4f", h, s)
		}
	}
}

// TestOptimalSampleFractionGrowsKey: the chosen fraction must do at least as well as 5%
func TestOptimalSampleFractionGrowsKey(t *testing.T) {
	fk := FiniteKeyConfig{}.withDefaults()
	budget := SplitSecurityBudget(fk.EpsSec, fk.EpsCor)
	predicted := func(n int, frac float64) int {
		k, nb := int(frac*float64(n)/2), int((1-frac)*float64(n)/2)
		phi := HypergeomUpperRate(nb, k, int(math.Round(0.068*float64(k))), budget.EpsPE/2)
		leak := int(sampleEcEfficiency*binaryEntropy(0.068)*float64(2*nb)) + 50
		return FiniteKeySecureKeyLength(2*nb, phi, leak, budget.EpsCor, budget.EpsPA)
	}
	for _, n := range []int{22500, 129000} {
		f := OptimalSampleFraction(n, 0.068, FiniteKeyConfig{})
		t.Logf("n=%d: optimal fraction %.2f -> %d bits, 5%% -> %d bits", n, f, predicted(n, f), predicted(n, 0.05))
		if predicted(n, f) < predicted(n, 0.05) {
			t.Fatalf("optimal fraction did worse than 5%%")
		}
	}
}

// TestVerificationTagChargedOnce: the formula's log2(2/epsCor) term is the tag's cost, so a block's key length is
// the formula on error correction's disclosure alone, while the report still counts the tag among disclosed bits
func TestVerificationTagChargedOnce(t *testing.T) {
	alice, bob := simulateClicks(300000, 0.05, 31)
	res, err := RunCascade(alice, bob, CascadeRunConfig{CoincidenceWindowPS: 500, Seed: 5, ChunkSize: 1024, ExpectedQBER: 0.05})
	if err != nil {
		t.Fatal(err)
	}
	fk := res.FiniteKey
	b := SplitSecurityBudget(1e-10, 1e-15)
	want := FiniteKeySecureKeyLength(len(res.CRCVerify.SurvivingAlice), fk.Phi, res.LeakedBits+fk.ChainBits, b.EpsCor, b.EpsPA)
	if fk.Length != want || fk.LeakedBits != res.LeakedBits+fk.TagBits || fk.TagBits > int(math.Log2(2/b.EpsCor))+1 {
		t.Fatalf("length %d, want %d; reported leak %d, tag %d", fk.Length, want, fk.LeakedBits, fk.TagBits)
	}
}

// TestEstimationFailsWithEpsSquared: the sampling bound may fail with probability epsPE^2, split over the
// bases (Tomamichel et al. 2012, S14), so it is wider than one failing with epsPE would be
func TestEstimationFailsWithEpsSquared(t *testing.T) {
	eps := 1e-10 / 3
	if got := estimationTail(eps); math.Abs(got-eps*eps/2) > 1e-30 {
		t.Fatalf("tail %g, want %g", got, eps*eps/2)
	}
	wide, narrow := HypergeomUpperRate(100_000, 20_000, 1_000, estimationTail(eps)), HypergeomUpperRate(100_000, 20_000, 1_000, eps/2)
	if wide <= narrow {
		t.Fatalf("bound at epsPE^2/2 %.5f should exceed the one at epsPE/2 %.5f", wide, narrow)
	}
}

// TestChainRuleBits: f(eps) = log2(2/(1 - sqrt(1 - eps^2))), about log2(4/eps^2), exact for tiny eps
func TestChainRuleBits(t *testing.T) {
	for _, eps := range []float64{1e-3, 1e-8, 1e-10 / 12} {
		want := math.Log2(4 / (eps * eps))
		if got := float64(chainRuleBits(eps)); got < want || got > want+1.01 {
			t.Fatalf("eps %g: %v bits, want about %.1f", eps, got, want)
		}
	}
}

// TestHypergeomCDFStopsEarlySafely: summing only the terms that matter never gives less than the full sum, the
// bound's safe side, and differs from it by nothing a double can show
func TestHypergeomCDFStopsEarlySafely(t *testing.T) {
	full := func(e, population, successes, draws int) float64 {
		base := logChoose(population, draws)
		var s float64
		for x := max(0, draws-(population-successes)); x <= min(e, successes, draws); x++ {
			s += math.Exp(logChoose(successes, x) + logChoose(population-successes, draws-x) - base)
		}
		return s
	}
	for _, c := range [][4]int{{1_000, 90_000, 6_000, 45_000}, {2_900, 90_000, 6_000, 45_000}, {3_100, 90_000, 6_000, 45_000},
		{40_000, 1_000_000, 90_000, 500_000}, {5, 2_000, 40, 1_000}, {3_000, 200_000, 3_500, 100_000}} {
		got, want := hypergeomCDF(c[0], c[1], c[2], c[3]), full(c[0], c[1], c[2], c[3])
		if got < want*(1-1e-12) || got > want*(1+1e-12)+1e-300 {
			t.Fatalf("P[X<=%d] of %v: %v, full sum %v", c[0], c, got, want)
		}
	}
}
