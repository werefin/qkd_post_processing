package qkd

import (
	"math/bits"
	"runtime"
	"sync"
)

// Toeplitz hashing as one integer convolution by number-theoretic transform, the approach of Cryptomite
// (Foreman, Yeung, Edgington and Curchod, Quantum 9, 1584 (2025)): O(L log L) against CLMUL's O(n(n+l))

const (
	nttP      = 7<<26 + 1 // prime below 2^30, so lazy values up to 4p fit 32 bits; 2^26 divides p-1
	nttG      = 3         // generates the multiplicative group mod p
	nttMaxLog = 26
)

var nttPInv = func() uint32 {
	// p^-1 mod 2^32 by Newton's iteration, each step doubling the correct low bits
	x := uint32(nttP)
	for range 5 {
		x *= 2 - nttP*x
	}
	return x
}()

// nttRedc is t/2^32 mod p for t < p*2^32 (Montgomery reduction), subtractive so nothing overflows; none of
// these helpers branch on the data, whose comparisons a CPU cannot predict
func nttRedc(t uint64) uint32 {
	q := uint32(t) * nttPInv
	return nttLow(uint32(t>>32) - uint32(uint64(q)*nttP>>32) + nttP)
}

// nttLow takes x < 2p to x mod p, nttLow2 x < 4p to x mod 2p
func nttLow(x uint32) uint32  { return x - nttP + nttP&uint32(int32(x-nttP)>>31) }
func nttLow2(x uint32) uint32 { return x - 2*nttP + 2*nttP&uint32(int32(x-2*nttP)>>31) }

// nttShoup is w*y mod p up to one p, in [0, 2p), for any 32-bit y, with ws = floor(w*2^32/p) (Shoup)
func nttShoup(y, w, ws uint32) uint32 {
	return w*y - uint32(uint64(ws)*uint64(y)>>32)*nttP
}

func nttPow(a uint32, e uint64) uint32 {
	r, b := uint64(1), uint64(a)
	for ; e > 0; e >>= 1 {
		if e&1 == 1 {
			r = r * b % nttP
		}
		b = b * b % nttP
	}
	return uint32(r)
}

// nttTables holds, for every stage half-width h and k < h, w[h+k] = root^(k*L/(2h)) and its Shoup companion,
// forward and inverse, so a stage reads its twiddles in order
type nttTables struct {
	logL               int
	fw, fws, inv, invs []uint32
}

var (
	nttCacheMu sync.Mutex
	nttCache   *nttTables // the last size used: a link's blocks come back at the same size
)

func nttTablesFor(logL int) *nttTables {
	nttCacheMu.Lock()
	defer nttCacheMu.Unlock()
	if nttCache != nil && nttCache.logL == logL {
		return nttCache
	}
	L, half := 1<<logL, 1<<(logL-1)
	t := &nttTables{logL: logL, fw: make([]uint32, L), fws: make([]uint32, L), inv: make([]uint32, L), invs: make([]uint32, L)}
	root := nttPow(nttG, (nttP-1)>>logL)
	rs := uint32((uint64(root) << 32) / nttP)
	t.fw[half] = 1
	for k := 1; k < half; k++ {
		t.fw[half+k] = nttLow(nttShoup(t.fw[half+k-1], root, rs))
	}
	for h := half / 2; h >= 1; h /= 2 {
		for k := range h {
			t.fw[h+k] = t.fw[2*h+2*k]
		}
	}
	for i := 1; i < L; i++ {
		t.fws[i] = uint32((uint64(t.fw[i]) << 32) / nttP)
	}
	// root^-k of a stage is -root^(h-k), and floor((p-w)2^32/p) is the complement of floor(w*2^32/p)
	for h := 1; h < L; h *= 2 {
		t.inv[h], t.invs[h] = 1, t.fws[h]
		for k := 1; k < h; k++ {
			t.inv[h+k], t.invs[h+k] = nttP-t.fw[2*h-k], ^t.fws[2*h-k]
		}
	}
	nttCache = t
	return t
}

// nttBlock is the span, in points, whose stages run together while it sits in the core's cache
const nttBlock = 1 << 13

