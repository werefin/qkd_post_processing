package qkd

import (
	"math/rand"
	"sort"
	"testing"
)

// TestPolarTransformSelfInverse checks G_N=F^{\otimes n} applied twice returns the
// original input, for every block size this file's tests exercise
func TestPolarTransformSelfInverse(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for _, n := range []int{2, 4, 8, 16, 32, 64, 256} {
		for trial := 0; trial < 20; trial++ {
			u := randomBits(r, n)
			x, err := PolarTransform(u)
			if err != nil {
				t.Fatalf("N=%d: PolarTransform: %v", n, err)
			}
			back, err := PolarTransform(x)
			if err != nil {
				t.Fatalf("N=%d: PolarTransform (second pass): %v", n, err)
			}
			for i := range u {
				if back[i] != u[i] {
					t.Fatalf("N=%d trial=%d: self-inverse failed at bit %d: got %v, want %v", n, trial, i, back, u)
				}
			}
		}
	}
}

// TestPolarTransformRejectsNonPowerOfTwo checks the length guard
func TestPolarTransformRejectsNonPowerOfTwo(t *testing.T) {
	if _, err := PolarTransform(make([]byte, 6)); err == nil {
		t.Fatal("expected an error for a non-power-of-two length")
	}
}

// TestPolarReconcileNoiselessExact: with Bob's bits identical to Alice's, reconciliation
// must return them unchanged, for many random blocks and several block sizes
func TestPolarReconcileNoiselessExact(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	for _, n := range []int{16, 64, 256} {
		code, err := BuildPolarCode(n, n/2, 0.03)
		if err != nil {
			t.Fatalf("N=%d: BuildPolarCode: %v", n, err)
		}
		for trial := 0; trial < 30; trial++ {
			alice := randomBits(r, n)
			bob := append([]byte(nil), alice...)
			corrected, totalBlocks, _, leaked, err := PolarReconcile(code, alice, bob, 0.03, PolarDecodeConfig{})
			if err != nil {
				t.Fatalf("N=%d trial=%d: PolarReconcile: %v", n, trial, err)
			}
			if totalBlocks != 1 {
				t.Fatalf("N=%d: expected 1 block, got %d", n, totalBlocks)
			}
			if leaked != n-n/2 {
				t.Fatalf("N=%d: expected %d leaked bits, got %d", n, n-n/2, leaked)
			}
			for i := range alice {
				if corrected[i] != alice[i] {
					t.Fatalf("N=%d trial=%d: noiseless reconciliation mismatch at bit %d", n, trial, i)
				}
			}
		}
	}
}

// bruteForceMLDecode tries all 2^K information assignments around Alice's frozen values and
// returns the block nearest bob in Hamming distance, the true ML decision on a BSC
func bruteForceMLDecode(code *PolarCode, aliceU, bob []byte) []byte {
	infoIdx := make([]int, 0, code.K)
	for i := 0; i < code.N; i++ {
		if !code.Frozen[i] {
			infoIdx = append(infoIdx, i)
		}
	}
	candidateU := make([]byte, code.N)
	for _, idx := range code.FrozenIdx {
		candidateU[idx] = aliceU[idx]
	}

	var best []byte
	bestDist := -1
	for assignment := 0; assignment < 1<<uint(code.K); assignment++ {
		for bit, idx := range infoIdx {
			candidateU[idx] = byte((assignment >> uint(bit)) & 1)
		}
		candidateA, err := PolarTransform(candidateU)
		if err != nil {
			panic(err) // code.N is always a power of two by construction
		}
		dist := 0
		for i := range bob {
			if candidateA[i] != bob[i] {
				dist++
			}
		}
		if bestDist == -1 || dist < bestDist {
			bestDist = dist
			best = append([]byte(nil), candidateA...)
		}
	}
	return best
}

// bitFlipChannel applies an i.i.d. BSC(qber) to raw, returning a fresh noisy copy
func bitFlipChannel(r *rand.Rand, raw []byte, qber float64) []byte {
	out := append([]byte(nil), raw...)
	for i := range out {
		if r.Float64() < qber {
			out[i] ^= 1
		}
	}
	return out
}

