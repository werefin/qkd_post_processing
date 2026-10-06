package qoj_post_processing

import "sort"

// EpochResult is one time-epoch's own, independently-run protocol outcome
type EpochResult struct {
	StartPS int64
	Result
}

// EpochedResult bundles every epoch's own outcome plus the concatenated key
type EpochedResult struct {
	Epochs        []EpochResult
	FinalKeyBits  []byte
	FinalKeyBytes []byte
}

// RunEpoched repeats Run() independently once per fixed-width time epoch,
// instead of pooling an entire recording into a single QBER estimate
// same way a continuously-operating QKD link already runs many short
// acquisition cycles rather than one long one
func RunEpoched(aliceEvents, bobEvents []DetectionEvent, cfg Config, epochPS int64) (EpochedResult, error) {
	var out EpochedResult

	aliceByEpoch := map[int64][]DetectionEvent{}
	for _, e := range aliceEvents {
		b := e.TimestampPS / epochPS
		aliceByEpoch[b] = append(aliceByEpoch[b], e)
	}
	bobByEpoch := map[int64][]DetectionEvent{}
	for _, e := range bobEvents {
		b := e.TimestampPS / epochPS
		bobByEpoch[b] = append(bobByEpoch[b], e)
	}

	seen := map[int64]bool{}
	epochs := make([]int64, 0, len(aliceByEpoch))
	for _, m := range []map[int64][]DetectionEvent{aliceByEpoch, bobByEpoch} {
		for b := range m {
			if !seen[b] {
				seen[b] = true
				epochs = append(epochs, b)
			}
		}
	}
	sort.Slice(epochs, func(i, j int) bool { return epochs[i] < epochs[j] })

	epochCfg := cfg
	epochCfg.KeyAlignBits = 0 // alignment applies once, to the concatenated total, below

	out.Epochs = make([]EpochResult, 0, len(epochs))
	for _, b := range epochs {
		res, err := Run(aliceByEpoch[b], bobByEpoch[b], epochCfg)
		if err != nil {
			return out, err
		}
		out.Epochs = append(out.Epochs, EpochResult{StartPS: b * epochPS, Result: res})
		out.FinalKeyBits = append(out.FinalKeyBits, res.FinalKeyBits...)
	}

	if cfg.KeyAlignBits > 0 {
		out.FinalKeyBits = out.FinalKeyBits[:len(out.FinalKeyBits)-len(out.FinalKeyBits)%cfg.KeyAlignBits]
	}
	out.FinalKeyBytes = packToBytes(out.FinalKeyBits)
	return out, nil
}
