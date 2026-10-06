package qoj_post_processing

import "sort"

// WindowReportConfig controls ReportWindowQuality's bucketing
type WindowReportConfig struct {
	BucketPS int64 // time-bucket width, ps
	MinPairs int   // buckets with fewer sifted pairs than this report as insufficient data
}

// WindowBucketReport is one time-bucket's own measured QBER, for display only
type WindowBucketReport struct {
	StartPS    int64
	PairCount  int
	QBER       float64 // only meaningful when Reportable is true
	Reportable bool
}

// ReportWindowQuality buckets already-sifted pairs by elapsed time and
// reports each bucket's own QBER, so we can see where and when a link's quality drifts
func ReportWindowQuality(aliceEv, bobEv []DetectionEvent, pairs []MatchedPair, cfg WindowReportConfig) []WindowBucketReport {
	if len(pairs) == 0 {
		return nil
	}

	byBucket := map[int64][]int{}
	for i, p := range pairs {
		bucket := aliceEv[p.AliceIdx].TimestampPS / cfg.BucketPS
		byBucket[bucket] = append(byBucket[bucket], i)
	}

	buckets := make([]int64, 0, len(byBucket))
	for b := range byBucket {
		buckets = append(buckets, b)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i] < buckets[j] })

	reports := make([]WindowBucketReport, 0, len(buckets))
	for _, b := range buckets {
		idxs := byBucket[b]
		rep := WindowBucketReport{StartPS: b * cfg.BucketPS, PairCount: len(idxs)}

		if len(idxs) >= cfg.MinPairs {
			errBits := 0
			for _, i := range idxs {
				p := pairs[i]
				if aliceEv[p.AliceIdx].Bit != bobEv[p.BobIdx].Bit {
					errBits++
				}
			}
			rep.QBER = float64(errBits) / float64(len(idxs))
			rep.Reportable = true
		}
		reports = append(reports, rep)
	}

	return reports
}