// TestPolarSCDecodeMatchesBruteForceML: at small N, SC's block success rate must track true
// ML decoding; a large gap means transform and decoder disagree on index convention
func TestPolarSCDecodeMatchesBruteForceML(t *testing.T) {
	const trials = 3000
	const qber = 0.08
	r := rand.New(rand.NewSource(3))

	for _, tc := range []struct{ n, k int }{{4, 2}, {8, 4}} {
		code, err := BuildPolarCode(tc.n, tc.k, qber)
		if err != nil {
			t.Fatalf("N=%d: BuildPolarCode: %v", tc.n, err)
		}
		scSuccess, mlSuccess, sclSuccess := 0, 0, 0
		for trial := 0; trial < trials; trial++ {
			alice := randomBits(r, tc.n)
			bob := bitFlipChannel(r, alice, qber)
			aliceU, err := PolarTransform(alice)
			if err != nil {
				t.Fatalf("PolarTransform: %v", err)
			}
			frozenVals := make([]byte, len(code.FrozenIdx))
			for i, idx := range code.FrozenIdx {
				frozenVals[i] = aliceU[idx]
			}

			scU, err := PolarSCDecode(code, bob, frozenVals, qber, PolarDecodeConfig{ListSize: 1})
			if err != nil {
				t.Fatalf("PolarSCDecode: %v", err)
			}
			scA, _ := PolarTransform(scU)
			if bytesEqual(scA, alice) {
				scSuccess++
			}

			sclU, err := PolarSCDecode(code, bob, frozenVals, qber, PolarDecodeConfig{ListSize: 8})
			if err != nil {
				t.Fatalf("PolarSCDecode (list): %v", err)
			}
			sclA, _ := PolarTransform(sclU)
			if bytesEqual(sclA, alice) {
				sclSuccess++
			}

			mlA := bruteForceMLDecode(code, aliceU, bob)
			if bytesEqual(mlA, alice) {
				mlSuccess++
			}
		}

		scRate := float64(scSuccess) / trials
		sclRate := float64(sclSuccess) / trials
		mlRate := float64(mlSuccess) / trials
		t.Logf("N=%d K=%d qber=%.2f: SC(L=1) success=%.4f, SCL(L=8) success=%.4f, brute-force ML success=%.4f",
			tc.n, tc.k, qber, scRate, sclRate, mlRate)

		if mlRate > 0 && scRate < 0.5*mlRate {
			t.Fatalf("N=%d: plain SC success rate %.4f is less than half brute-force ML's %.4f, indicates a decoder bug", tc.n, scRate, mlRate)
		}
		if sclRate < scRate-1e-9 {
			t.Fatalf("N=%d: SCL(L=8) success rate %.4f is worse than plain SC's %.4f, list decoding should never do worse", tc.n, sclRate, scRate)
		}
	}
}

func bytesEqual(a, b []byte) bool {
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

// genieChannelLLR is each synthetic channel's LLR when every earlier bit is genie-known,
// test-only, to check gaMeans and the decoder share one index convention
func genieChannelLLR(llr []float64, trueU []byte) []float64 {
	n := len(llr)
	if n == 1 {
		return []float64{llr[0]}
	}
	half := n / 2
	u1, u2 := trueU[:half], trueU[half:]
	lv1 := make([]float64, half)
	for i := range lv1 {
		lv1[i] = llrF(llr[i], llr[half+i])
	}
	out1 := genieChannelLLR(lv1, u1)

	code1, _ := PolarTransform(u1) // the true left codeword T(u1), genie-known
	lv2 := make([]float64, half)
	for i := range lv2 {
		lv2[i] = llrG(llr[i], llr[half+i], code1[i])
	}
	out2 := genieChannelLLR(lv2, u2)
	return append(out1, out2...)
}

// rankOf assigns average ranks (1-based, ties split evenly), for Spearman's rho below
func rankOf(x []float64) []float64 {
	n := len(x)
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return x[idx[a]] < x[idx[b]] })
	ranks := make([]float64, n)
	i := 0
	for i < n {
		j := i
		for j+1 < n && x[idx[j+1]] == x[idx[i]] {
			j++
		}
		avgRank := float64(i+j)/2 + 1
		for k := i; k <= j; k++ {
			ranks[idx[k]] = avgRank
		}
		i = j + 1
	}
	return ranks
}

