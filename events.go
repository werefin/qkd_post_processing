// Package qoj_post_processing implements the BBM92-style classical
// post-processing chain: sifting, block error detection, CRC, QBER, privacy amplification
package qoj_post_processing

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
