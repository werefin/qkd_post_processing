package qkd

import "math/rand"

// simulateClicks builds correlated Alice/Bob click streams at the given intrinsic bit error rate
func simulateClicks(n int, errRate float64, seed int64) (alice, bob []DetectionEvent) {
	r := rand.New(rand.NewSource(seed))
	tcur := int64(0)
	for range n {
		tcur += 1000
		aBasis := Basis(r.Intn(2))
		bBasis := Basis(r.Intn(2))
		aBit := byte(r.Intn(2))
		bBit := aBit
		if aBasis == bBasis && r.Float64() < errRate {
			bBit ^= 1
		} else if aBasis != bBasis {
			bBit = byte(r.Intn(2))
		}
		alice = append(alice, DetectionEvent{TimestampPS: tcur, Basis: aBasis, Bit: aBit})
		bob = append(bob, DetectionEvent{TimestampPS: tcur, Basis: bBasis, Bit: bBit})
	}
	return alice, bob
}
