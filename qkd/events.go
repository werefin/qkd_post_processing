// Package qkd implements the BBM92 classical post-processing chain: sifting, coincidence
// filtering, error correction, verification, finite-key privacy amplification
package qkd

// Basis identifies which measurement basis a detector click was recorded in
// each party randomly picks one of two bases per detection event
type Basis uint8

// DetectionEvent represents a single click on one party's detector
type DetectionEvent struct {
	TimestampPS int64 // detection time in picoseconds (or any fixed unit)
	Basis       Basis
	Bit         byte // measured outcome, 0 or 1
	MultiPhoton bool // detector logic flagged an ambiguous multi-photon event
}
