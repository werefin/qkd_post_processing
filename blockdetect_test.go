package qoj_post_processing

import (
	"bytes"
	"math/rand"
	"testing"
)

// testMatrixA/B/C: small fixed matrices for exercising generic
// GeneratorMatrix behavior across this test suite, independent of any
// matrix the library exposes publicly (m=4/m=4/m=6, p=3 each)
var (
	// testMatrixA: code rate 4/7, detects 100% of 1/2-bit and 75% of
	// 3-bit errors, 100% of 4-bit errors
	testMatrixA = GeneratorMatrix{
		{1, 0, 1, 0},
		{1, 1, 0, 1},
		{0, 0, 1, 1},
	}
	// testMatrixB: same shape as A, trades 4-bit detection (0%) for 100%
	// 3-bit detection
	testMatrixB = GeneratorMatrix{
		{1, 1, 1, 1},
		{1, 1, 1, 0},
		{0, 1, 1, 0},
	}
	// testMatrixC: code rate 2/3, 88.9% of all bit errors detected overall
	testMatrixC = GeneratorMatrix{
		{0, 0, 0, 1, 1, 1},
		{1, 1, 0, 1, 0, 0},
		{1, 0, 1, 1, 1, 0},
	}
)

// TestParityKnownVectors checks parityInto against a table of known
// M --> P pairs for testMatrixC, computed independently by hand
func TestParityKnownVectors(t *testing.T) {
	cases := []struct {
		m []byte
		p []byte
	}{
		{[]byte{0, 0, 0, 0, 0, 0}, []byte{0, 0, 0}},
		{[]byte{0, 0, 0, 0, 0, 1}, []byte{1, 0, 0}},
		{[]byte{0, 0, 0, 0, 1, 0}, []byte{1, 0, 1}},
		{[]byte{0, 0, 0, 0, 1, 1}, []byte{0, 0, 1}},
		{[]byte{1, 1, 1, 1, 0, 0}, []byte{1, 1, 1}},
		{[]byte{1, 1, 1, 1, 0, 1}, []byte{0, 1, 1}},
		{[]byte{1, 1, 1, 1, 1, 0}, []byte{0, 1, 0}},
		{[]byte{1, 1, 1, 1, 1, 1}, []byte{1, 1, 0}},
	}
	masks := buildParityMasks(testMatrixC)
	words := make([]uint64, masks.words)
	parity := make([]byte, testMatrixC.ParityBits())
	for _, c := range cases {
		got := masks.parityInto(c.m, words, parity)
		if !parityEqual(got, c.p) {
			t.Fatalf("M=%v: expected P=%v, got %v", c.m, c.p, got)
		}
	}
}

// TestParityCollision checks a known collision under testMatrixA: two
// different blocks that happen to give identical parity
func TestParityCollision(t *testing.T) {
	masks := buildParityMasks(testMatrixA)
	words := make([]uint64, masks.words)
	p1 := append([]byte(nil), masks.parityInto([]byte{0, 0, 0, 0}, words, make([]byte, 3))...)
	p2 := masks.parityInto([]byte{1, 0, 1, 1}, words, make([]byte, 3))
	want := []byte{0, 0, 0}
	if !parityEqual(p1, want) || !parityEqual(p2, want) {
		t.Fatalf("expected M=(0,0,0,0) and M=(1,0,1,1) to both give P=%v under testMatrixA, got %v and %v", want, p1, p2)
	}
}

// TestCascadeCatchesCollision: A and B collide under testMatrixC alone
// (same parity despite differing by 3 bits), but a small G2 tells them apart
func TestCascadeCatchesCollision(t *testing.T) {
	a := []byte{0, 0, 0, 0, 0, 0}
	b := []byte{0, 0, 1, 0, 1, 1}

	masks := buildParityMasks(testMatrixC)
	words := make([]uint64, masks.words)
	pa := append([]byte(nil), masks.parityInto(a, words, make([]byte, 3))...)
	pb := masks.parityInto(b, words, make([]byte, 3))
	if !parityEqual(pa, pb) {
		t.Fatalf("expected A and B to collide under testMatrixC alone, got %v and %v", pa, pb)
	}

	g2 := GeneratorMatrix{
		{1, 0, 0, 0, 0, 0},
		{0, 0, 1, 0, 0, 0},
	}
	cascaded, err := CascadeGeneratorMatrices(testMatrixC, g2)
	if err != nil {
		t.Fatalf("CascadeGeneratorMatrices: %v", err)
	}

	res := BlockErrorDetect(a, b, cascaded)
	if res.BlocksKept != 0 || res.BlocksDropped != 1 {
		t.Fatalf("expected the cascade to catch the collision and drop the block, got kept=%d dropped=%d", res.BlocksKept, res.BlocksDropped)
	}
}

