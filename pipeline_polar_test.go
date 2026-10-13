package qkd_post_processing

import "testing"

// TestRunPolarEndToEnd checks the same invariants as the other Run*EndToEnd tests:
// the error-free stream really is error-free, and the key never exceeds the input
func TestRunPolarEndToEnd(t *testing.T) {
	alice, bob := simulateClicks(50000, 0.02, 55)
	cfg := PolarRunConfig{
		Asymptotic:          true, // mechanics test, finite-key needs far larger blocks (finitekey_test.go)
		CoincidenceWindowPS: 500,
		SampleFraction:      0.05,
		BlockLength:         4096,
		RateMargin:          0.15, // comfortably below 1-h2(0.02)=0.859, see doc/finite_key_procedure.md 2.3
		ListSize:            16,
		Seed:                1,
		ChunkSize:           2048,
	}
	res, err := RunPolar(alice, bob, cfg)
	if err != nil {
		t.Fatalf("RunPolar returned error: %v", err)
	}
	if res.SiftedBits == 0 {
		t.Fatal("expected nonzero sifted bits")
	}
	for i := range res.CRCVerify.SurvivingAlice {
		if res.CRCVerify.SurvivingAlice[i] != res.CRCVerify.SurvivingBob[i] {
			t.Fatalf("error-free stream has a residual mismatch at %d", i)
		}
	}
	if len(res.FinalKeyBits) > len(res.CRCVerify.SurvivingAlice) {
		t.Fatalf("final key (%d bits) longer than error-free input (%d bits)",
			len(res.FinalKeyBits), len(res.CRCVerify.SurvivingAlice))
	}
	if len(res.FinalKeyBits) == 0 {
		t.Fatal("expected a nonzero final key at 2% QBER over 50000 attempts")
	}
	t.Logf("sifted=%d polar_blocks=%d bits_corrected=%d leaked=%d final=%d bits",
		res.SiftedBits, res.PolarBlocks, res.PolarBitsCorrected, res.LeakedBits, len(res.FinalKeyBits))
}

// TestRunPolarZeroSampleFraction mirrors TestRunWinnowZeroSampleFraction: with nothing
// sacrificed for QBER, the estimate is 0 regardless of the real error rate
func TestRunPolarZeroSampleFraction(t *testing.T) {
	alice, bob := simulateClicks(5000, 0.02, 9)
	cfg := PolarRunConfig{
		Asymptotic:          true, // mechanics test, finite-key needs far larger blocks (finitekey_test.go)
		CoincidenceWindowPS: 500,
		SampleFraction:      0,
		BlockLength:         1024,
		RateMargin:          0.15,
		ListSize:            8,
		Seed:                1,
		ChunkSize:           512,
	}
	res, err := RunPolar(alice, bob, cfg)
	if err != nil {
		t.Fatalf("RunPolar returned error: %v", err)
	}
	if res.QBER.SampleBits != 0 || res.QBER.QBER != 0 {
		t.Fatalf("expected a zero-bit, zero QBER sample, got %+v", res.QBER)
	}
}

// TestRunPolarAutoBlockLength checks BlockLength=0 picks the largest power of two
// that fits the remaining sifted key, rather than erroring
func TestRunPolarAutoBlockLength(t *testing.T) {
	alice, bob := simulateClicks(20000, 0.02, 12)
	cfg := PolarRunConfig{
		Asymptotic:          true, // mechanics test, finite-key needs far larger blocks (finitekey_test.go)
		CoincidenceWindowPS: 500,
		SampleFraction:      0.05,
		RateMargin:          0.15,
		ListSize:            8,
		Seed:                1,
		ChunkSize:           512,
	}
	res, err := RunPolar(alice, bob, cfg)
	if err != nil {
		t.Fatalf("RunPolar returned error: %v", err)
	}
	if res.PolarBlocks == 0 {
		t.Fatal("expected at least one polar block with an auto-selected block length")
	}
}

// TestRunPolarVsLDPCVsWinnowVsBlockDiscard is informational: it logs all four step 2/3
// methods' final key length on identical data
func TestRunPolarVsLDPCVsWinnowVsBlockDiscard(t *testing.T) {
	alice, bob := simulateClicks(50000, 0.02, 55)

	discardRes, err := Run(alice, bob, Config{
		CoincidenceWindowPS: 500,
		GeneratorMatrix:     testMatrixC,
		ChunkSize:           2048,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	winnowRes, err := RunWinnow(alice, bob, WinnowRunConfig{
		Asymptotic:          true, // mechanics test, finite-key needs far larger blocks (finitekey_test.go)
		CoincidenceWindowPS: 500,
		SampleFraction:      0.05,
		WinnowBlockSizes:    []int{25, 50, 100},
		Seed:                1,
		ChunkSize:           128,
	})
	if err != nil {
		t.Fatalf("RunWinnow returned error: %v", err)
	}
	ldpcRes, err := RunLDPC(alice, bob, LDPCRunConfig{
		Asymptotic:          true, // mechanics test, finite-key needs far larger blocks (finitekey_test.go)
		CoincidenceWindowPS: 500,
		SampleFraction:      0.05,
		ColWeight:           3,
		RowWeight:           12,
		Seed:                1,
		ChunkSize:           2048,
	})
	if err != nil {
		t.Fatalf("RunLDPC returned error: %v", err)
	}
	polarRes, err := RunPolar(alice, bob, PolarRunConfig{
		Asymptotic:          true, // mechanics test, finite-key needs far larger blocks (finitekey_test.go)
		CoincidenceWindowPS: 500,
		SampleFraction:      0.05,
		BlockLength:         4096,
		RateMargin:          0.15,
		ListSize:            16,
		Seed:                1,
		ChunkSize:           2048,
	})
	if err != nil {
		t.Fatalf("RunPolar returned error: %v", err)
	}

	t.Logf("discard(baseline): sifted=%d leaked=%d final=%d bits", discardRes.SiftedBits, discardRes.LeakedBits, len(discardRes.FinalKeyBits))
	t.Logf("winnow:       sifted=%d leaked=%d final=%d bits", winnowRes.SiftedBits, winnowRes.LeakedBits, len(winnowRes.FinalKeyBits))
	t.Logf("ldpc:         sifted=%d leaked=%d final=%d bits (blocks=%d converged=%d)",
		ldpcRes.SiftedBits, ldpcRes.LeakedBits, len(ldpcRes.FinalKeyBits), ldpcRes.LDPCBlocks, ldpcRes.LDPCConverged)
	t.Logf("polar:        sifted=%d leaked=%d final=%d bits (blocks=%d corrected=%d)",
		polarRes.SiftedBits, polarRes.LeakedBits, len(polarRes.FinalKeyBits), polarRes.PolarBlocks, polarRes.PolarBitsCorrected)
}
