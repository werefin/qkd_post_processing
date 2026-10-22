package qkd

import (
	"bytes"
	"errors"
	"math"
	"strings"
	"testing"
)

// runBoth runs Alice and Bob concurrently over an in-process pipe
func runBoth(alice, bob []DetectionEvent, ca, cb TwoPartyConfig) (ra, rb TwoPartyResult, ea, eb error) {
	connA, connB := PipeConns()
	done := make(chan struct{})
	go func() {
		ra, ea = RunAlice(connA, alice, ca)
		close(done)
	}()
	rb, eb = RunBob(connB, StripBit(alice), bob, cb)
	<-done
	return
}

func twoPartyCfg() TwoPartyConfig {
	return TwoPartyConfig{WindowPS: 500, RateMargin: 0.02, ListSize: 1, ChunkSize: 1024, ExpectedQBER: 0.02, SampleSeed: 7, KeyAlignBits: 256}
}

// TestTwoPartyMatchesJoint: on the same data and sample, the two-party protocol certifies the
// same key length as the joint pipeline, and the two ends hold the same key under the same IDs
func TestTwoPartyMatchesJoint(t *testing.T) {
	t.Parallel() // heavy and independent: runs beside the other heavy tests
	alice, bob := simulateClicks(600000, 0.02, 3)
	cfg := twoPartyCfg()
	joint, err := RunPolar(alice, bob, PolarRunConfig{CoincidenceWindowPS: 500, RateMargin: 0.02, ListSize: 1,
		ChunkSize: 1024, ExpectedQBER: 0.02, Seed: 7, KeyAlignBits: 256})
	if err != nil {
		t.Fatal(err)
	}
	ra, rb, ea, eb := runBoth(alice, bob, cfg, cfg)
	if ea != nil || eb != nil {
		t.Fatalf("alice %v, bob %v", ea, eb)
	}
	if len(ra.FinalKeyBits) == 0 || !bytes.Equal(ra.FinalKeyBytes, rb.FinalKeyBytes) {
		t.Fatalf("keys: alice %d bits, bob %d bits, equal %v", len(ra.FinalKeyBits), len(rb.FinalKeyBits), bytes.Equal(ra.FinalKeyBytes, rb.FinalKeyBytes))
	}
	if len(ra.KeyIDs) != len(ra.FinalKeyBits)/256 || strings.Join(ra.KeyIDs, ",") != strings.Join(rb.KeyIDs, ",") {
		t.Fatalf("key IDs: %d vs %d", len(ra.KeyIDs), len(rb.KeyIDs))
	}
	if len(ra.FinalKeyBits) != len(joint.FinalKeyBits) || ra.LeakedBits != joint.LeakedBits || ra.SiftedBits != joint.SiftedBits {
		t.Fatalf("two-party %d key bits, %d leaked, %d sifted; joint %d, %d, %d",
			len(ra.FinalKeyBits), ra.LeakedBits, ra.SiftedBits, len(joint.FinalKeyBits), joint.LeakedBits, joint.SiftedBits)
	}
	if rb.PolarBitsCorrected == 0 || ra.QBER != rb.QBER {
		t.Fatalf("bob corrected %d, QBER alice %+v bob %+v", rb.PolarBitsCorrected, ra.QBER, rb.QBER)
	}
	t.Logf("%d sifted, QBER %.4f, %d bits disclosed, %d-bit key", ra.SiftedBits, ra.QBER.QBER, ra.LeakedBits, len(ra.FinalKeyBits))
}

// TestTwoPartyTimingAwareBob: Bob's timing-aware decoding changes nothing Alice must know and
// still yields the same key at both ends
func TestTwoPartyTimingAwareBob(t *testing.T) {
	t.Parallel() // heavy and independent: runs beside the other heavy tests
	alice, bob := simulateClicks(300000, 0.03, 9)
	cfg := twoPartyCfg()
	cfg.TimingBins = 8
	ra, rb, ea, eb := runBoth(alice, bob, cfg, cfg)
	if ea != nil || eb != nil || !bytes.Equal(ra.FinalKeyBytes, rb.FinalKeyBytes) || len(ra.FinalKeyBits) == 0 {
		t.Fatalf("alice %v, bob %v, %d vs %d bits", ea, eb, len(ra.FinalKeyBits), len(rb.FinalKeyBits))
	}
}

