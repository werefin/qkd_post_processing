package qoj_post_processing

import (
	"bytes"
	"math/rand"
	"testing"
)

// naiveBitsToBytes is the original one-bit-at-a-time reference, kept only
// here as an independent oracle for TestBitsToBytesIntoMatchesNaive
func naiveBitsToBytes(bits []byte) []byte {
	dst := make([]byte, (len(bits)+7)/8)
	for i, b := range bits {
		if b != 0 {
			dst[i/8] |= 1 << uint(7-i%8)
		}
	}
	return dst
}

// TestBitsToBytesIntoMatchesNaive checks the branch-free packer against
// the naive reference across every tail size (0-7 bits) and a dirty dst
func TestBitsToBytesIntoMatchesNaive(t *testing.T) {
	r := rand.New(rand.NewSource(21))
	for _, n := range []int{0, 1, 7, 8, 9, 15, 16, 17, 63, 64, 65, 300, 2048, 2053} {
		bits := make([]byte, n)
		for i := range bits {
			bits[i] = byte(r.Intn(2))
		}
		want := naiveBitsToBytes(bits)

		// dirty, oversized dst: every byte must still end up correct
		dirty := make([]byte, len(want)+8)
		for i := range dirty {
			dirty[i] = 0xFF
		}
		got := bitsToBytesInto(bits, dirty)
		if !bytes.Equal(got, want) {
			t.Fatalf("n=%d: got %08b, want %08b", n, got, want)
		}
	}
}

// TestCRC16ChecksumMatchesKnownVector checks against the standard
// CRC-16/CCITT-FALSE test vector for "123456789" --> 0x29B1
func TestCRC16ChecksumMatchesKnownVector(t *testing.T) {
	got := crc16Checksum([]byte("123456789"))
	if got != 0x29B1 {
		t.Fatalf("expected 0x29B1, got 0x%04X", got)
	}
}

func TestCRCVerifyCatchesMismatch(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	n := 1000
	alice := make([]byte, n)
	for i := range alice {
		alice[i] = byte(r.Intn(2))
	}
	bob := append([]byte(nil), alice...)
	bob[550] ^= 1 // single error inside chunk 2 (chunkSize 256 --> chunk index 2)

	chunkSize := 256
	res := CRCVerify(alice, bob, chunkSize)
	if res.ChunksTotal != 4 {
		t.Fatalf("expected 4 chunks, got %d", res.ChunksTotal)
	}
	if res.ChunksDropped != 1 {
		t.Fatalf("expected exactly 1 dropped chunk, got %d", res.ChunksDropped)
	}
	for i := range res.SurvivingAlice {
		if res.SurvivingAlice[i] != res.SurvivingBob[i] {
			t.Fatalf("surviving CRC-verified stream has a mismatch at %d", i)
		}
	}
}

func TestCRCVerifyIdenticalStreamsAllKept(t *testing.T) {
	r := rand.New(rand.NewSource(4))
	n := 2000
	alice := make([]byte, n)
	for i := range alice {
		alice[i] = byte(r.Intn(2))
	}
	bob := append([]byte(nil), alice...)
	res := CRCVerify(alice, bob, 300)
	if res.ChunksDropped != 0 {
		t.Fatalf("expected 0 dropped chunks for identical streams, got %d", res.ChunksDropped)
	}
	if len(res.SurvivingAlice) != n {
		t.Fatalf("expected all %d bits to survive, got %d", n, len(res.SurvivingAlice))
	}
}

// TestCRCSplitMatchesCombined: ComputeChunkCRC + ReconcileChunks run per
// side give the same result as the combined convenience wrapper
func TestCRCSplitMatchesCombined(t *testing.T) {
	r := rand.New(rand.NewSource(9))
	n := 5001
	chunkSize := 300
	alice := make([]byte, n)
	bob := make([]byte, n)
	for i := range alice {
		b := byte(r.Intn(2))
		alice[i] = b
		bob[i] = b
		if r.Float64() < 0.01 {
			bob[i] ^= 1
		}
	}

	want := CRCVerify(alice, bob, chunkSize)

	aliceCRC := ComputeChunkCRC(alice, chunkSize)
	bobCRC := ComputeChunkCRC(bob, chunkSize)
	aliceRes := ReconcileChunks(alice, aliceCRC, bobCRC, chunkSize)
	bobRes := ReconcileChunks(bob, bobCRC, aliceCRC, chunkSize)

	if !bytes.Equal(aliceRes.Surviving, want.SurvivingAlice) {
		t.Fatal("Alice's split surviving bits diverged from the combined result")
	}
	if !bytes.Equal(bobRes.Surviving, want.SurvivingBob) {
		t.Fatal("Bob's split surviving bits diverged from the combined result")
	}
	if !bytes.Equal(aliceRes.Discarded, want.DiscardedAlice) {
		t.Fatal("Alice's split discarded bits diverged from the combined result")
	}
	if aliceRes.LeakedBits != want.LeakedBits {
		t.Fatalf("expected %d leaked bits, got %d", want.LeakedBits, aliceRes.LeakedBits)
	}
}
