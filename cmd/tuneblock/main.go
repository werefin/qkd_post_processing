// Command tuneblock compares the three example generator matrices over
// one fixed simulated batch and reports the highest key rate
package main

import (
	"flag"
	"fmt"
	"math/rand"

	qkd "qkdpostproc"
)

// simulate is the same generator cmd/qkdpostproc uses, duplicated here so
// this tool has no dependency beyond the library itself
func simulate(n int, errRate, multiPhotonRate float64, jitterPS int64, seed int64) ([]qkd.DetectionEvent, []qkd.DetectionEvent) {
	r := rand.New(rand.NewSource(seed))
	alice := make([]qkd.DetectionEvent, 0, n)
	bob := make([]qkd.DetectionEvent, 0, n)

	t := int64(0)
	for range n {
		t += 1000
		aBasis := qkd.Basis(r.Intn(2))
		bBasis := qkd.Basis(r.Intn(2))
		aBit := byte(r.Intn(2))
		bBit := aBit
		if aBasis == bBasis && r.Float64() < errRate {
			bBit ^= 1
		} else if aBasis != bBasis {
			bBit = byte(r.Intn(2))
		}

		aMP := r.Float64() < multiPhotonRate
		bMP := r.Float64() < multiPhotonRate

		aJitter := int64(r.Intn(int(2*jitterPS+1))) - jitterPS
		bJitter := int64(r.Intn(int(2*jitterPS+1))) - jitterPS

		alice = append(alice, qkd.DetectionEvent{TimestampPS: t + aJitter, Basis: aBasis, Bit: aBit, MultiPhoton: aMP})
		bob = append(bob, qkd.DetectionEvent{TimestampPS: t + bJitter, Basis: bBasis, Bit: bBit, MultiPhoton: bMP})
	}
	return alice, bob
}

// matrixNames fixes iteration order; map order is randomized in Go
var matrixNames = []string{"ex1", "ex2", "ex3", "bch"}

var matrices = map[string]qkd.GeneratorMatrix{
	"ex1": qkd.GeneratorMatrixEx1,
	"ex2": qkd.GeneratorMatrixEx2,
	"ex3": qkd.GeneratorMatrixEx3,
	"bch": qkd.GeneratorMatrixBCHt2,
}

type row struct {
	name           string
	blockSize      int
	parityBits     int
	blocksTotal    int
	blocksDropped  int
	discardFrac    float64
	survivingStep2 int
	finalKeyBits   int
	keyRate        float64 // final key bits per raw attempt
	keysPerBatch   int     // whole keyBits-sized keys this batch yields
}

func main() {
	n := flag.Int("n", 300000, "number of raw emission attempts to simulate")
	errRate := flag.Float64("err", 0.03, "intrinsic bit error rate on matched-basis events")
	mpRate := flag.Float64("multiphoton", 0.01, "probability a given click is flagged multi-photon")
	windowPS := flag.Int64("window", 500, "coincidence window in picoseconds")
	jitterPS := flag.Int64("jitter", 300, "simulated detector timing jitter in picoseconds")
	chunkSize := flag.Int("chunk", 2048, "step 3 CRC chunk size, held fixed during the sweep")
	seed := flag.Int64("seed", 1, "PRNG seed for the simulation")
	keyBits := flag.Int("keybits", 256, "key size you'll actually consume, for the keys-per-batch column")
	flag.Parse()

	// one fixed batch reused for every candidate, so the sweep compares
	// matrices on identical data rather than fresh noise each time
	aliceEv, bobEv := simulate(*n, *errRate, *mpRate, *jitterPS, *seed)

	var rows []row
	for _, name := range matrixNames {
		g := matrices[name]
		cfg := qkd.Config{CoincidenceWindowPS: *windowPS, GeneratorMatrix: g, ChunkSize: *chunkSize}
		res, err := qkd.Run(aliceEv, bobEv, cfg)
		if err != nil {
			fmt.Printf("%s: error: %v\n", name, err)
			continue
		}
		bd := res.BlockDetect
		discardFrac := 0.0
		if res.SiftedBits > 0 {
			discardFrac = float64(len(bd.DiscardedAlice)) / float64(res.SiftedBits)
		}
		finalBits := len(res.FinalKeyBits)
		rows = append(rows, row{
			name:           name,
			blockSize:      g.BlockSize(),
			parityBits:     g.ParityBits(),
			blocksTotal:    bd.BlocksTotal,
			blocksDropped:  bd.BlocksDropped,
			discardFrac:    discardFrac,
			survivingStep2: len(bd.SurvivingAlice),
			finalKeyBits:   finalBits,
			keyRate:        float64(finalBits) / float64(*n),
			keysPerBatch:   finalBits / *keyBits,
		})
	}
	if len(rows) == 0 {
		fmt.Println("no candidates produced a result")
		return
	}

	bestRate := 0
	for i, r := range rows {
		if r.keyRate > rows[bestRate].keyRate {
			bestRate = i
		}
	}

	fmt.Printf("matrix | m,p  | blocks(total/dropped) | step2 discard%% | key rate (bits/attempt) | %d-bit keys/batch\n", *keyBits)
	for i, r := range rows {
		mark := ""
		if i == bestRate {
			mark = " [best rate]"
		}
		fmt.Printf("%6s | %d,%-2d | %6d / %-7d | %13.2f%% | %22.5f | %14d%s\n",
			r.name, r.blockSize, r.parityBits, r.blocksTotal, r.blocksDropped, r.discardFrac*100, r.keyRate, r.keysPerBatch, mark)
	}

	best := rows[bestRate]
	fmt.Printf("\nhighest key rate: %s (%.5f bits/attempt, %d %d-bit keys from this batch)\n",
		best.name, best.keyRate, best.keysPerBatch, *keyBits)
}
