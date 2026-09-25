package qoj_post_processing

import (
	"bytes"
	"testing"
)

// TestBCHGeneratorMatrixDetectsUpToT: by linearity, checking parity(e)!=0
// for every nonzero e of weight<=2t covers every differing block pair
func TestBCHGeneratorMatrixDetectsUpToT(t *testing.T) {
	cases := []struct {
		gfBits, t, blockSize int
	}{
		{3, 1, 7},  // Hamming(7,4)-equivalent: guarantees weight 1-2
		{4, 2, 15}, // guarantees weight 1-4
	}
	for _, c := range cases {
		g, err := BCHGeneratorMatrix(c.gfBits, c.t, c.blockSize)
		if err != nil {
			t.Fatalf("gfBits=%d t=%d blockSize=%d: %v", c.gfBits, c.t, c.blockSize, err)
		}
		masks := buildParityMasks(g)
		words := make([]uint64, masks.words)
		parity := make([]byte, g.ParityBits())
		zeroParity := make([]byte, g.ParityBits())

		e := make([]byte, c.blockSize)
		var walk func(start, left int) bool
		walk = func(start, left int) bool {
			if left == 0 {
				got := masks.parityInto(e, words, parity)
				return !parityEqual(got, zeroParity)
			}
			for i := start; i <= c.blockSize-left; i++ {
				e[i] = 1
				ok := walk(i+1, left-1)
				e[i] = 0
				if !ok {
					return false
				}
			}
			return true
		}

		for w := 1; w <= 2*c.t; w++ {
			if !walk(0, w) {
				t.Fatalf("gfBits=%d t=%d: a weight-%d error pattern %v went undetected", c.gfBits, c.t, w, e)
			}
		}
	}
}

func TestBCHGeneratorMatrixRejectsUnsupportedField(t *testing.T) {
	if _, err := BCHGeneratorMatrix(20, 1, 10); err == nil {
		t.Fatal("expected an error for an unsupported GF(2^20)")
	}
}

func TestBCHGeneratorMatrixRejectsOversizedBlock(t *testing.T) {
	// GF(2^3) has multiplicative order 7, so blockSize=8 is out of range
	if _, err := BCHGeneratorMatrix(3, 1, 8); err == nil {
		t.Fatal("expected an error for a block size beyond the field's natural length")
	}
}

func TestBCHGeneratorMatrixRejectsNoRemainingSecret(t *testing.T) {
	// t=3 over a 7-bit block leaves no room for a positive code rate
	if _, err := BCHGeneratorMatrix(3, 3, 7); err == nil {
		t.Fatal("expected an error when parity bits would consume the whole block")
	}
}

// TestBCHGeneratorMatrixPlugsIntoBlockErrorDetect: a BCH matrix works
// with the same convenience wrapper the patent-derived matrices use
func TestBCHGeneratorMatrixPlugsIntoBlockErrorDetect(t *testing.T) {
	g, err := BCHGeneratorMatrix(4, 2, 15)
	if err != nil {
		t.Fatalf("BCHGeneratorMatrix: %v", err)
	}
	blockSize := g.BlockSize()
	numBlocks := 10
	alice := make([]byte, blockSize*numBlocks)
	bob := make([]byte, blockSize*numBlocks)
	for i := range alice {
		alice[i] = byte(i % 2)
		bob[i] = alice[i]
	}
	// a 4-bit error in block 2, within the guaranteed weight-4 detection range
	bob[2*blockSize+0] ^= 1
	bob[2*blockSize+3] ^= 1
	bob[2*blockSize+6] ^= 1
	bob[2*blockSize+9] ^= 1

	res := BlockErrorDetect(alice, bob, g)
	if res.BlocksDropped != 1 {
		t.Fatalf("expected exactly 1 dropped block, got %d", res.BlocksDropped)
	}
	if !bytes.Equal(res.SurvivingAlice, res.SurvivingBob) {
		t.Fatal("surviving stream has a mismatch --> a bit was corrected instead of discarded")
	}
}