// TestTwoPartyRefusesMismatches: different parameters, or announced events that differ from
// Alice's own, stop both sides with an error instead of producing a key
func TestTwoPartyRefusesMismatches(t *testing.T) {
	alice, bob := simulateClicks(20000, 0.02, 4)
	other := twoPartyCfg()
	other.RateMargin = 0.05
	_, _, ea, eb := runBoth(alice, bob, twoPartyCfg(), other)
	if eb == nil || !errors.Is(ea, ErrPeerAborted) {
		t.Fatalf("parameter mismatch: alice %v, bob %v", ea, eb)
	}

	connA, connB := PipeConns()
	done := make(chan error)
	go func() {
		_, err := RunAlice(connA, alice, twoPartyCfg())
		done <- err
	}()
	tampered := StripBit(alice)
	tampered[5].Basis ^= 1
	_, eb = RunBob(connB, tampered, bob, twoPartyCfg())
	if ea := <-done; eb == nil || !errors.Is(ea, ErrPeerAborted) {
		t.Fatalf("tampered announcement: alice %v, bob %v", ea, eb)
	}
}

// TestTwoPartyTooLittleData: a short run ends on both sides with no key and no deadlock
func TestTwoPartyTooLittleData(t *testing.T) {
	alice, bob := simulateClicks(5000, 0.02, 5)
	ra, rb, ea, eb := runBoth(alice, bob, twoPartyCfg(), twoPartyCfg())
	if ea != nil || eb != nil || len(ra.FinalKeyBits) != 0 || len(rb.FinalKeyBits) != 0 {
		t.Fatalf("alice %v %d bits, bob %v %d bits", ea, len(ra.FinalKeyBits), eb, len(rb.FinalKeyBits))
	}
}

// TestTwoPartyAliceRejectsBadPairs: Bob cannot make Alice pair one event twice
func TestTwoPartyAliceRejectsBadPairs(t *testing.T) {
	alice, _ := simulateClicks(1000, 0.02, 6)
	connA, connB := PipeConns()
	done := make(chan error)
	go func() {
		_, err := RunAlice(connA, alice, twoPartyCfg())
		done <- err
	}()
	var h tpHello
	if err := connB.Recv("hello", &h); err != nil {
		t.Fatal(err)
	}
	connB.Send("sift", tpSift{AliceIdx: []int32{3, 3}, SampleSeed: 1})
	if err := <-done; err == nil || !strings.Contains(err.Error(), "paired twice") {
		t.Fatalf("got %v", err)
	}
}