// spearmanRho is the rank correlation coefficient between x and y
func spearmanRho(x, y []float64) float64 {
	n := len(x)
	rx, ry := rankOf(x), rankOf(y)
	var sumD2 float64
	for i := range n {
		d := rx[i] - ry[i]
		sumD2 += d * d
	}
	return 1 - 6*sumD2/(float64(n)*float64(n*n-1))
}

// TestPolarReliabilityOrderingMatchesMeasuredErrors: positions gaMeans ranks reliable must
// measure reliable under genie-aided decoding, or transform/decoder/construction disagree
func TestPolarReliabilityOrderingMatchesMeasuredErrors(t *testing.T) {
	const n = 64
	const qber = 0.1
	const trials = 4000
	r := rand.New(rand.NewSource(4))

	mean := gaMeans(6, qber)
	unreliability := make([]float64, n)
	for i, m := range mean {
		unreliability[i] = -m
	}
	errCount := make([]float64, n)
	for trial := 0; trial < trials; trial++ {
		alice := randomBits(r, n)
		bob := bitFlipChannel(r, alice, qber)
		aliceU, err := PolarTransform(alice)
		if err != nil {
			t.Fatalf("PolarTransform: %v", err)
		}
		llr := channelLLRs(bob, qber)
		genie := genieChannelLLR(llr, aliceU)
		for i := range genie {
			predicted := byte(0)
			if genie[i] < 0 {
				predicted = 1
			}
			if predicted != aliceU[i] {
				errCount[i]++
			}
		}
	}
	errRate := make([]float64, n)
	for i := range errRate {
		errRate[i] = errCount[i] / trials
	}

	rho := spearmanRho(unreliability, errRate)
	t.Logf("Spearman rank correlation between GA unreliability and measured bit error rate: %.4f", rho)
	if rho < 0.9 {
		t.Fatalf("expected a strong positive rank correlation, got %.4f", rho)
	}
}

// TestPolarReconcileBlockFailureRate bounds block failure at a conservative margin and checks
// a larger SCL list lowers it, the trend Yan et al. 2018 (Table 1) report
func TestPolarReconcileBlockFailureRate(t *testing.T) {
	t.Parallel() // heavy and independent: runs beside the other heavy tests
	const n = 1024
	const qber = 0.03
	const margin = 0.15
	const trials = 500
	rate := 1 - binaryEntropy(qber) - margin
	k := int(rate * n)

	code, err := BuildPolarCode(n, k, qber)
	if err != nil {
		t.Fatalf("BuildPolarCode: %v", err)
	}

	measure := func(listSize int, seed int64) float64 {
		r := rand.New(rand.NewSource(seed))
		failures := 0
		for trial := 0; trial < trials; trial++ {
			alice := randomBits(r, n)
			bob := bitFlipChannel(r, alice, qber)
			corrected, _, _, _, err := PolarReconcile(code, alice, bob, qber, PolarDecodeConfig{ListSize: listSize})
			if err != nil {
				t.Fatalf("PolarReconcile: %v", err)
			}
			if !bytesEqual(corrected, alice) {
				failures++
			}
		}
		return float64(failures) / trials
	}

	failL1 := measure(1, 10)
	failL16 := measure(16, 10)
	t.Logf("N=%d K=%d qber=%.2f rate=%.3f (margin=%.2f below 1-h2(qber)=%.3f): block failure rate SC(L=1)=%.4f, SCL(L=16)=%.4f over %d trials",
		n, k, qber, rate, margin, 1-binaryEntropy(qber), failL1, failL16, trials)

	if failL16 > failL1+1e-9 && failL1 > 0 {
		t.Fatalf("expected SCL(L=16) failure rate (%.4f) not to exceed plain SC's (%.4f)", failL16, failL1)
	}
	if failL16 > 0.08 { // measured ~2.4%; the old Bhattacharyya construction gave ~17%

		t.Fatalf("SCL(L=16) block failure rate %.4f is too high at this honestly-conservative margin; tune margin or list size", failL16)
	}
}