func TestCascadeGeneratorMatricesRejectsMismatchedBlockSize(t *testing.T) {
	_, err := CascadeGeneratorMatrices(testMatrixA, testMatrixC)
	if err == nil {
		t.Fatal("expected an error for mismatched block sizes (m=4 vs m=6)")
	}
}

func TestCascadeGeneratorMatricesRejectsNoRemainingSecret(t *testing.T) {
	_, err := CascadeGeneratorMatrices(testMatrixA, testMatrixB)
	if err == nil {
		t.Fatal("expected an error: testMatrixA+testMatrixB leak 6 parity bits from a 4-bit block")
	}
}

// TestParityTableMatchesPopcountReference: the lookup-table fast path
// must agree bit-for-bit with the POPCNT reference for every block value
func TestParityTableMatchesPopcountReference(t *testing.T) {
	for _, g := range []GeneratorMatrix{testMatrixA, testMatrixB, testMatrixC} {
		m := g.BlockSize()
		table, ok := buildParityTable(g)
		if !ok {
			t.Fatalf("expected a table for m=%d", m)
		}
		masks := buildParityMasks(g)
		words := make([]uint64, masks.words)
		scratch := make([]byte, g.ParityBits())
		for idx := range 1 << uint(m) {
			block := make([]byte, m)
			for j := range block {
				block[j] = byte(idx>>uint(j)) & 1
			}
			want := append([]byte(nil), masks.parityInto(block, words, scratch)...)

			pr := table.entries[idx]
			got := make([]byte, g.ParityBits())
			for i := range got {
				got[i] = byte(pr>>uint(i)) & 1
			}

			if !bytes.Equal(want, got) {
				t.Fatalf("matrix m=%d, block index %d (%v): popcount=%v, table=%v", m, idx, block, want, got)
			}
		}
	}
}

// TestBuildParityTableRejectsOversizedMatrix confirms ComputeBlockParity
// still falls back to the POPCNT path when m exceeds the table limit
func TestBuildParityTableRejectsOversizedMatrix(t *testing.T) {
	row := make([]byte, maxTableBits+1)
	row[0] = 1
	big := GeneratorMatrix{row}
	if _, ok := buildParityTable(big); ok {
		t.Fatalf("expected no table for m=%d (> maxTableBits=%d)", maxTableBits+1, maxTableBits)
	}

	r := rand.New(rand.NewSource(21))
	n := maxTableBits + 1
	alice := make([]byte, n)
	for i := range alice {
		alice[i] = byte(r.Intn(2))
	}
	bob := append([]byte(nil), alice...)
	res := BlockErrorDetect(alice, bob, big)
	if res.BlocksDropped != 0 || len(res.SurvivingAlice) != n {
		t.Fatalf("expected the fallback path to keep the single identical block, got %+v", res)
	}
}

// TestComputeBlockParityLargeScaleMatchesReference forces multiple workers
// each packing their own byte range, where a boundary off-by-one would hide
func TestComputeBlockParityLargeScaleMatchesReference(t *testing.T) {
	for _, g := range []GeneratorMatrix{testMatrixA, testMatrixC} {
		blockSize := g.BlockSize()
		n := minParallelBlockWork + 777 // past the threshold, not block-aligned
		r := rand.New(rand.NewSource(31))
		data := make([]byte, n)
		for i := range data {
			data[i] = byte(r.Intn(2))
		}

		got := ComputeBlockParity(data, g)

		masks := buildParityMasks(g)
		words := make([]uint64, masks.words)
		scratch := make([]byte, g.ParityBits())
		numBlocks := (n + blockSize - 1) / blockSize
		if len(got) != numBlocks {
			t.Fatalf("m=%d: expected %d blocks, got %d", blockSize, numBlocks, len(got))
		}
		for bi := range numBlocks {
			bs := bi * blockSize
			be := min(bs+blockSize, n)
			want := masks.parityInto(data[bs:be], words, scratch)
			if !bytes.Equal(want, got[bi]) {
				t.Fatalf("m=%d, block %d: reference=%v, got=%v", blockSize, bi, want, got[bi])
			}
		}
	}
}

