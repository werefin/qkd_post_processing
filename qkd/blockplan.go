// Block length of continuous QKD: each length's finite key is predicted with the bound the key is sized by, from
// what the link's earlier blocks made public, so the choice never depends on the block it sets
package qkd

import "math"

// BlockPlanInput is a link's recent public record and the lengths allowed
type BlockPlanInput struct {
	SiftedPerS   float64 `json:"sifted_per_s"`  // sifted pairs per second of data
	QBER         float64 `json:"qber"`          // counted by correction
	ECEfficiency float64 `json:"ec_efficiency"` // Cascade's disclosure over n*h2(QBER), 0 = plannedECEfficiency
	FailPerS     float64 `json:"fail_per_s"`    // failed blocks per second of blocks run
	StepS        float64 `json:"step_s"`        // a block is a whole number of these, the segment length
	MinS         float64 `json:"min_s"`
	MaxS         float64 `json:"max_s"`
}

// BlockPlan is the length chosen and why
type BlockPlan struct {
	BlockS   float64 `json:"block_s"`
	Rate     float64 `json:"rate"`      // expected key bits per second at BlockS
	BestS    float64 `json:"best_s"`    // the length with the highest expected rate
	BestRate float64 `json:"best_rate"` // and that rate
}

const (
	plannedECEfficiency = 1.07 // optimized Cascade across 3-11% QBER, until the link has measured its own
	blockRateShare      = 0.95 // the shortest block within 5% of the best rate: nearly all the key, far sooner
	blockPlanLengths    = 60   // lengths tried at most, so a plan takes well under a second
)

// PredictCorrectionKey is the finite key of nSifted pairs at qber with errors counted by correction and
// Cascade disclosing f*h2(qber) per bit: the bound distillation uses, on the counts the QBER implies
func PredictCorrectionKey(nSifted int, qber, f float64, fk FiniteKeyConfig) int {
	if nSifted < 2 || qber < 0 || qber >= 0.5 {
		return 0
	}
	fk = fk.withDefaults()
	budget := SplitSecurityBudget(fk.EpsSec, fk.EpsCor)
	tail, chain := correctionBudget(budget.EpsPE)
	n := [2]int{nSifted / 2, nSifted - nSifted/2}
	e := [2]int{int(math.Round(qber * float64(n[0]))), int(math.Round(qber * float64(n[1])))}
	phi, _ := phaseErrorFromCounts(n, n, e, nSifted, tail)
	leak := int(math.Ceil(f*binaryEntropy(qber)*float64(nSifted))) + chain
	return FiniteKeySecureKeyLength(nSifted, phi, leak, budget.EpsCor, budget.EpsPA)
}

// PlanBlock picks the next block's length: a block of L seconds is expected to give exp(-FailPerS*L) times its
// predicted key, since a failed block gives none, and the shortest L within blockRateShare of the best rate wins
func PlanBlock(in BlockPlanInput, fk FiniteKeyConfig) BlockPlan {
	step := in.StepS
	if step <= 0 {
		step = 1
	}
	f := in.ECEfficiency
	if f < 1 {
		f = plannedECEfficiency
	}
	lo, hi := math.Ceil(in.MinS/step), math.Floor(in.MaxS/step)
	lo = math.Max(lo, 1)
	if hi < lo {
		return BlockPlan{BlockS: lo * step}
	}
	stride := math.Ceil((hi - lo + 1) / blockPlanLengths)
	var lengths []float64
	for k := lo; k < hi; k += stride {
		lengths = append(lengths, k*step)
	}
	lengths = append(lengths, hi*step)
	rates := make([]float64, len(lengths))
	best := BlockPlan{BlockS: hi * step, BestS: hi * step}
	for i, s := range lengths {
		key := PredictCorrectionKey(int(in.SiftedPerS*s), in.QBER, f, fk)
		rates[i] = math.Exp(-math.Max(in.FailPerS, 0)*s) * float64(key) / s
		if rates[i] > best.BestRate {
			best.BestS, best.BestRate = s, rates[i]
		}
	}
	if best.BestRate <= 0 {
		return best // no length gives key: the longest gathers the most statistics
	}
	for i, s := range lengths {
		if rates[i] >= blockRateShare*best.BestRate {
			best.BlockS, best.Rate = s, rates[i]
			break
		}
	}
	return best
}
