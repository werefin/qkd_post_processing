package qkd

import (
	"bytes"
	"math/rand"
	"sync"
	"testing"

	"qkd_post_processing/pyec"
)

var (
	backendOnce sync.Once
	backend     *pyec.Pool
	backendErr  error
)

// ecBackend starts the Python service once; tests skip where ldpc and py3gpp are not installed
func ecBackend(t *testing.T) LDPC5GBackend {
	t.Helper()
	backendOnce.Do(func() { backend, backendErr = pyec.Start(0) })
	if backendErr != nil {
		t.Skipf("5G NR LDPC service unavailable (set QKD_EC_PYTHON to a Python with ec/requirements.txt): %v", backendErr)
	}
	return backend
}

func randomBSC(n int, q float64, seed int64) (alice, bob []byte, qs []float64) {
	r := rand.New(rand.NewSource(seed))
	alice, bob, qs = make([]byte, n), make([]byte, n), make([]float64, n)
	for i := range alice {
		alice[i] = byte(r.Intn(2))
		bob[i] = alice[i]
		if r.Float64() < q {
			bob[i] ^= 1
		}
		qs[i] = q
	}
	return
}

// TestLDPC5GReconcile: across the QBER range every frame is recovered exactly within the efficiency
// the comparison measured, and the punctured columns earn their credit
func TestLDPC5GReconcile(t *testing.T) {
	b := ecBackend(t)
	for _, q := range []float64{0.02, 0.05, 0.09} {
		alice, bob, qs := randomBSC(120000, q, int64(q*1000))
		got, res, err := LDPC5GReconcile(b, alice, bob, qs, q, 7)
		if err != nil {
			t.Fatal(err)
		}
		f := float64(res.LeakedBits) / (float64(len(alice)) * binaryEntropy(q))
		t.Logf("q %.2f: m=%d, %d frames, %d attempts, credit %v, f = %.3f", q, res.M, res.Frames, res.Attempts, res.Credit, f)
		if res.Failed > 0 || !bytes.Equal(got, alice) || !res.Credit || f > 1.4 {
			t.Fatalf("q %.2f: %d failed, equal %v, f %.3f", q, res.Failed, bytes.Equal(got, alice), f)
		}
	}
}

// TestRunLDPC5GBeatsPolar: on the same data and sample the full chain with 5G NR LDPC certifies a
// verified key and discloses less than polar SCL8
func TestRunLDPC5GBeatsPolar(t *testing.T) {
	b := ecBackend(t)
	alice, bob := simulateClicks(600000, 0.05, 21)
	ld, err := RunLDPC5G(alice, bob, LDPC5GRunConfig{Backend: b, CoincidenceWindowPS: 500, Seed: 5, ChunkSize: 1024, ExpectedQBER: 0.05})
	if err != nil {
		t.Fatal(err)
	}
	pol, err := RunPolar(alice, bob, PolarRunConfig{CoincidenceWindowPS: 500, Seed: 5, ChunkSize: 1024, ExpectedQBER: 0.05, RateMargin: 0.02, ListSize: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("5G NR LDPC: %d disclosed, %d-bit key; polar SCL8: %d disclosed, %d-bit key", ld.LeakedBits, len(ld.FinalKeyBits), pol.LeakedBits, len(pol.FinalKeyBits))
	if len(ld.FinalKeyBits) == 0 || !ld.FiniteKey.Verified || ld.LeakedBits >= pol.LeakedBits || len(ld.FinalKeyBits) <= len(pol.FinalKeyBits) {
		t.Fatal("5G NR LDPC should certify a longer key with less disclosure")
	}
}