// TestTwoPartyLDPC5G: with 5G NR LDPC both sides hold the same key under the same IDs, agree on
// every disclosed bit, land within 3% of the joint run, and beat two-party polar on the same data
func TestTwoPartyLDPC5G(t *testing.T) {
	t.Parallel() // heavy and independent: runs beside the other heavy tests
	b := ecBackend(t)
	alice, bob := simulateClicks(300000, 0.05, 3)
	cfg := twoPartyCfg()
	cfg.ExpectedQBER = 0.05
	cfg.Method, cfg.LDPC = "ldpc5g", b
	ra, rb, ea, eb := runBoth(alice, bob, cfg, cfg)
	if ea != nil || eb != nil {
		t.Fatalf("alice %v, bob %v", ea, eb)
	}
	if len(ra.FinalKeyBits) == 0 || !bytes.Equal(ra.FinalKeyBytes, rb.FinalKeyBytes) || strings.Join(ra.KeyIDs, ",") != strings.Join(rb.KeyIDs, ",") {
		t.Fatalf("keys differ: %d vs %d bits", len(ra.FinalKeyBits), len(rb.FinalKeyBits))
	}
	if ra.LeakedBits != rb.LeakedBits || ra.LDPC.Attempts != rb.LDPC.Attempts || rb.LDPC.Failed != 0 {
		t.Fatalf("alice %+v, bob %+v", ra.LDPC, rb.LDPC)
	}
	joint, err := RunLDPC5G(alice, bob, LDPC5GRunConfig{Backend: b, CoincidenceWindowPS: 500, Seed: 7, ChunkSize: 1024, ExpectedQBER: 0.05, KeyAlignBits: 256})
	if err != nil {
		t.Fatal(err)
	}
	pcfg := twoPartyCfg()
	pcfg.ExpectedQBER, pcfg.ListSize = 0.05, 8
	pa, _, _, _ := runBoth(alice, bob, pcfg, pcfg)
	t.Logf("two-party LDPC %d bits (%d disclosed), joint LDPC %d bits, two-party polar %d bits (%d disclosed)",
		len(ra.FinalKeyBits), ra.LeakedBits, len(joint.FinalKeyBits), len(pa.FinalKeyBits), pa.LeakedBits)
	if d := math.Abs(float64(len(ra.FinalKeyBits)-len(joint.FinalKeyBits))) / float64(len(joint.FinalKeyBits)); d > 0.03 || len(ra.FinalKeyBits) <= len(pa.FinalKeyBits) {
		t.Fatalf("two-party LDPC off the joint run by %.1f%% or not above polar", 100*d)
	}
}

// TestTwoPartyCascade: Cascade with Bob asking Alice for parities across the connection gives both ends the
// same key under the same IDs, as long as the one-process pipeline's on the same data, the chain rule charged
// when errors are counted by correction and not when a sample is sacrificed
func TestTwoPartyCascade(t *testing.T) {
	t.Parallel() // heavy and independent: runs beside the other heavy tests
	alice, bob := simulateClicks(300000, 0.05, 23)
	for _, sample := range []bool{false, true} {
		cfg := TwoPartyConfig{WindowPS: 500, ChunkSize: 1024, ExpectedQBER: 0.05, SampleSeed: 5, KeyAlignBits: 256,
			Method: "cascade", CascadeSample: sample}
		joint, err := RunCascade(alice, bob, CascadeRunConfig{CoincidenceWindowPS: 500, Seed: 5, ChunkSize: 1024,
			ExpectedQBER: 0.05, KeyAlignBits: 256, SacrificeSample: sample})
		if err != nil {
			t.Fatal(err)
		}
		ra, rb, ea, eb := runBoth(alice, bob, cfg, cfg)
		if ea != nil || eb != nil {
			t.Fatalf("sample %v: alice %v, bob %v", sample, ea, eb)
		}
		t.Logf("sample %v: %d-bit key, %d parities in %d round trips, phi %.4f, chain rule %d bits; one process %d bits",
			sample, len(ra.FinalKeyBits), rb.Cascade.Parities, rb.Cascade.Rounds, ra.FiniteKey.Phi, ra.FiniteKey.ChainBits, len(joint.FinalKeyBits))
		if len(ra.FinalKeyBits) == 0 || !bytes.Equal(ra.FinalKeyBytes, rb.FinalKeyBytes) || strings.Join(ra.KeyIDs, ",") != strings.Join(rb.KeyIDs, ",") {
			t.Fatalf("sample %v: keys of %d and %d bits, equal %v", sample, len(ra.FinalKeyBits), len(rb.FinalKeyBits), bytes.Equal(ra.FinalKeyBytes, rb.FinalKeyBytes))
		}
		if ra.ECLeakedBits != rb.Cascade.Parities || len(ra.FinalKeyBits) != len(joint.FinalKeyBits) || ra.FiniteKey.Phi != joint.FiniteKey.Phi {
			t.Fatalf("sample %v: Alice charged %d, Bob %d; key %d vs %d bits, phi %.5f vs %.5f", sample, ra.ECLeakedBits,
				rb.Cascade.Parities, len(ra.FinalKeyBits), len(joint.FinalKeyBits), ra.FiniteKey.Phi, joint.FiniteKey.Phi)
		}
		if (ra.FiniteKey.ChainBits > 0) == sample {
			t.Fatalf("sample %v: chain rule charged %d bits", sample, ra.FiniteKey.ChainBits)
		}
	}
}

