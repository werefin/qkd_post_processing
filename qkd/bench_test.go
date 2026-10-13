package qkd

import (
	"math/rand"
	"testing"
)

func benchInputs(n, l int) (x, seed []byte) {
	r := rand.New(rand.NewSource(1))
	x = randomBits(r, n)
	seed = randomBits(r, l+n-1)
	return
}

func BenchmarkToeplitzNaive_4kx2k(b *testing.B) {
	x, seed := benchInputs(4096, 2048)
	for b.Loop() {
		toeplitzHashNaive(x, seed, 2048)
	}
}

func BenchmarkToeplitzFast_4kx2k(b *testing.B) {
	x, seed := benchInputs(4096, 2048)
	for b.Loop() {
		toeplitzHashFast(x, seed, 2048)
	}
}

func BenchmarkToeplitzFast_100kx50k(b *testing.B) {
	x, seed := benchInputs(100000, 50000)
	for b.Loop() {
		toeplitzHashFast(x, seed, 50000)
	}
}

// BenchmarkToeplitzImpl_* compares every implementation head-to-head
// toeplitzHashFast always equals the fastest of these on the current CPU
func BenchmarkToeplitzImpl_4kx2k(b *testing.B) {
	x, seed := benchInputs(4096, 2048)
	for name, impl := range toeplitzImpls {
		b.Run(name, func(b *testing.B) {
			for b.Loop() {
				impl(x, seed, 2048)
			}
		})
	}
}

func BenchmarkToeplitzImpl_100kx50k(b *testing.B) {
	x, seed := benchInputs(100000, 50000)
	for name, impl := range toeplitzImpls {
		b.Run(name, func(b *testing.B) {
			for b.Loop() {
				impl(x, seed, 50000)
			}
		})
	}
}

func BenchmarkBlockErrorDetect_1M(b *testing.B) {
	r := rand.New(rand.NewSource(2))
	n := 1_000_000
	alice := randomBits(r, n)
	bob := append([]byte(nil), alice...)
	for b.Loop() {
		BlockErrorDetect(alice, bob, testMatrixC)
	}
}

func BenchmarkCRCVerify_1M(b *testing.B) {
	r := rand.New(rand.NewSource(2))
	n := 1_000_000
	alice := randomBits(r, n)
	bob := append([]byte(nil), alice...)
	for b.Loop() {
		CRCVerify(alice, bob, 2048)
	}
}

func BenchmarkBitsToBytesInto_2048(b *testing.B) {
	r := rand.New(rand.NewSource(2))
	bits := randomBits(r, 2048)
	var dst []byte
	for b.Loop() {
		dst = bitsToBytesInto(bits, dst)
	}
}

func BenchmarkWinnowReconcile_1M(b *testing.B) {
	r := rand.New(rand.NewSource(2))
	n := 1_000_000
	alice := randomBits(r, n)
	bob := append([]byte(nil), alice...)
	for i := range bob {
		if r.Float64() < 0.03 {
			bob[i] ^= 1
		}
	}
	cfg := WinnowConfig{
		BlockSizes:   []int{8, 8, 16, 16, 32},
		Permutations: make([][]int, 5),
	}
	for i := range cfg.Permutations {
		cfg.Permutations[i] = GeneratePermutation(n, int64(1000+i))
	}
	for b.Loop() {
		WinnowReconcile(alice, bob, cfg)
	}
}

func BenchmarkLDPCReconcile_1M(b *testing.B) {
	r := rand.New(rand.NewSource(2))
	n := 1_000_000
	alice := randomBits(r, n)
	bob := append([]byte(nil), alice...)
	for i := range bob {
		if r.Float64() < 0.03 {
			bob[i] ^= 1
		}
	}
	h, err := BuildRegularLDPC(n, 3, 10, 7) // rate 0.7; regular codes' real threshold runs below Shannon capacity, wr=12 (0.75) doesn't converge here even though 0.75<0.806
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		LDPCReconcile(h, alice, bob, 0.03, LDPCDecodeConfig{})
	}
}

// benchmarkPolarReconcile matches BenchmarkLDPCReconcile_1M: ~1M bits, QBER 3%, rate 0.7
func benchmarkPolarReconcile(b *testing.B, listSize int) {
	r := rand.New(rand.NewSource(2))
	n := 1 << 20
	alice := randomBits(r, n)
	bob := append([]byte(nil), alice...)
	for i := range bob {
		if r.Float64() < 0.03 {
			bob[i] ^= 1
		}
	}
	code, err := BuildPolarCode(1<<16, 7*(1<<16)/10, 0.03)
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, _, _, _, err := PolarReconcile(code, alice, bob, 0.03, PolarDecodeConfig{ListSize: listSize}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPolarReconcileSC_1M(b *testing.B)   { benchmarkPolarReconcile(b, 1) }
func BenchmarkPolarReconcileSCL8_1M(b *testing.B) { benchmarkPolarReconcile(b, 8) }
