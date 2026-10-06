package qoj_post_processing

import (
	"math/rand"
	"testing"
)

// buildBucket generates n matched, same-basis pairs inside one bucket's
// time range, with errRate fraction of them bit-mismatched
func buildBucket(r *rand.Rand, n int, startPS, bucketPS int64, errRate float64) (alice, bob []DetectionEvent, pairs []MatchedPair) {
	for i := 0; i < n; i++ {
		ts := startPS + int64(i)*(bucketPS/int64(n+1))
		bit := byte(r.Intn(2))
		otherBit := bit
		if r.Float64() < errRate {
			otherBit ^= 1
		}
		alice = append(alice, DetectionEvent{TimestampPS: ts, Basis: 0, Bit: bit})
		bob = append(bob, DetectionEvent{TimestampPS: ts, Basis: 0, Bit: otherBit})
		pairs = append(pairs, MatchedPair{AliceIdx: len(alice) - 1, BobIdx: len(bob) - 1})
	}
	return alice, bob, pairs
}

// TestReportWindowQualityReportsPerBucketQBER builds one clearly-good and
// one clearly-bad bucket and checks each is reported with its own true rate
func TestReportWindowQualityReportsPerBucketQBER(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	const bucketPS = 500_000_000_000 // 0.5s in ps

	goodAlice, goodBob, goodPairs := buildBucket(r, 300, 0, bucketPS, 0.02)
	badAlice, badBob, badPairs := buildBucket(r, 300, bucketPS, bucketPS, 0.50)

	var alice, bob []DetectionEvent
	var pairs []MatchedPair
	alice = append(alice, goodAlice...)
	alice = append(alice, badAlice...)
	bob = append(bob, goodBob...)
	bob = append(bob, badBob...)
	pairs = append(pairs, goodPairs...)
	for _, p := range badPairs {
		pairs = append(pairs, MatchedPair{AliceIdx: p.AliceIdx + len(goodAlice), BobIdx: p.BobIdx + len(goodBob)})
	}

	cfg := WindowReportConfig{BucketPS: bucketPS, MinPairs: 10}
	reports := ReportWindowQuality(alice, bob, pairs, cfg)

	if len(reports) != 2 {
		t.Fatalf("expected 2 buckets, got %d", len(reports))
	}
	if !reports[0].Reportable || reports[0].QBER > 0.10 {
		t.Fatalf("good bucket should report a QBER close to 0.02, got %+v", reports[0])
	}
	if !reports[1].Reportable || reports[1].QBER < 0.35 {
		t.Fatalf("bad bucket should report a QBER close to 0.50, got %+v", reports[1])
	}
}

// TestReportWindowQualityMinPairsFlagsSmallBuckets checks a bucket with
// fewer than MinPairs is marked unreportable rather than given a QBER
func TestReportWindowQualityMinPairsFlagsSmallBuckets(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	alice, bob, pairs := buildBucket(r, 3, 0, 500_000_000_000, 0.0)
	cfg := WindowReportConfig{BucketPS: 500_000_000_000, MinPairs: 10}
	reports := ReportWindowQuality(alice, bob, pairs, cfg)
	if len(reports) != 1 || reports[0].Reportable {
		t.Fatalf("a bucket below MinPairs must be marked unreportable, got %+v", reports)
	}
}

// TestReportWindowQualityEmptyInput checks the zero-pairs edge case
func TestReportWindowQualityEmptyInput(t *testing.T) {
	cfg := WindowReportConfig{BucketPS: 500_000_000_000, MinPairs: 10}
	reports := ReportWindowQuality(nil, nil, nil, cfg)
	if reports != nil {
		t.Fatalf("expected a nil report slice for empty input, got %+v", reports)
	}
}
