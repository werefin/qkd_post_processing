package qoj_post_processing

import (
	"bytes"
	"testing"
)

// toBits turns arbitrary fuzzer bytes into a 0/1-per-byte bit slice, since
// the fuzzer only generates native types ([]byte, string, int, ...), never
// our own "0/1 per byte" convention directly
func toBits(raw []byte) []byte {
	bits := make([]byte, len(raw))
	for i, b := range raw {
		bits[i] = b & 1
	}
	return bits
}

// FuzzToeplitzHashMatchesNaive is the fuzz target for the step with the
// actual security claim: the optimized (CLMUL/popcount) Toeplitz hash must
// match the textbook GF(2) matrix-vector product for every input, not just
// the fixed cases in TestToeplitzImplsMatchNaive
func FuzzToeplitzHashMatchesNaive(f *testing.F) {
	f.Add([]byte{1, 0, 1, 1, 0, 0, 1, 0}, []byte{1, 1, 0, 0, 1, 0, 1, 0, 1, 0, 1}, 4)
	f.Add([]byte{0, 0, 0, 0}, []byte{1, 0, 1, 0, 1, 0, 1}, 3)
	f.Add([]byte{1}, []byte{1}, 1)
	f.Fuzz(func(t *testing.T, dataRaw []byte, seedRaw []byte, l int) {
		if len(dataRaw) == 0 || len(dataRaw) > 5000 {
			return
		}
		if l <= 0 || l > 2000 {
			return
		}
		data := toBits(dataRaw)
		n := len(data)
		need := l + n - 1
		seedBits := toBits(seedRaw)
		if need <= 0 || need > len(seedBits) {
			return
		}
		seed := seedBits[:need]

		fast := toeplitzHashFast(data, seed, l)
		naive := toeplitzHashNaive(data, seed, l)
		if !bytes.Equal(fast, naive) {
			t.Fatalf("toeplitzHashFast disagrees with naive reference: n=%d l=%d\n fast=%v\nnaive=%v", n, l, fast, naive)
		}
	})
}

// FuzzComputeBlockParityMatchesReference checks the optimized (parity-table,
// parallel-for-large-input) block parity computation against the
// unoptimized mask-based reference, across random data and both test matrices
func FuzzComputeBlockParityMatchesReference(f *testing.F) {
	f.Add([]byte{1, 0, 1, 1, 0, 0, 1, 0, 1, 1, 0, 0, 1, 0, 1, 0})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, dataRaw []byte) {
		if len(dataRaw) > 20_000 {
			return
		}
		data := toBits(dataRaw)
		for _, g := range []GeneratorMatrix{testMatrixA, testMatrixC} {
			got := ComputeBlockParity(data, g)
			masks := buildParityMasks(g)
			words := make([]uint64, masks.words)
			scratch := make([]byte, g.ParityBits())
			blockSize := g.BlockSize()
			numBlocks := (len(data) + blockSize - 1) / blockSize
			if len(got) != numBlocks {
				t.Fatalf("m=%d: expected %d blocks, got %d", blockSize, numBlocks, len(got))
			}
			for bi := range numBlocks {
				bs := bi * blockSize
				be := min(bs+blockSize, len(data))
				want := masks.parityInto(data[bs:be], words, scratch)
				if !bytes.Equal(want, got[bi]) {
					t.Fatalf("m=%d, block %d: reference=%v, got=%v", blockSize, bi, want, got[bi])
				}
			}
		}
	})
}

// FuzzCRC16Deterministic checks the CRC-16 checksum is a pure function of
// its input (same bytes in, same checksum out, every time, no shared state
// leaking between calls --> the table-driven implementation makes this a
// real, not purely theoretical, risk)
func FuzzCRC16Deterministic(f *testing.F) {
	f.Add([]byte("123456789"))
	f.Add([]byte{})
	f.Add([]byte{0xff, 0x00, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		a := crc16Checksum(data)
		b := crc16Checksum(data)
		if a != b {
			t.Fatalf("crc16Checksum not deterministic: %v then %v for % x", a, b, data)
		}
	})
}

// FuzzBitsToBytesMatchesNaive extends TestBitsToBytesIntoMatchesNaive's
// fixed cases with randomized ones
func FuzzBitsToBytesMatchesNaive(f *testing.F) {
	f.Add([]byte{1, 0, 1, 1, 0, 0, 1, 0, 1})
	f.Fuzz(func(t *testing.T, dataRaw []byte) {
		if len(dataRaw) > 10_000 {
			return
		}
		bits := toBits(dataRaw)
		got := bitsToBytes(bits)
		want := naiveBitsToBytes(bits)
		if !bytes.Equal(got, want) {
			t.Fatalf("bitsToBytes disagrees with naive reference for %d bits: got=% x want=% x", len(bits), got, want)
		}
	})
}