// nttRun is one transform of a: forward by decimation in frequency (stages from the widest), else inverse by
// decimation in time (from the narrowest); a stage no wider than nttBlock runs block by block in cache
func nttRun(a, w, ws []uint32, forward bool) {
	L := len(a)
	block := min(L, nttBlock)
	workers := runtime.GOMAXPROCS(0)
	// below this the goroutines cost more than they save
	if L < 1<<16 {
		workers = 1
	}
	butterfly := ditButterflies
	if forward {
		butterfly = difButterflies
	}
	// parallel calls f(lo, hi) over [0, n) in one share per worker and waits for them
	parallel := func(n int, f func(lo, hi int)) {
		if workers == 1 {
			f(0, n)
			return
		}
		var wg sync.WaitGroup
		chunk := (n + workers - 1) / workers
		for lo := 0; lo < n; lo += chunk {
			wg.Add(1)
			go func(lo, hi int) {
				defer wg.Done()
				f(lo, hi)
			}(lo, min(lo+chunk, n))
		}
		wg.Wait()
	}
	wide := func(h int) {
		parallel(L/2, func(lo, hi int) { butterfly(a, w, ws, h, lo, hi) })
	}
	local := func() {
		parallel(L/block, func(lo, hi int) {
			for blk := lo; blk < hi; blk++ {
				sub := a[blk*block : (blk+1)*block]
				if forward {
					for h := block / 2; h >= 1; h /= 2 {
						butterfly(sub, w, ws, h, 0, block/2)
					}
				} else {
					for h := 1; h < block; h *= 2 {
						butterfly(sub, w, ws, h, 0, block/2)
					}
				}
			}
		})
	}
	if forward {
		for h := L / 2; h >= block; h /= 2 {
			wide(h)
		}
		local()
		return
	}
	local()
	for h := block; h < L; h *= 2 {
		wide(h)
	}
}

// difButterflies: decimation in frequency, natural order in, bit-reversed out, values kept in [0, 2p) (Harvey)
func difButterflies(a, w, ws []uint32, h, lo, hi int) {
	if h == 1 {
		for b := lo; b < hi; b++ {
			u, v := a[2*b], a[2*b+1]
			a[2*b], a[2*b+1] = nttLow2(u+v), nttLow2(u-v+2*nttP)
		}
		return
	}
	shift := uint(bits.TrailingZeros(uint(h)))
	for b := lo; b < hi; {
		k, start := b&(h-1), (b>>shift)<<(shift+1)
		end := min(h, k+hi-b)
		x, y := a[start+k:start+end], a[start+h+k:start+h+end]
		t, ts := w[h+k:h+end], ws[h+k:h+end]
		y, t, ts = y[:len(x)], t[:len(x)], ts[:len(x)]
		for i := range x {
			u, v := x[i], y[i]
			x[i] = nttLow2(u + v)
			y[i] = nttShoup(u-v+2*nttP, t[i], ts[i])
		}
		b += end - k
	}
}

// ditButterflies: decimation in time, bit-reversed in, natural order out, values kept in [0, 2p)
func ditButterflies(a, w, ws []uint32, h, lo, hi int) {
	if h == 1 {
		for b := lo; b < hi; b++ {
			u, v := a[2*b], a[2*b+1]
			a[2*b], a[2*b+1] = nttLow2(u+v), nttLow2(u-v+2*nttP)
		}
		return
	}
	shift := uint(bits.TrailingZeros(uint(h)))
	for b := lo; b < hi; {
		k, start := b&(h-1), (b>>shift)<<(shift+1)
		end := min(h, k+hi-b)
		x, y := a[start+k:start+end], a[start+h+k:start+h+end]
		t, ts := w[h+k:h+end], ws[h+k:h+end]
		y, t, ts = y[:len(x)], t[:len(x)], ts[:len(x)]
		for i := range x {
			u, v := x[i], nttShoup(y[i], t[i], ts[i])
			x[i] = nttLow2(u + v)
			y[i] = nttLow2(u - v + 2*nttP)
		}
		b += end - k
	}
}

// toeplitzHashNTT: y_i = parity of coefficient i+n-1 of X*S over the integers, one cyclic convolution of
// L >= n+l-1 points, which no wrap-around reaches at those coefficients; each is at most n < p, so exact
func toeplitzHashNTT(x []byte, seed []byte, l int) []byte {
	n := len(x)
	if l == 0 || n == 0 {
		return make([]byte, l)
	}
	logL := max(bits.Len(uint(n+l-2)), 1)
	if logL > nttMaxLog {
		return toeplitzImpl(x, seed, l)
	}
	L := 1 << logL
	tab := nttTablesFor(logL)
	a, s := make([]uint32, L), make([]uint32, L)
	for i, v := range x {
		a[i] = uint32(v & 1)
	}
	for i, v := range seed[:n+l-1] {
		s[i] = uint32(v & 1)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); nttRun(a, tab.fw, tab.fws, true) }()
	go func() { defer wg.Done(); nttRun(s, tab.fw, tab.fws, true) }()
	wg.Wait()
	// the pointwise product and the final scale each leave a factor 2^-32, which scaling by 2^64/L takes out
	for i := range a {
		a[i] = nttRedc(uint64(a[i]) * uint64(s[i]))
	}
	nttRun(a, tab.inv, tab.invs, false)
	scale := uint32((uint64(nttPow(uint32(L), nttP-2)) << 32) % nttP)
	scale = uint32((uint64(scale) << 32) % nttP)
	out := make([]byte, l)
	for i := range out {
		out[i] = byte(nttRedc(uint64(a[i+n-1])*uint64(scale)) & 1)
	}
	return out
}
