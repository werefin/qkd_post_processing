package qoj_post_processing

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestSplitEndToEndAliceBobIndependentlyAgree(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	n := 50000
	var aliceEvents, bobEvents []DetectionEvent
	tcur := int64(0)
	for range n {
		tcur += 1000
		aBasis := Basis(r.Intn(2))
		bBasis := Basis(r.Intn(2))
		aBit := byte(r.Intn(2))
		bBit := aBit
		if aBasis == bBasis && r.Float64() < 0.01 {
			bBit ^= 1
		} else if aBasis != bBasis {
			bBit = byte(r.Intn(2))
		}
		aliceEvents = append(aliceEvents, DetectionEvent{TimestampPS: tcur, Basis: aBasis, Bit: aBit})
		bobEvents = append(bobEvents, DetectionEvent{TimestampPS: tcur, Basis: bBasis, Bit: bBit})
	}

	const windowPS = 500
	g := testMatrixC
	const chunkSize = 2048

	// step 1 (sifting): each side strips its OWN events to the public view;
	// MatchCoincidences only ever sees PublicEvent, never a measured bit
	alicePublic := StripBit(aliceEvents)
	bobPublic := StripBit(bobEvents)
	matched := MatchCoincidences(alicePublic, bobPublic, windowPS)
	if len(matched) == 0 {
		t.Fatal("expected nonzero sifted bits")
	}

	// each side independently reads ITS OWN bit at the matched indices
	// the only place a measured bit is read, and only ever its own
	aliceBits := make([]byte, len(matched))
	bobBits := make([]byte, len(matched))
	for i, m := range matched {
		aliceBits[i] = aliceEvents[m.AliceIdx].Bit
		bobBits[i] = bobEvents[m.BobIdx].Bit
	}

	// step 2 (block error detection): only parity bits cross the channel
	aliceParity := ComputeBlockParity(aliceBits, g)
	bobParity := ComputeBlockParity(bobBits, g)
	aliceReconcile := ReconcileBlocks(aliceBits, aliceParity, bobParity, g.BlockSize())
	bobReconcile := ReconcileBlocks(bobBits, bobParity, aliceParity, g.BlockSize())

	if aliceReconcile.BlocksKept != bobReconcile.BlocksKept {
		t.Fatalf("Alice and Bob disagree on which blocks survived: alice kept %d, bob kept %d",
			aliceReconcile.BlocksKept, bobReconcile.BlocksKept)
	}

	// step 3 (QBER): the discarded blocks are the sacrificed public sample,
	// legitimate to compare openly since neither side's final key uses them
	qber := CalculateQBER(aliceReconcile.Discarded, bobReconcile.Discarded, len(matched))

	// step 4 (chunk verification): only CRC-16s cross the channel
	aliceCRC := ComputeChunkCRC(aliceReconcile.Surviving, chunkSize)
	bobCRC := ComputeChunkCRC(bobReconcile.Surviving, chunkSize)
	aliceChunks := ReconcileChunks(aliceReconcile.Surviving, aliceCRC, bobCRC, chunkSize)
	bobChunks := ReconcileChunks(bobReconcile.Surviving, bobCRC, aliceCRC, chunkSize)

	if aliceChunks.ChunksKept != bobChunks.ChunksKept {
		t.Fatalf("Alice and Bob disagree on which chunks survived: alice kept %d, bob kept %d",
			aliceChunks.ChunksKept, bobChunks.ChunksKept)
	}
	if !bytes.Equal(aliceChunks.Surviving, bobChunks.Surviving) {
		t.Fatal("CRC verification passed but the error-free streams still differ -- CRC-16 collision or a reconciliation bug")
	}

	leakedBits := aliceReconcile.LeakedBits + aliceChunks.LeakedBits

	// step 5 (privacy amplification): Alice generates the Toeplitz seed
	// it's public, safe to transmit, same as the parity/CRC bits above
	aliceKey, seed, err := PrivacyAmplify(aliceChunks.Surviving, qber.QBER, leakedBits, 0)
	if err != nil {
		t.Fatalf("Alice PrivacyAmplify: %v", err)
	}
	if len(aliceKey) == 0 {
		t.Fatal("expected a nonzero final key at 1% QBER over 50000 attempts")
	}

	// Bob receives only the seed over the classical channel and applies it
	// to HIS OWN surviving stream --> never Alice's bits or Alice's key
	l := SecureKeyLength(len(bobChunks.Surviving), qber.QBER, leakedBits)
	bobKey := toeplitzHashFast(bobChunks.Surviving, seed, l)

	if !bytes.Equal(aliceKey, bobKey) {
		t.Fatal("Alice and Bob derived DIFFERENT final keys from fully independent computation")
	}
}
