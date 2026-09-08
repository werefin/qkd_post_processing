package qkdpostproc

import (
	"encoding/binary"
	"runtime"
	"sync"
)

// crc16Poly is CRC-16/CCITT-FALSE, init 0xFFFF, no reflect, xorout 0
const crc16Poly = 0x1021

// crc16Table is built once at init from crc16Poly, one entry per byte value
var crc16Table = buildCRC16Table(crc16Poly)

func buildCRC16Table(poly uint16) [256]uint16 {
	var t [256]uint16
	for i := range 256 {
		crc := uint16(i) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ poly
			} else {
				crc <<= 1
			}
		}
		t[i] = crc
	}
	return t
}

// crc16Checksum runs CRC-16/CCITT-FALSE over data, byte at a time via table
func crc16Checksum(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc = crc<<8 ^ crc16Table[byte(crc>>8)^b]
	}
	return crc
}

// CRCVerifyResult summarizes step 3
type CRCVerifyResult struct {
	SurvivingAlice []byte // final error-cleaned bits (Alice side, post block+CRC filtering)
	SurvivingBob   []byte
	DiscardedAlice []byte // bits from chunks whose CRCs disagreed (for QBER use)
	DiscardedBob   []byte
	ChunksTotal    int
	ChunksKept     int
	ChunksDropped  int
	LeakedBits     int // classical-channel bits spent announcing CRCs of surviving chunks (16 bits/chunk)
}

// bitsToBytes packs 0/1 bits (MSB-first) into bytes, zero-padding the
// last byte --> both sides pad identically so it never causes a mismatch
func bitsToBytes(bits []byte) []byte {
	return bitsToBytesInto(bits, nil)
}

// bitsToBytesInto reuses dst instead of allocating, packing 8 bits per
// word load, branch-free, instead of one bit at a time
func bitsToBytesInto(bits []byte, dst []byte) []byte {
	n := len(bits)
	need := (n + 7) / 8
	if cap(dst) < need {
		dst = make([]byte, need)
	} else {
		dst = dst[:need]
	}

	full := n &^ 7 // largest multiple of 8 <= n
	i := 0
	for ; i < full; i += 8 {
		v := binary.BigEndian.Uint64(bits[i : i+8])
		dst[i>>3] = byte(v>>56)&1<<7 | byte(v>>48)&1<<6 | byte(v>>40)&1<<5 | byte(v>>32)&1<<4 |
			byte(v>>24)&1<<3 | byte(v>>16)&1<<2 | byte(v>>8)&1<<1 | byte(v)&1
	}
	if i < n {
		var b byte
		for k := i; k < n; k++ {
			b |= (bits[k] & 1) << uint(7-(k-i))
		}
		dst[i>>3] = b
	}
	return dst
}

// ComputeChunkCRC computes the CRC-16 of every chunkSize-bit chunk, using
// only this side's own bits --> a peer only ever needs the result
func ComputeChunkCRC(bits []byte, chunkSize int) []uint16 {
	if chunkSize <= 0 {
		panic("chunkSize must be > 0")
	}
	n := len(bits)
	numChunks := (n + chunkSize - 1) / chunkSize
	crcs := make([]uint16, numChunks)

	workers := max(min(runtime.GOMAXPROCS(0), numChunks), 1)

	var wg sync.WaitGroup
	chunkOfWork := (numChunks + workers - 1) / workers
	for w := range workers {
		start := w * chunkOfWork
		end := min(start+chunkOfWork, numChunks)
		if start >= end {
			continue
		}
		wg.Add(1)
		go func(startC, endC int) {
			defer wg.Done()
			var buf []byte
			for ci := startC; ci < endC; ci++ {
				cs := ci * chunkSize
				ce := min(cs+chunkSize, n)
				buf = bitsToBytesInto(bits[cs:ce], buf)
				crcs[ci] = crc16Checksum(buf)
			}
		}(start, end)
	}
	wg.Wait()
	return crcs
}

// ChunkReconcileResult summarizes one side's outcome of comparing its own
// chunk CRCs against a peer's, without ever needing the peer's bits
type ChunkReconcileResult struct {
	Surviving     []byte
	Discarded     []byte
	ChunksTotal   int
	ChunksKept    int
	ChunksDropped int
	LeakedBits    int // classical-channel bits spent announcing CRCs of surviving chunks
}

// ReconcileChunks drops any chunk whose own CRC disagrees with the
// peer's --> never needs to know the peer's raw bits
func ReconcileChunks(ownBits []byte, ownCRC, peerCRC []uint16, chunkSize int) ChunkReconcileResult {
	if len(ownCRC) != len(peerCRC) {
		panic("ownCRC and peerCRC must cover the same number of chunks")
	}
	numChunks := len(ownCRC)
	n := len(ownBits)

	var res ChunkReconcileResult
	res.ChunksTotal = numChunks

	kept := make([]bool, numChunks)
	var survivingLen, discardedLen int
	for ci := range numChunks {
		cs := ci * chunkSize
		ce := min(cs+chunkSize, n)
		clen := ce - cs
		ok := ownCRC[ci] == peerCRC[ci]
		kept[ci] = ok
		if ok {
			survivingLen += clen
			res.LeakedBits += 16
		} else {
			discardedLen += clen
		}
	}
	res.Surviving = make([]byte, 0, survivingLen)
	res.Discarded = make([]byte, 0, discardedLen)

	for ci := range numChunks {
		cs := ci * chunkSize
		ce := min(cs+chunkSize, n)
		if kept[ci] {
			res.ChunksKept++
			res.Surviving = append(res.Surviving, ownBits[cs:ce]...)
		} else {
			res.ChunksDropped++
			res.Discarded = append(res.Discarded, ownBits[cs:ce]...)
		}
	}
	return res
}

// CRCVerify is a convenience wrapper for simulation and testing
// use ComputeChunkCRC + ReconcileChunks directly to run each side apart
func CRCVerify(alice, bob []byte, chunkSize int) CRCVerifyResult {
	aliceCRC := ComputeChunkCRC(alice, chunkSize)
	bobCRC := ComputeChunkCRC(bob, chunkSize)
	aliceRes := ReconcileChunks(alice, aliceCRC, bobCRC, chunkSize)
	bobRes := ReconcileChunks(bob, bobCRC, aliceCRC, chunkSize)
	return CRCVerifyResult{
		SurvivingAlice: aliceRes.Surviving,
		SurvivingBob:   bobRes.Surviving,
		DiscardedAlice: aliceRes.Discarded,
		DiscardedBob:   bobRes.Discarded,
		ChunksTotal:    aliceRes.ChunksTotal,
		ChunksKept:     aliceRes.ChunksKept,
		ChunksDropped:  aliceRes.ChunksDropped,
		LeakedBits:     aliceRes.LeakedBits, // same 16-bits-per-kept-chunk count on both sides
	}
}
