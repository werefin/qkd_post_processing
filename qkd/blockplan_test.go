package qkd

import (
	"math"
	"testing"
	"time"
)

// TestPredictCorrectionKeyMatchesDistillation: the planner's prediction is the key distillation certifies on the
// same counts, so a plan can never promise more than the bound gives
func TestPredictCorrectionKeyMatchesDistillation(t *testing.T) {
	fk := FiniteKeyConfig{}.withDefaults()
	budget := SplitSecurityBudget(fk.EpsSec, fk.EpsCor)
	tail, chain := correctionBudget(budget.EpsPE)
	n := [2]int{45_000, 45_000}
	e := [2]int{2_700, 2_700}
	phi, _ := phaseErrorFromCounts(n, n, e, 90_000, tail)
	leak := int(math.Ceil(1.05 * binaryEntropy(0.06) * 90_000))
	want := FiniteKeySecureKeyLength(90_000, phi, leak+chain, budget.EpsCor, budget.EpsPA)
	if got := PredictCorrectionKey(90_000, 0.06, 1.05, fk); got != want || want == 0 {
		t.Fatalf("predicted %d bits, distillation gives %d", got, want)
	}
}

// TestPlanBlock: a clean link gets short blocks, a noisy one longer, a link that keeps failing shorter again,
// and every length is a whole number of segments within the limits
func TestPlanBlock(t *testing.T) {
	plan := func(q, fail float64) BlockPlan {
		start := time.Now()
		p := PlanBlock(BlockPlanInput{SiftedPerS: 300, QBER: q, FailPerS: fail, StepS: 5, MinS: 30, MaxS: 1800}, FiniteKeyConfig{})
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("a plan took %v", d)
		}
		t.Logf("QBER %.0f%%, %g failures/h: %g s at %.1f bit/s, best %g s at %.1f bit/s", 100*q, 3600*fail, p.BlockS, p.Rate, p.BestS, p.BestRate)
		if p.BlockS < 30 || p.BlockS > 1800 || int(p.BlockS)%5 != 0 || p.Rate < blockRateShare*p.BestRate {
			t.Fatalf("bad plan %+v", p)
		}
		return p
	}
	clean, noisy := plan(0.02, 0), plan(0.08, 0)
	if clean.BlockS >= noisy.BlockS {
		t.Fatalf("2%% QBER plans %g s, 8%% plans %g s", clean.BlockS, noisy.BlockS)
	}
	if flaky := plan(0.08, 2.0/3600); flaky.BlockS >= noisy.BlockS || flaky.BestS >= 1800 {
		t.Fatalf("two failures an hour still plan %+v", flaky)
	}
	if none := PlanBlock(BlockPlanInput{SiftedPerS: 1, QBER: 0.05, StepS: 10, MinS: 30, MaxS: 600}, FiniteKeyConfig{}); none.BlockS != 600 {
		t.Fatalf("a link giving no key should take the longest block, got %+v", none)
	}
}
