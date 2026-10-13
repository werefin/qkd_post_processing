// Multi-hop key relay: each hop re-encrypts with a fresh OTP and a fresh
// polynomial MAC (polymac.go), both QKD-derived and single-use
package qkd_post_processing

import (
	"fmt"
	"time"
)

// defaultRelayTTL bounds how long a delivered key waits unconsumed locally
const defaultRelayTTL = 24 * time.Hour

// RelayXOR one-time-pads payload with pad; same length required, always
func RelayXOR(payload, pad []byte) ([]byte, error) {
	if len(payload) != len(pad) {
		return nil, fmt.Errorf("relay: payload is %d bytes, pad is %d: OTP requires exact length match", len(payload), len(pad))
	}
	out := make([]byte, len(payload))
	for i := range payload {
		out[i] = payload[i] ^ pad[i]
	}
	return out, nil
}

// RelayNode is one network node: a local delivery store, one KMS pool per
// neighbor (that link's QKD output), and a destination-to-neighbor route table
type RelayNode struct {
	ID       string
	Local    *KMS
	HopPools map[string]*KMS
	NextHop  map[string]string
}

// NewRelayNode builds a node from its local store, neighbor pools, and routes
func NewRelayNode(id string, local *KMS, hopPools map[string]*KMS, routes map[string]string) *RelayNode {
	return &RelayNode{ID: id, Local: local, HopPools: hopPools, NextHop: routes}
}

// RelayedPacket is what one hop sends the next: ciphertext and tag, plus
// key_ID references to the pad/MAC key -- never the key material itself
type RelayedPacket struct {
	Dest      string   `json:"dest"`
	FromHop   string   `json:"from_hop"`
	PadKeyIDs []string `json:"pad_key_ids"`
	MACKeyIDs []string `json:"mac_key_ids"`
	Cipher    []byte   `json:"cipher"`
	Tag       []byte   `json:"tag"`
}

// macMessage is the canonical, length-prefixed encoding the MAC covers --
// everything an attacker could tamper with, not just the ciphertext bytes
func macMessage(dest, fromHop string, padKeyIDs []string, cipher []byte) []byte {
	var buf []byte
	field := func(s string) {
		buf = append(buf, byte(len(s)))
		buf = append(buf, s...)
	}
	field(dest)
	field(fromHop)
	buf = append(buf, byte(len(padKeyIDs)))
	for _, id := range padKeyIDs {
		field(id)
	}
	return append(buf, cipher...)
}

// Originate encrypts and authenticates keyBytes for the first hop toward dest
func (n *RelayNode) Originate(dest string, keyBytes []byte) (RelayedPacket, error) {
	hop, ok := n.NextHop[dest]
	if !ok {
		return RelayedPacket{}, fmt.Errorf("relay: no route to %s", dest)
	}
	return n.forward(hop, dest, keyBytes)
}

// forward draws a fresh pad and a fresh MAC key from the hop's pool,
// encrypts, then authenticates the ciphertext plus its own routing metadata
func (n *RelayNode) forward(hop, dest string, keyBytes []byte) (RelayedPacket, error) {
	pool, ok := n.HopPools[hop]
	if !ok {
		return RelayedPacket{}, fmt.Errorf("relay: no key pool shared with %s", hop)
	}

	pad, padIDs, err := pool.GetNextBytes(len(keyBytes))
	if err != nil {
		return RelayedPacket{}, fmt.Errorf("relay: pad: %w", err)
	}
	cipher, err := RelayXOR(keyBytes, pad)
	if err != nil {
		return RelayedPacket{}, err
	}

	msg := macMessage(dest, n.ID, padIDs, cipher)
	macKey, macIDs, err := pool.GetNextBytes(PolyMACKeyBytes)
	if err != nil {
		return RelayedPacket{}, fmt.Errorf("relay: mac key: %w", err)
	}
	tag, err := ComputePolyMAC(msg, macKey)
	if err != nil {
		return RelayedPacket{}, fmt.Errorf("relay: mac: %w", err)
	}

	return RelayedPacket{Dest: dest, FromHop: n.ID, PadKeyIDs: padIDs, MACKeyIDs: macIDs, Cipher: cipher, Tag: tag}, nil
}

// Receive verifies the packet's MAC before touching the ciphertext at all
// (fail closed: an invalid tag is refused, never decrypted), then decrypts
func (n *RelayNode) Receive(pkt RelayedPacket) ([]byte, error) {
	pool, ok := n.HopPools[pkt.FromHop]
	if !ok {
		return nil, fmt.Errorf("relay: no key pool shared with %s", pkt.FromHop)
	}

	msg := macMessage(pkt.Dest, pkt.FromHop, pkt.PadKeyIDs, pkt.Cipher)
	macKey, err := pool.GetBytes(pkt.MACKeyIDs, PolyMACKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("relay: mac key: %w", err)
	}
	if !VerifyPolyMAC(msg, pkt.Tag, macKey) {
		return nil, fmt.Errorf("relay: authentication failed -- packet tampered with or corrupted, discarding")
	}

	pad, err := pool.GetBytes(pkt.PadKeyIDs, len(pkt.Cipher))
	if err != nil {
		return nil, fmt.Errorf("relay: pad: %w", err)
	}
	return RelayXOR(pkt.Cipher, pad)
}

// Handle is the full receive step: authenticate, decrypt, then deliver
// locally (if this node is the destination) or re-encrypt toward the next hop
func (n *RelayNode) Handle(pkt RelayedPacket) (delivered bool, keyID string, next RelayedPacket, err error) {
	keyBytes, err := n.Receive(pkt)
	if err != nil {
		return false, "", RelayedPacket{}, err
	}
	if pkt.Dest == n.ID {
		keyID, err = n.Local.Put(keyBytes, defaultRelayTTL)
		return true, keyID, RelayedPacket{}, err
	}
	hop, ok := n.NextHop[pkt.Dest]
	if !ok {
		return false, "", RelayedPacket{}, fmt.Errorf("relay: no route from %s to %s", n.ID, pkt.Dest)
	}
	next, err = n.forward(hop, pkt.Dest, keyBytes)
	return false, "", next, err
}
