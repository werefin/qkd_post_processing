// Multi-hop key relay: each hop re-encrypts with a fresh OTP and a fresh
// polynomial MAC (polymac.go), both QKD-derived and single-use
package relay

import (
	"encoding/binary"
	"fmt"
	"time"

	"qkd_post_processing/kms"
)

// defaultRelayTTL bounds how long a delivered key waits for its slave SAE
var defaultRelayTTL = 24 * time.Hour

// SetDeliveryTTL sets how long relayed keys wait at their destination for the slave SAE
func SetDeliveryTTL(d time.Duration) { defaultRelayTTL = d }

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
	Local    *kms.KMS
	HopPools map[string]*kms.KMS
	NextHop  map[string]string
}

// NewRelayNode builds a node from its local store, neighbor pools, and routes
func NewRelayNode(id string, local *kms.KMS, hopPools map[string]*kms.KMS, routes map[string]string) *RelayNode {
	return &RelayNode{ID: id, Local: local, HopPools: hopPools, NextHop: routes}
}

// RelayedPacket is what one hop sends the next: ciphertext and tag, key_ID references to the
// pad and MAC keys (never key material), and the end-to-end key_ID and SAE pair it is reserved for
type RelayedPacket struct {
	Dest      string   `json:"dest"`
	FromHop   string   `json:"from_hop"`
	Origin    string   `json:"origin"`
	KeyID     string   `json:"key_ID"`
	MasterSAE string   `json:"master_SAE_ID"`
	SlaveSAE  string   `json:"slave_SAE_ID"`
	PadKeyIDs []string `json:"pad_key_ids"`
	MACKeyIDs []string `json:"mac_key_ids"`
	// PayloadKeyIDs, first hop only: the key is these pool keys of the sender's half, which the
	// receiver holds too, so they travel as IDs and need no pad
	PayloadKeyIDs []string `json:"payload_key_ids,omitempty"`
	Cipher        []byte   `json:"cipher"`
	Tag           []byte   `json:"tag"`
}

// macMessage is the canonical encoding the MAC covers: every field an attacker could change,
// each length-prefixed (uvarint) so no two packets share an encoding
func macMessage(p RelayedPacket) []byte {
	var buf []byte
	field := func(b []byte) {
		buf = binary.AppendUvarint(buf, uint64(len(b)))
		buf = append(buf, b...)
	}
	for _, s := range []string{p.Dest, p.FromHop, p.Origin, p.KeyID, p.MasterSAE, p.SlaveSAE} {
		field([]byte(s))
	}
	for _, ids := range [][]string{p.PadKeyIDs, p.PayloadKeyIDs} {
		buf = binary.AppendUvarint(buf, uint64(len(ids)))
		for _, id := range ids {
			field([]byte(id))
		}
	}
	field(p.Cipher)
	return buf
}

// Originate encrypts and authenticates keyBytes for the first hop toward dest, where it will be
// stored as keyID for the SAE pair master/slave only
func (n *RelayNode) Originate(dest, keyID, master, slave string, keyBytes []byte) (RelayedPacket, error) {
	hop, ok := n.NextHop[dest]
	if !ok {
		return RelayedPacket{}, fmt.Errorf("relay: no route to %s", dest)
	}
	return n.forward(hop, RelayedPacket{Dest: dest, Origin: n.ID, KeyID: keyID, MasterSAE: master, SlaveSAE: slave}, keyBytes)
}

// OriginateFromPool makes a key of size bytes for keyID from this node's half of the pool it
// shares with the first hop toward dest, QKD output end to end; it returns the packet and the key
func (n *RelayNode) OriginateFromPool(dest, keyID, master, slave string, size int) (RelayedPacket, []byte, error) {
	hop, ok := n.NextHop[dest]
	if !ok {
		return RelayedPacket{}, nil, fmt.Errorf("relay: no route to %s", dest)
	}
	pool, ok := n.HopPools[hop]
	if !ok {
		return RelayedPacket{}, nil, fmt.Errorf("relay: no key pool shared with %s", hop)
	}
	if size <= 0 || size%kms.ETSI014KeySizeBytes != 0 {
		return RelayedPacket{}, nil, fmt.Errorf("relay: key size must be a positive multiple of %d bytes", kms.ETSI014KeySizeBytes)
	}
	material, ids, err := pool.GetNextBytesMatching(size+PolyMACKeyBytes, kms.OriginatorOf(n.ID, hop))
	if err != nil {
		return RelayedPacket{}, nil, fmt.Errorf("relay: key and mac key: %w", err)
	}
	keyIDs, macIDs, err := splitIDs(ids, size)
	if err != nil {
		clear(material)
		return RelayedPacket{}, nil, err
	}
	pkt := RelayedPacket{Dest: dest, FromHop: n.ID, Origin: n.ID, KeyID: keyID, MasterSAE: master, SlaveSAE: slave,
		MACKeyIDs: macIDs, PayloadKeyIDs: keyIDs}
	pkt.Tag, err = ComputePolyMAC(macMessage(pkt), material[size:])
	clear(material[size:])
	if err != nil {
		clear(material)
		return RelayedPacket{}, nil, err
	}
	return pkt, material[:size], nil
}

