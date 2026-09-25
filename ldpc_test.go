package qoj_post_processing

import (
	"bytes"
	"math/rand"
	"testing"
)

// TestBuildRegularLDPCColumnWeight checks every variable gets exactly wc
// edges, and every check gets at most wr (nominal, one short row per
// submatrix is expected)
func TestBuildRegularLDPCColumnWeight(t *testing.T) {
	h, err := BuildRegularLDPC(2000, 3, 10, 1)
	if err != nil {
		t.Fatalf("BuildRegularLDPC: %v", err)
	}
	for v := range h.N {
		if got := len(h.varEdges[v]); got != h.Wc {
			t.Fatalf("variable %d has degree %d, want %d", v, got, h.Wc)
		}
	}
	for c := range h.M {
		if got := len(h.CheckVars[c]); got > h.Wr {
			t.Fatalf("check %d has degree %d, exceeds nominal %d", c, got, h.Wr)
		}
	}
}

// TestBuildPEGLDPCColumnWeight checks PEG produces the same regular
// column/row weight guarantee as BuildRegularLDPC, and every check
// appears at most once per row (no duplicate edges from the BFS placement)
func TestBuildPEGLDPCColumnWeight(t *testing.T) {
	h, err := BuildPEGLDPC(2000, 3, 10, 1)
	if err != nil {
		t.Fatalf("BuildPEGLDPC: %v", err)
	}
	for v := range h.N {
		if got := len(h.varEdges[v]); got != h.Wc {
			t.Fatalf("variable %d has degree %d, want %d", v, got, h.Wc)
		}
	}
	for c := range h.M {
		row := h.CheckVars[c]
		if len(row) > h.Wr {
			t.Fatalf("check %d has degree %d, exceeds nominal %d", c, len(row), h.Wr)
		}
		seen := make(map[int32]bool, len(row))
		for _, v := range row {
			if seen[v] {
				t.Fatalf("check %d has a duplicate edge to variable %d", c, v)
			}
			seen[v] = true
		}
	}
}

// TestBuildPEGLDPCDeterministic checks the same seed reproduces the same
// matrix, both sides must derive it independently
func TestBuildPEGLDPCDeterministic(t *testing.T) {
	a, err := BuildPEGLDPC(500, 3, 9, 42)
	if err != nil {
		t.Fatalf("BuildPEGLDPC: %v", err)
	}
	b, err := BuildPEGLDPC(500, 3, 9, 42)
	if err != nil {
		t.Fatalf("BuildPEGLDPC: %v", err)
	}
	for c := range a.M {
		if !int32SliceEqual(a.CheckVars[c], b.CheckVars[c]) {
			t.Fatalf("check %d differs between two builds with the same seed", c)
		}
	}
}

// TestBuildPEGLDPCLowersShortCycles checks PEG's core claim: for the same
// (n, wc, wr), it produces no more 4-cycles than BuildRegularLDPC, usually
// far fewer, since it places edges to directly maximize local girth
func TestBuildPEGLDPCLowersShortCycles(t *testing.T) {
	countPairs := func(h *LDPCMatrix) int {
		pairCount := make(map[uint64]int)
		for c := range h.M {
			row := h.CheckVars[c]
			for i := range row {
				for j := i + 1; j < len(row); j++ {
					pairCount[pairKey(row[i], row[j])]++
				}
			}
		}
		conflicts := 0
		for _, cnt := range pairCount {
			if cnt > 1 {
				conflicts++
			}
		}
		return conflicts
	}

	reg, err := BuildRegularLDPC(10000, 3, 11, 7)
	if err != nil {
		t.Fatalf("BuildRegularLDPC: %v", err)
	}
	peg, err := BuildPEGLDPC(10000, 3, 11, 7)
	if err != nil {
		t.Fatalf("BuildPEGLDPC: %v", err)
	}
	regCycles, pegCycles := countPairs(reg), countPairs(peg)
	t.Logf("regular 4-cycle pairs=%d, PEG 4-cycle pairs=%d", regCycles, pegCycles)
	if pegCycles > regCycles {
		t.Fatalf("expected PEG to not exceed regular construction's 4-cycle count: %d > %d", pegCycles, regCycles)
	}
}