// TestReconcileBlocksLargeScaleMatchesReference forces ReconcileBlocks
// past minParallelBlockWork, checked against a naive serial append
func TestReconcileBlocksLargeScaleMatchesReference(t *testing.T) {
	g := testMatrixC
	blockSize := g.BlockSize()
	n := minParallelBlockWork + 777
	r := rand.New(rand.NewSource(41))
	alice := make([]byte, n)
	bob := make([]byte, n)
	for i := range alice {
		b := byte(r.Intn(2))
		alice[i] = b
		bob[i] = b
		if r.Float64() < 0.02 {
			bob[i] ^= 1
		}
	}

	aliceParity := ComputeBlockParity(alice, g)
	bobParity := ComputeBlockParity(bob, g)
	got := ReconcileBlocks(alice, aliceParity, bobParity, blockSize)

	// naive serial reference: append in block order, no parallelism
	var wantSurviving, wantDiscarded []byte
	wantKept, wantDropped, wantLeaked := 0, 0, 0
	numBlocks := (n + blockSize - 1) / blockSize
	for bi := range numBlocks {
		bs := bi * blockSize
		be := min(bs+blockSize, n)
		if parityEqual(aliceParity[bi], bobParity[bi]) {
			wantKept++
			wantLeaked += len(aliceParity[bi])
			wantSurviving = append(wantSurviving, alice[bs:be]...)
		} else {
			wantDropped++
			wantDiscarded = append(wantDiscarded, alice[bs:be]...)
		}
	}

	if !bytes.Equal(got.Surviving, wantSurviving) {
		t.Fatal("Surviving diverged from the serial reference")
	}
	if !bytes.Equal(got.Discarded, wantDiscarded) {
		t.Fatal("Discarded diverged from the serial reference")
	}
	if got.BlocksKept != wantKept || got.BlocksDropped != wantDropped {
		t.Fatalf("expected kept=%d dropped=%d, got kept=%d dropped=%d", wantKept, wantDropped, got.BlocksKept, got.BlocksDropped)
	}
	if got.LeakedBits != wantLeaked {
		t.Fatalf("expected %d leaked bits, got %d", wantLeaked, got.LeakedBits)
	}
}

func TestBlockErrorDetectDropsErroredBlocksOnly(t *testing.T) {
	g := testMatrixC
	blockSize := g.BlockSize() // 6, 100% 2-bit-error detection
	numBlocks := 20
	alice := make([]byte, blockSize*numBlocks)
	bob := make([]byte, blockSize*numBlocks)
	r := rand.New(rand.NewSource(1))
	for i := range alice {
		b := byte(r.Intn(2))
		alice[i] = b
		bob[i] = b
	}
	// Introduce a 2-bit error in block 3 only
	bob[3*blockSize+1] ^= 1
	bob[3*blockSize+2] ^= 1

	res := BlockErrorDetect(alice, bob, g)
	if res.BlocksTotal != numBlocks {
		t.Fatalf("expected %d blocks, got %d", numBlocks, res.BlocksTotal)
	}
	if res.BlocksDropped != 1 {
		t.Fatalf("expected exactly 1 dropped block, got %d", res.BlocksDropped)
	}
	if len(res.SurvivingAlice) != blockSize*(numBlocks-1) {
		t.Fatalf("unexpected surviving length: %d", len(res.SurvivingAlice))
	}
	// Bits are never flipped: surviving Alice/Bob bits must still match
	// exactly, and discarded bits must be exactly the errored block
	for i := range res.SurvivingAlice {
		if res.SurvivingAlice[i] != res.SurvivingBob[i] {
			t.Fatalf("surviving stream has a mismatch at %d - a bit was corrected instead of discarded", i)
		}
	}
	if len(res.DiscardedAlice) != blockSize {
		t.Fatalf("expected discarded block of size %d, got %d", blockSize, len(res.DiscardedAlice))
	}
	if res.LeakedBits != g.ParityBits()*(numBlocks-1) {
		t.Fatalf("expected %d leaked bits, got %d", g.ParityBits()*(numBlocks-1), res.LeakedBits)
	}
}

