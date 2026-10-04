package qoj_post_processing

import (
	"encoding/json"
	"math/rand"
	"os"
	"testing"
)

// toeplitzVector is one cross-check case: Go's own output for a given
// (data, seed, l), for an independent Python/NumPy implementation to verify
type toeplitzVector struct {
	DataBits []byte `json:"data_bits"`
	SeedBits []byte `json:"seed_bits"`
	L        int    `json:"l"`
	WantHash []byte `json:"want_hash"` // Go's toeplitzHashFast output, 0/1 per byte
}

// TestGenerateToeplitzCrossCheckVectors writes testdata/toeplitz_vectors.json
// for cross_check_toeplitz.py to independently re-derive with NumPy
func TestGenerateToeplitzCrossCheckVectors(t *testing.T) {
	if os.Getenv("GENERATE_VECTORS") == "" {
		t.Skip("set GENERATE_VECTORS=1 to (re)generate testdata/toeplitz_vectors.json")
	}

	r := rand.New(rand.NewSource(2026))
	var vectors []toeplitzVector
	sizes := []struct{ n, l int }{
		{1, 1}, {8, 4}, {17, 5}, {100, 32}, {256, 128}, {777, 256}, {4096, 1024},
	}
	for _, sz := range sizes {
		data := make([]byte, sz.n)
		for i := range data {
			data[i] = byte(r.Intn(2))
		}
		seed := make([]byte, sz.l+sz.n-1)
		for i := range seed {
			seed[i] = byte(r.Intn(2))
		}
		got := toeplitzHashFast(data, seed, sz.l)
		vectors = append(vectors, toeplitzVector{
			DataBits: data, SeedBits: seed, L: sz.l, WantHash: got,
		})
	}

	if err := os.MkdirAll("testdata", 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create("testdata/toeplitz_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(vectors); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d vectors to testdata/toeplitz_vectors.json", len(vectors))
}