// TestBuildRegularLDPCDeterministic checks the same seed reproduces the
// same matrix, both sides must derive it independently
func TestBuildRegularLDPCDeterministic(t *testing.T) {
	a, err := BuildRegularLDPC(500, 3, 9, 42)
	if err != nil {
		t.Fatalf("BuildRegularLDPC: %v", err)
	}
	b, err := BuildRegularLDPC(500, 3, 9, 42)
	if err != nil {
		t.Fatalf("BuildRegularLDPC: %v", err)
	}
	for c := range a.M {
		if !int32SliceEqual(a.CheckVars[c], b.CheckVars[c]) {
			t.Fatalf("check %d differs between two builds with the same seed", c)
		}
	}
}

func int32SliceEqual(a, b []int32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestLDPCDecodeZeroErrorConverges checks a received block identical to
// the encoder's decodes back to itself immediately, converged
func TestLDPCDecodeZeroErrorConverges(t *testing.T) {
	h, err := BuildRegularLDPC(1000, 3, 9, 2)
	if err != nil {
		t.Fatalf("BuildRegularLDPC: %v", err)
	}
	r := rand.New(rand.NewSource(3))
	alice := make([]byte, h.N)
	for i := range alice {
		alice[i] = byte(r.Intn(2))
	}
	syndrome := h.Syndrome(alice)
	corrected, converged, iterations := LDPCDecode(h, alice, syndrome, 0.02, LDPCDecodeConfig{})
	if !converged {
		t.Fatal("expected an error-free block to converge")
	}
	if iterations != 1 {
		t.Fatalf("expected convergence on iteration 1, got %d", iterations)
	}
	if !bytes.Equal(corrected, alice) {
		t.Fatal("expected the decoded block to match the error-free input exactly")
	}
}

// TestLDPCDecodeCorrectsRandomErrors checks a code rate suited to a given
// QBER actually drives the error count to zero under belief propagation
func TestLDPCDecodeCorrectsRandomErrors(t *testing.T) {
	const n = 4000
	const qber = 0.03
	h, err := BuildRegularLDPC(n, 3, 12, 7) // rate 0.75, safely below 1-h2(0.03)=0.81
	if err != nil {
		t.Fatalf("BuildRegularLDPC: %v", err)
	}
	r := rand.New(rand.NewSource(11))
	alice := make([]byte, n)
	for i := range alice {
		alice[i] = byte(r.Intn(2))
	}
	bob := append([]byte(nil), alice...)
	initialErrors := 0
	for i := range bob {
		if r.Float64() < qber {
			bob[i] ^= 1
			initialErrors++
		}
	}

	res := LDPCPass(h, alice, bob, qber, LDPCDecodeConfig{})
	remainingErrors := 0
	for i := range res.CorrectedBob {
		if res.CorrectedBob[i] != alice[i] {
			remainingErrors++
		}
	}
	t.Logf("initial errors=%d, converged=%v, iterations=%d, remaining errors=%d, leaked=%d",
		initialErrors, res.Converged, res.Iterations, remainingErrors, res.LeakedBits)
	if !res.Converged {
		t.Fatal("expected belief propagation to converge at this rate/QBER")
	}
	if remainingErrors != 0 {
		t.Fatalf("expected a converged decode to be fully corrected, got %d residual errors", remainingErrors)
	}
}

// TestLDPCReconcileHandlesShortFinalBlock checks a total length that
// isn't a multiple of the block length still round-trips correctly
func TestLDPCReconcileHandlesShortFinalBlock(t *testing.T) {
	h, err := BuildRegularLDPC(1000, 3, 12, 4) // rate 0.75, safely below 1-h2(0.02)=0.859
	if err != nil {
		t.Fatalf("BuildRegularLDPC: %v", err)
	}
	r := rand.New(rand.NewSource(21))
	total := 1000*2 + 137 // a short final block
	alice := make([]byte, total)
	for i := range alice {
		alice[i] = byte(r.Intn(2))
	}
	bob := append([]byte(nil), alice...)
	for i := range bob {
		if r.Float64() < 0.02 {
			bob[i] ^= 1
		}
	}

	corrected, convergedBlocks, _, leaked := LDPCReconcile(h, alice, bob, 0.02, LDPCDecodeConfig{})
	if len(corrected) != total {
		t.Fatalf("expected %d corrected bits, got %d", total, len(corrected))
	}
	if convergedBlocks != 3 {
		t.Fatalf("expected all 3 blocks to converge, got %d", convergedBlocks)
	}
	if leaked != 3*h.M {
		t.Fatalf("expected %d leaked bits, got %d", 3*h.M, leaked)
	}
}

// TestRunLDPCEndToEnd checks the same invariants as TestRunEndToEnd and
// TestRunWinnowEndToEnd: the error-free stream really is error-free, and
// the key never exceeds the input length
func TestRunLDPCEndToEnd(t *testing.T) {
	alice, bob := simulateClicks(50000, 0.02, 55)
	cfg := LDPCRunConfig{
		CoincidenceWindowPS: 500,
		SampleFraction:      0.05,
		BlockLength:         40000,
		ColWeight:           3,
		RowWeight:           12, // rate 0.75, safely below 1-h2(0.02)=0.859
		Seed:                1,
		ChunkSize:           2048,
	}
	res, err := RunLDPC(alice, bob, cfg)
	if err != nil {
		t.Fatalf("RunLDPC returned error: %v", err)
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
	t.Logf("sifted=%d ldpc_blocks=%d converged=%d leaked=%d final=%d bits",
		res.SiftedBits, res.LDPCBlocks, res.LDPCConverged, res.LeakedBits, len(res.FinalKeyBits))
}

// TestRunLDPCVsWinnowVsBlockDiscard is informational, not a race, same
// spirit as TestRunWinnowVsBlockDiscard: it logs all three step 2/3
// methods' final key length on identical data. LDPC's edge is structural,
// one syndrome reveal per block regardless of QBER, versus Winnow's
// multi-pass whole-array re-reveal or discard's per-block parity reveal,
// so it should leak the least of the three at a QBER a single well-tuned
// rate can cover, and land the largest final key
func TestRunLDPCVsWinnowVsBlockDiscard(t *testing.T) {
	alice, bob := simulateClicks(50000, 0.02, 55)

	discardRes, err := Run(alice, bob, Config{
		CoincidenceWindowPS: 500,
		GeneratorMatrix:     GeneratorMatrixEx3,
		ChunkSize:           2048,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	winnowRes, err := RunWinnow(alice, bob, WinnowRunConfig{
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
		CoincidenceWindowPS: 500,
		SampleFraction:      0.05,
		ColWeight:           3,
		RowWeight:           12, // rate 0.75, safely below 1-h2(0.02)=0.859
		Seed:                1,
		ChunkSize:           2048,
	})
	if err != nil {
		t.Fatalf("RunLDPC returned error: %v", err)
	}

	t.Logf("discard(ex3): sifted=%d leaked=%d final=%d bits", discardRes.SiftedBits, discardRes.LeakedBits, len(discardRes.FinalKeyBits))
	t.Logf("winnow:       sifted=%d leaked=%d final=%d bits", winnowRes.SiftedBits, winnowRes.LeakedBits, len(winnowRes.FinalKeyBits))
	t.Logf("ldpc:         sifted=%d leaked=%d final=%d bits (blocks=%d converged=%d)",
		ldpcRes.SiftedBits, ldpcRes.LeakedBits, len(ldpcRes.FinalKeyBits), ldpcRes.LDPCBlocks, ldpcRes.LDPCConverged)
}