func TestBlockErrorDetectHandlesShortFinalBlock(t *testing.T) {
	alice := make([]byte, 37) // not a multiple of the m=6 block size
	bob := make([]byte, 37)
	res := BlockErrorDetect(alice, bob, testMatrixC)
	if res.BlocksTotal != 7 { // 6*6=36, plus a 1-bit final block
		t.Fatalf("expected 7 blocks, got %d", res.BlocksTotal)
	}
}

// TestSplitReconciliationMatchesCombined: ComputeBlockParity + Reconcile
// run per side give the same result as the combined convenience wrapper
func TestSplitReconciliationMatchesCombined(t *testing.T) {
	g := testMatrixC
	blockSize := g.BlockSize()
	r := rand.New(rand.NewSource(7))
	n := 6001
	alice := make([]byte, n)
	bob := make([]byte, n)
	for i := range alice {
		b := byte(r.Intn(2))
		alice[i] = b
		bob[i] = b
		if r.Float64() < 0.05 {
			bob[i] ^= 1
		}
	}

	want := BlockErrorDetect(alice, bob, g)

	// Each side only ever touches its own bits plus the parity it
	// received --> never the peer's raw bits
	aliceParity := ComputeBlockParity(alice, g)
	bobParity := ComputeBlockParity(bob, g)
	aliceRes := ReconcileBlocks(alice, aliceParity, bobParity, blockSize)
	bobRes := ReconcileBlocks(bob, bobParity, aliceParity, blockSize)

	if !bytes.Equal(aliceRes.Surviving, want.SurvivingAlice) {
		t.Fatalf("Alice's split surviving bits diverged from the combined result")
	}
	if !bytes.Equal(bobRes.Surviving, want.SurvivingBob) {
		t.Fatalf("Bob's split surviving bits diverged from the combined result")
	}
	if !bytes.Equal(aliceRes.Discarded, want.DiscardedAlice) {
		t.Fatalf("Alice's split discarded bits diverged from the combined result")
	}
	if aliceRes.LeakedBits != want.LeakedBits {
		t.Fatalf("expected %d leaked bits, got %d", want.LeakedBits, aliceRes.LeakedBits)
	}
}

// TestParityTablePatchBitMatchesRebuild checks patchBit against an
// independent full rebuild after a random sequence of bit flips, and
// checks that patching the same (row, col) twice is a no-op
func TestParityTablePatchBitMatchesRebuild(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	m, p := 10, 4

	g := make(GeneratorMatrix, p)
	for i := range g {
		row := make([]byte, m)
		for j := range row {
			row[j] = byte(r.Intn(2))
		}
		g[i] = row
	}

	table, ok := buildParityTable(g)
	if !ok {
		t.Fatal("expected a table for m=10, p=4")
	}

	for range 50 {
		row, col := r.Intn(p), r.Intn(m)
		g[row][col] ^= 1
		table.patchBit(row, col)

		want, ok := buildParityTable(g)
		if !ok {
			t.Fatal("expected a table for m=10, p=4")
		}
		if !bytes.Equal(table.entries, want.entries) {
			t.Fatalf("patchBit(%d,%d) diverged from a full rebuild", row, col)
		}
	}

	// patching every entry a second time (same order) must undo it all
	// exactly, since flip-then-flip is a no-op on both the matrix and the table
	original, _ := buildParityTable(testMatrixC)
	patched := original
	patched.entries = append([]uint8(nil), original.entries...)
	patched.patchBit(1, 2)
	patched.patchBit(1, 2)
	if !bytes.Equal(patched.entries, original.entries) {
		t.Fatal("patching the same (row, col) twice did not restore the original table")
	}
}
