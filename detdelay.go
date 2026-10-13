package qkd_post_processing

import (
	"fmt"
	"math"
)

// Per-detector delay calibration: unequal detector delays make dt depend on which detector
// clicked, i.e. on the bit, leaking it through announced timing; each party removes its own

// DetectorDelays are per-detector offsets in ps, indexed [basis][bit], each party subtracting
// its own; only differences are physical, the common offset is fixed by drift tracking
type DetectorDelays struct {
	Alice [2][2]float64 `json:"alice_ps"`
	Bob   [2][2]float64 `json:"bob_ps"`
}

// PairPeak is the dt peak of one (basis, Alice bit, Bob bit) class of coincidences
type PairPeak struct {
	Basis            Basis
	AliceBit, BobBit byte
	Count            int
	CentrePS, RMSPS  float64
}

// pairPeaks collects every same-basis coincidence within +-radius and, per class, takes the
// centre and rms inside +-radius of the class's own iteratively re-centred mean
func pairPeaks(alice, bob []DetectionEvent, radiusPS int64) []PairPeak {
	var dts [2][2][2][]float64
	pa, pb := StripBit(alice), StripBit(bob)
	ai, bi := sortedIndices(pa), sortedIndices(pb)
	lo := 0
	for _, a := range ai {
		ea := alice[a]
		if ea.MultiPhoton || ea.Basis > 1 {
			continue
		}
		for lo < len(bi) && bob[bi[lo]].TimestampPS < ea.TimestampPS-2*radiusPS {
			lo++
		}
		for j := lo; j < len(bi); j++ {
			eb := bob[bi[j]]
			d := eb.TimestampPS - ea.TimestampPS
			if d > 2*radiusPS {
				break
			}
			if eb.MultiPhoton || eb.Basis != ea.Basis {
				continue
			}
			dts[ea.Basis][ea.Bit&1][eb.Bit&1] = append(dts[ea.Basis][ea.Bit&1][eb.Bit&1], float64(d))
		}
	}
	var out []PairPeak
	for b := range 2 {
		for x := range 2 {
			for y := range 2 {
				v := dts[b][x][y]
				c := 0.0
				var n, s, s2 float64
				for range 5 {
					n, s, s2 = 0, 0, 0
					for _, d := range v {
						if math.Abs(d-c) <= float64(radiusPS) {
							n++
							s += d
							s2 += d * d
						}
					}
					if n == 0 {
						break
					}
					c = s / n
				}
				p := PairPeak{Basis: Basis(b), AliceBit: byte(x), BobBit: byte(y), Count: int(n), CentrePS: c}
				if n > 1 {
					p.RMSPS = math.Sqrt(math.Max(s2/n-c*c, 0))
				}
				out = append(out, p)
			}
		}
	}
	return out
}

// CalibrateDetectorDelays fits centre(a,b) = bob(b) - alice(a) per basis by weighted least squares;
// it reads detector identities (bit values), so use only a calibration recording never used for key
func CalibrateDetectorDelays(alice, bob []DetectionEvent, radiusPS int64) (DetectorDelays, []PairPeak, error) {
	peaks := pairPeaks(alice, bob, radiusPS)
	var d DetectorDelays
	for b := range 2 {
		// unknowns: alice0, alice1, bob0, bob1; equation per class: bob_y - alice_x = centre
		var jtj [4][4]float64
		var jtr [4]float64
		classes := 0
		for _, p := range peaks {
			if int(p.Basis) != b || p.Count < 10 {
				continue
			}
			classes++
			w := float64(p.Count) / math.Max(p.RMSPS*p.RMSPS, 1)
			row := [4]float64{}
			row[p.AliceBit] = -1
			row[2+p.BobBit] = 1
			for i := range 4 {
				jtr[i] += w * row[i] * p.CentrePS
				for k := range 4 {
					jtj[i][k] += w * row[i] * row[k]
				}
			}
		}
		if classes < 3 {
			return d, peaks, fmt.Errorf("detector delays: basis %d has only %d bit classes with >=10 coincidences, need 3", b, classes)
		}
		// gauge: the four delays sum to 0, added as a heavily weighted extra equation
		g := 0.0
		for i := range 4 {
			for k := range 4 {
				g = math.Max(g, jtj[i][k])
			}
		}
		for i := range 4 {
			for k := range 4 {
				jtj[i][k] += g
			}
		}
		x, ok := solve4(jtj, jtr)
		if !ok {
			return d, peaks, fmt.Errorf("detector delays: singular system for basis %d", b)
		}
		d.Alice[b] = [2]float64{x[0], x[1]}
		d.Bob[b] = [2]float64{x[2], x[3]}
	}
	return d, peaks, nil
}

// ApplyDetectorDelays returns events shifted by -delays[basis][bit]: each party applies its own,
// before announcing timestamps, so dt no longer depends on which detector clicked
func ApplyDetectorDelays(events []DetectionEvent, delays [2][2]float64) []DetectionEvent {
	out := make([]DetectionEvent, len(events))
	for i, e := range events {
		if e.Basis <= 1 {
			e.TimestampPS -= int64(math.Round(delays[e.Basis][e.Bit&1]))
		}
		out[i] = e
	}
	return out
}

// TimingLeakBits estimates I(Alice's bit; dt) per sifted bit from the per-class Gaussians, the
// information announced timestamps give away about the key if detector delays differ
func TimingLeakBits(peaks []PairPeak) float64 {
	var total, weighted float64
	for b := range 2 {
		// per Alice bit, dt is a mixture over Bob's bit of that class's Gaussian
		var comp [2][]PairPeak
		var nBit [2]float64
		for _, p := range peaks {
			if int(p.Basis) == b && p.Count > 0 && p.RMSPS > 0 {
				comp[p.AliceBit] = append(comp[p.AliceBit], p)
				nBit[p.AliceBit] += float64(p.Count)
			}
		}
		n := nBit[0] + nBit[1]
		if nBit[0] == 0 || nBit[1] == 0 {
			continue
		}
		density := func(x float64, cs []PairPeak, norm float64) float64 {
			var f float64
			for _, c := range cs {
				z := (x - c.CentrePS) / c.RMSPS
				f += float64(c.Count) / norm * math.Exp(-z*z/2) / (c.RMSPS * math.Sqrt(2*math.Pi))
			}
			return f
		}
		// I = sum_a P(a) integral f(x|a) log2(f(x|a)/f(x)) dx, by the trapezoid rule
		var mi float64
		const step = 0.5
		for x := -400.0; x <= 400; x += step {
			f0, f1 := density(x, comp[0], nBit[0]), density(x, comp[1], nBit[1])
			fm := (nBit[0]*f0 + nBit[1]*f1) / n
			for a, fa := range [2]float64{f0, f1} {
				if fa > 0 && fm > 0 {
					mi += nBit[a] / n * fa * math.Log2(fa/fm) * step
				}
			}
		}
		total += n
		weighted += n * mi
	}
	if total == 0 {
		return 0
	}
	return weighted / total
}
