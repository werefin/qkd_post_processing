package qkdpostproc

import "fmt"

// bchPrimitivePoly holds one canonical primitive polynomial per GF(2^m)
// degree, used to build that field's exp table
var bchPrimitivePoly = map[int]int{
	2: 0b111,       // x^2+x+1
	3: 0b1011,      // x^3+x+1
	4: 0b10011,     // x^4+x+1
	5: 0b100101,    // x^5+x^2+1
	6: 0b1000011,   // x^6+x+1
	7: 0b10000011,  // x^7+x+1
	8: 0b100011101, // x^8+x^4+x^3+x^2+1
}

// galoisField holds the exp table (powers of the primitive element) for
// GF(2^bits), built from a canonical primitive polynomial
type galoisField struct {
	bits int
	n    int // 2^bits - 1, the multiplicative order
	exp  []int
}

func newGaloisField(bits int) (*galoisField, error) {
	poly, ok := bchPrimitivePoly[bits]
	if !ok {
		return nil, fmt.Errorf("bch: no primitive polynomial known for GF(2^%d)", bits)
	}
	n := (1 << bits) - 1
	exp := make([]int, n)
	val := 1
	for i := range exp {
		exp[i] = val
		val <<= 1
		if val&(1<<bits) != 0 {
			val ^= poly
		}
	}
	return &galoisField{bits: bits, n: n, exp: exp}, nil
}

// pow returns alpha^k, the field's primitive element raised to k
func (gf *galoisField) pow(k int) int {
	k %= gf.n
	if k < 0 {
		k += gf.n
	}
	return gf.exp[k]
}

// BCHGeneratorMatrix builds a shortened BCH parity-check matrix
// guaranteeing every error of Hamming weight <= t is detected
func BCHGeneratorMatrix(gfBits, t, blockSize int) (GeneratorMatrix, error) {
	gf, err := newGaloisField(gfBits)
	if err != nil {
		return nil, err
	}
	if t < 1 {
		return nil, fmt.Errorf("bch: t must be >= 1")
	}
	if blockSize <= 0 || blockSize > gf.n {
		return nil, fmt.Errorf("bch: blockSize must be in [1, %d] for GF(2^%d)", gf.n, gfBits)
	}

	var rows [][]byte
	for r := 1; r <= 2*t; r++ {
		bitPlanes := make([][]byte, gfBits)
		for b := range bitPlanes {
			bitPlanes[b] = make([]byte, blockSize)
		}
		for j := 0; j < blockSize; j++ {
			val := gf.pow(r * j)
			for b := 0; b < gfBits; b++ {
				if val&(1<<b) != 0 {
					bitPlanes[b][j] = 1
				}
			}
		}
		rows = append(rows, bitPlanes...)
	}
	rows = dedupeRows(rows)

	g := GeneratorMatrix(rows)
	if g.ParityBits() >= blockSize {
		return nil, fmt.Errorf("bch: t=%d over block size %d leaves no remaining secret (%d parity bits)", t, blockSize, g.ParityBits())
	}
	return g, nil
}

// GeneratorMatrixBCHt2 catches every 1/2/3/4-bit error over a 32-bit
// block, code rate 0.44
var GeneratorMatrixBCHt2 = mustBCHGeneratorMatrix(6, 2, 32)

func mustBCHGeneratorMatrix(gfBits, t, blockSize int) GeneratorMatrix {
	g, err := BCHGeneratorMatrix(gfBits, t, blockSize)
	if err != nil {
		panic(err)
	}
	return g
}

// dedupeRows drops exact duplicate rows - the direct construction above
// often repeats a bit-plane, wasting a leaked bit for no extra detection
func dedupeRows(rows [][]byte) [][]byte {
	seen := make(map[string]bool, len(rows))
	out := make([][]byte, 0, len(rows))
	for _, row := range rows {
		key := string(row)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, row)
	}
	return out
}
