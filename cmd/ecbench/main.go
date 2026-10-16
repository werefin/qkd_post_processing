// ecbench: Go error-correction methods on a simulated binary symmetric channel, per QBER:
// efficiency, block failures, residual errors, speed and secret fraction (scripts/ec_compare.py adds LDPC)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"qkd_post_processing/qkd"
)

type row struct {
	Method      string  `json:"method"`
	QBER        float64 `json:"qber"`
	Efficiency  float64 `json:"efficiency"` // disclosed bits over n*h2(QBER)
	BlockFail   float64 `json:"block_fail"` // blocks left with any error after reconciliation
	ResidualBER float64 `json:"residual_ber"`
	Mbps        float64 `json:"mbit_per_s"`
	Secret      float64 `json:"secret_fraction"` // (1-fail)(1-h2) - f*h2, per sifted bit, asymptotic
	Blocks      int     `json:"blocks"`
}

func h2(p float64) float64 {
	if p <= 0 || p >= 1 {
		return 0
	}
	return -p*math.Log2(p) - (1-p)*math.Log2(1-p)
}

func bsc(n int, q float64, seed int64) (a, b []byte, rates []float64) {
	r := rand.New(rand.NewSource(seed))
	a, b, rates = make([]byte, n), make([]byte, n), make([]float64, n)
	for i := range a {
		a[i] = byte(r.Intn(2))
		b[i] = a[i]
		if r.Float64() < q {
			b[i] ^= 1
		}
		rates[i] = q
	}
	return
}

// parallelBlocks runs fn over whole blocks of length block on every core, so all methods are timed
// on the same hardware use; fn returns the corrected slice and the bits it disclosed
func parallelBlocks(a, b []byte, q []float64, block int, fn func(a, b []byte, q []float64) ([]byte, int)) ([]byte, int) {
	out := make([]byte, len(b))
	blocks := (len(a) + block - 1) / block
	workers := min(runtime.NumCPU(), blocks)
	per := (blocks + workers - 1) / workers
	leaked := make([]int, workers)
	var wg sync.WaitGroup
	for w := range workers {
		lo, hi := w*per*block, min((w+1)*per*block, len(a))
		if lo >= hi {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, l := fn(a[lo:hi], b[lo:hi], q[lo:hi])
			copy(out[lo:hi], c)
			leaked[w] = l
		}()
	}
	wg.Wait()
	total := 0
	for _, l := range leaked {
		total += l
	}
	return out, total
}

type method struct {
	name  string
	block int // block length for counting failures, the method's own frame or block length
	run   func(a, b []byte, q []float64, qber float64) ([]byte, int, int, error)
}

func methods() []method {
	polar := func(n, list int) method {
		return method{fmt.Sprintf("polar %s N=2^%d", map[bool]string{true: "SC", false: fmt.Sprintf("SCL%d", list)}[list == 1], int(math.Log2(float64(n)))), n,
			func(a, b []byte, q []float64, _ float64) ([]byte, int, int, error) {
				var err error
				out, leak := parallelBlocks(a, b, q, n, func(a, b []byte, q []float64) ([]byte, int) {
					c, _, _, l, e := qkd.PolarReconcileSoft(a, b, q, n, 0.02, qkd.PolarDecodeConfig{ListSize: list})
					if e != nil {
						err = e
					}
					return c, l
				})
				return out, leak, n, err
			}}
	}
	regular := method{"LDPC regular (3,wr) n~2^14", 0, func(a, b []byte, q []float64, qber float64) ([]byte, int, int, error) {
		// rate set for 1.25 times the Shannon limit, the usual margin for a fixed-rate regular code
		wr := max(4, int(math.Round(3/(1.25*h2(qber)))))
		n := wr * (16384 / wr)
		h, err := qkd.BuildRegularLDPC(n, 3, wr, 7)
		if err != nil {
			return nil, 0, 0, err
		}
		out, leak := parallelBlocks(a, b, q, n, func(a, b []byte, _ []float64) ([]byte, int) {
			c, _, _, l := qkd.LDPCReconcile(h, a, b, qber, qkd.LDPCDecodeConfig{MaxIterations: 100})
			return c, l
		})
		return out, leak, n, nil
	}}
	return []method{polar(1<<14, 1), polar(1<<16, 1), polar(1<<16, 8), polar(1<<16, 32), polar(1<<20, 8), regular}
}

func main() {
	bits := flag.Int("bits", 1<<20, "sifted bits per QBER point")
	qs := flag.String("q", "0.01,0.02,0.03,0.05,0.07,0.09,0.11", "QBER points")
	only := flag.String("methods", "", "comma-separated substrings selecting methods, empty = all")
	jsonOut := flag.Bool("json", false, "one JSON array instead of the table")
	flag.Parse()

	var rows []row
	for _, qs := range strings.Split(*qs, ",") {
		qber, err := strconv.ParseFloat(strings.TrimSpace(qs), 64)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		a, b, rates := bsc(*bits, qber, int64(qber*1e6))
		for _, m := range methods() {
			if *only != "" && !matches(m.name, *only) {
				continue
			}
			t0 := time.Now()
			out, leak, block, err := m.run(a, b, rates, qber)
			el := time.Since(t0)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s at %.2f: %v\n", m.name, qber, err)
				continue
			}
			blocks, failed, wrong := 0, 0, 0
			for lo := 0; lo < len(a); lo += block {
				hi := min(lo+block, len(a))
				blocks++
				bad := false
				for i := lo; i < hi; i++ {
					if out[i] != a[i] {
						wrong++
						bad = true
					}
				}
				if bad {
					failed++
				}
			}
			h := h2(qber)
			f := float64(leak) / (float64(len(a)) * h)
			fail := float64(failed) / float64(blocks)
			r := row{Method: m.name, QBER: qber, Efficiency: f, BlockFail: fail, ResidualBER: float64(wrong) / float64(len(a)),
				Mbps: float64(len(a)) / el.Seconds() / 1e6, Secret: max(0, (1-fail)*(1-h)-f*h), Blocks: blocks}
			rows = append(rows, r)
			if !*jsonOut {
				fmt.Fprintf(os.Stderr, "%-28s q=%.2f  f=%.3f  fail=%.3f  BER=%.1e  %.2f Mbit/s  secret=%.4f  (%d blocks)\n",
					r.Method, r.QBER, r.Efficiency, r.BlockFail, r.ResidualBER, r.Mbps, r.Secret, r.Blocks)
			}
		}
	}
	if *jsonOut {
		json.NewEncoder(os.Stdout).Encode(rows)
	}
}

func matches(name, spec string) bool {
	for _, s := range strings.Split(spec, ",") {
		if strings.Contains(strings.ToLower(name), strings.ToLower(strings.TrimSpace(s))) {
			return true
		}
	}
	return false
}