// TestTwoPartyReducedAnnouncement: Bob holding only the announced clicks that can pair, with their indices in
// the whole announcement, and Alice holding only her bits and flags, exchange exactly the messages of the whole
// run and end with the same key
func TestTwoPartyReducedAnnouncement(t *testing.T) {
	alice, bob := simulateClicks(200000, 0.03, 12)
	// clicks of Alice's with no partner of Bob's, as most are at MHz rates
	end := alice[len(alice)-1].TimestampPS
	for k := range 100000 {
		alice = append(alice, DetectionEvent{TimestampPS: end + int64(k+1)*1_000_000, Basis: Basis(k % 2), Bit: byte(k / 2 % 2)})
	}
	cfg := twoPartyCfg()
	cfg.Method = "cascade"
	full, _, ea, eb := runBoth(alice, bob, cfg, cfg)
	if ea != nil || eb != nil {
		t.Fatalf("whole run: alice %v, bob %v", ea, eb)
	}
	// Bob keeps the announced clicks with one of his within 2 windows, as the stream does
	pub := StripBit(alice)
	near := make([]bool, len(pub))
	j := 0
	for i, a := range pub {
		for j < len(bob) && bob[j].TimestampPS < a.TimestampPS-2*cfg.WindowPS {
			j++
		}
		near[i] = j < len(bob) && bob[j].TimestampPS <= a.TimestampPS+2*cfg.WindowPS
	}
	var reduced []PublicEvent
	var index []int32
	for i, ok := range near {
		if ok {
			reduced, index = append(reduced, pub[i]), append(index, int32(i))
		}
	}
	if len(reduced) != 200000 {
		t.Fatalf("kept %d of %d announced clicks", len(reduced), len(pub))
	}
	cb := cfg
	cb.AnnouncementIndex, cb.AnnouncementEvents, cb.AnnouncementDigest = index, len(pub), PublicDigest(pub)
	connA, connB := PipeConns()
	done := make(chan struct{})
	var ra TwoPartyResult
	go func() {
		ra, ea = RunAliceClicks(connA, eventClicks(alice), PublicDigest(pub), cfg)
		close(done)
	}()
	rb, eb := RunBob(connB, reduced, bob, cb)
	<-done
	if ea != nil || eb != nil {
		t.Fatalf("reduced run: alice %v, bob %v", ea, eb)
	}
	// the privacy-amplification seed is fresh each run: the same length, disclosure and pairs, not the same bytes
	if len(ra.FinalKeyBits) == 0 || !bytes.Equal(ra.FinalKeyBytes, rb.FinalKeyBytes) || len(ra.FinalKeyBits) != len(full.FinalKeyBits) ||
		ra.SiftedBits != full.SiftedBits || ra.LeakedBits != full.LeakedBits || ra.QBER != full.QBER {
		t.Fatalf("reduced %d sifted, %d leaked, %d key bits; whole %d, %d, %d", ra.SiftedBits, ra.LeakedBits,
			len(ra.FinalKeyBits), full.SiftedBits, full.LeakedBits, len(full.FinalKeyBits))
	}
	// a reduced announcement without the whole one's digest is refused
	cb.AnnouncementDigest = nil
	connA, connB = PipeConns()
	go RunAliceClicks(connA, eventClicks(alice), PublicDigest(pub), cfg)
	if _, err := RunBob(connB, reduced, bob, cb); err == nil {
		t.Fatal("a reduced announcement without the whole digest was accepted")
	}
}