// forward draws a fresh pad and a fresh MAC key from this node's own half of the hop's pool,
// encrypts, then authenticates the ciphertext with all of the packet's metadata
func (n *RelayNode) forward(hop string, meta RelayedPacket, keyBytes []byte) (RelayedPacket, error) {
	pool, ok := n.HopPools[hop]
	if !ok {
		return RelayedPacket{}, fmt.Errorf("relay: no key pool shared with %s", hop)
	}
	mine := kms.OriginatorOf(n.ID, hop)

	// one draw for pad and MAC key together, so a short pool consumes nothing
	need := len(keyBytes) + PolyMACKeyBytes
	material, ids, err := pool.GetNextBytesMatching(need, mine)
	if err != nil {
		return RelayedPacket{}, fmt.Errorf("relay: pad and mac key: %w", err)
	}
	defer clear(material)
	padIDs, macIDs, err := splitIDs(ids, len(keyBytes))
	if err != nil {
		return RelayedPacket{}, err
	}
	pad, macKey := material[:len(keyBytes)], material[len(keyBytes):]
	cipher, err := RelayXOR(keyBytes, pad)
	if err != nil {
		return RelayedPacket{}, err
	}

	pkt := meta
	pkt.FromHop, pkt.PadKeyIDs, pkt.MACKeyIDs, pkt.Cipher, pkt.PayloadKeyIDs = n.ID, padIDs, macIDs, cipher, nil
	if pkt.Tag, err = ComputePolyMAC(macMessage(pkt), macKey); err != nil {
		return RelayedPacket{}, fmt.Errorf("relay: mac: %w", err)
	}
	return pkt, nil
}

// splitIDs assigns whole pool keys to the pad and the MAC key; the pool's keys are all the
// same size, so the pad takes the first ceil(padBytes/keySize) of them
func splitIDs(ids []string, padBytes int) (padIDs, macIDs []string, err error) {
	keySize := kms.ETSI014KeySizeBytes
	nPad := (padBytes + keySize - 1) / keySize
	if nPad >= len(ids) || padBytes%keySize != 0 {
		return nil, nil, fmt.Errorf("relay: payload must be a positive multiple of %d bytes, got %d", keySize, padBytes)
	}
	return ids[:nPad], ids[nPad:], nil
}

// Receive verifies the packet's MAC before touching the ciphertext at all (fail closed: an
// invalid tag is refused, never decrypted), then decrypts
func (n *RelayNode) Receive(pkt RelayedPacket) ([]byte, error) {
	pool, ok := n.HopPools[pkt.FromHop]
	if !ok {
		return nil, fmt.Errorf("relay: no key pool shared with %s", pkt.FromHop)
	}
	// keys from this node's own half could already have padded something it sent: refuse them
	theirs := kms.OriginatorOf(pkt.FromHop, n.ID)
	if len(pkt.PayloadKeyIDs) > 0 && (pkt.Origin != pkt.FromHop || len(pkt.PadKeyIDs) > 0 || len(pkt.Cipher) > 0) {
		return nil, fmt.Errorf("relay: a key sent by IDs must come straight from its origin, without a pad")
	}
	for _, id := range append(append(append([]string{}, pkt.PadKeyIDs...), pkt.MACKeyIDs...), pkt.PayloadKeyIDs...) {
		if !theirs(id) {
			return nil, fmt.Errorf("relay: %s used key %s from this node's half of the pool", pkt.FromHop, id)
		}
	}

	macKey, err := pool.GetBytes(pkt.MACKeyIDs, PolyMACKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("relay: mac key: %w", err)
	}
	defer clear(macKey)
	if !VerifyPolyMAC(macMessage(pkt), pkt.Tag, macKey) {
		return nil, fmt.Errorf("relay: authentication failed -- packet tampered with or corrupted, discarding")
	}

	if len(pkt.PayloadKeyIDs) > 0 {
		key, err := pool.GetBytes(pkt.PayloadKeyIDs, len(pkt.PayloadKeyIDs)*kms.ETSI014KeySizeBytes)
		if err != nil {
			return nil, fmt.Errorf("relay: key: %w", err)
		}
		return key, nil
	}
	pad, err := pool.GetBytes(pkt.PadKeyIDs, len(pkt.Cipher))
	if err != nil {
		return nil, fmt.Errorf("relay: pad: %w", err)
	}
	defer clear(pad)
	return RelayXOR(pkt.Cipher, pad)
}

// Handle is the full receive step: authenticate, decrypt, then store the key reserved for its SAE
// pair (if this node is the destination) or re-encrypt toward the next hop
func (n *RelayNode) Handle(pkt RelayedPacket) (delivered bool, keyID string, next RelayedPacket, err error) {
	keyBytes, err := n.Receive(pkt)
	if err != nil {
		return false, "", RelayedPacket{}, err
	}
	defer clear(keyBytes)
	if pkt.Dest == n.ID {
		err = n.Local.PutReserved(pkt.KeyID, keyBytes, pkt.MasterSAE, pkt.SlaveSAE, defaultRelayTTL)
		return true, pkt.KeyID, RelayedPacket{}, err
	}
	hop, ok := n.NextHop[pkt.Dest]
	if !ok {
		return false, "", RelayedPacket{}, fmt.Errorf("relay: no route from %s to %s", n.ID, pkt.Dest)
	}
	next, err = n.forward(hop, pkt, keyBytes)
	return false, "", next, err
}
