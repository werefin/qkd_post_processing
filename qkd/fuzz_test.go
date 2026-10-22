package qkd

import (
	"bytes"
	"testing"
)

// toBits turns arbitrary fuzzer bytes into a 0/1-per-byte bit slice, since the fuzzer
// only generates native types, never our own "0/1 per byte" convention directly
func toBits(raw []byte) []byte {
	bits := make([]byte, len(raw))
	for i, b := range raw {
		bits[i] = b & 1
	}
	return bits
}

// FuzzToeplitzHashMatchesNaive is the fuzz target for the actual security claim: the
// optimized (CLMUL/popcount) Toeplitz hash must match the textbook GF(2) product for every input
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

// FuzzCRC16Deterministic checks the CRC-16 checksum is a pure function with no shared
// state leaking between calls -- a real risk given the table-driven implementation
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
