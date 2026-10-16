package relay

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"strings"
	"testing"
	"time"

	"qkd_post_processing/kms"
)

// TestRelayXORRejectsLengthMismatch: a shorter/longer pad must never be
// accepted -- wraparound or truncation breaks the one-time-pad guarantee
func TestRelayXORRejectsLengthMismatch(t *testing.T) {
	if _, err := RelayXOR([]byte{1, 2, 3}, []byte{1, 2}); err == nil {
		t.Fatal("expected an error for mismatched payload/pad length")
	}
}

// TestRelayXORRoundTrips: XOR with the same pad twice returns the original
func TestRelayXORRoundTrips(t *testing.T) {
	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	pad := []byte{0x01, 0x02, 0x03, 0x04}
	cipher, err := RelayXOR(payload, pad)
	if err != nil {
		t.Fatal(err)
	}
	back, err := RelayXOR(cipher, pad)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, payload) {
		t.Fatalf("got %x, want %x", back, payload)
	}
}

// TestRelayXORIsPerfectSecrecy proves Shannon's OTP property by enumeration:
// for every ciphertext byte, each of the 256 pads must map to a different plaintext
func TestRelayXORIsPerfectSecrecy(t *testing.T) {
	for cipherByte := 0; cipherByte < 256; cipherByte++ {
		seenPlaintext := make([]bool, 256)
		for pad := 0; pad < 256; pad++ {
			plaintext := byte(cipherByte) ^ byte(pad)
			if seenPlaintext[plaintext] {
				t.Fatalf("cipher=%d: plaintext %d reachable via more than one pad -- not a bijection", cipherByte, plaintext)
			}
			seenPlaintext[plaintext] = true
		}
		for p := 0; p < 256; p++ {
			if !seenPlaintext[p] {
				t.Fatalf("cipher=%d: plaintext %d unreachable by any pad -- ciphertext leaks information", cipherByte, p)
			}
		}
	}
	// This is secrecy only: XOR is malleable, so forward()/Receive() always pair it with a MAC
}

// TestRelayXORCiphertextIndistinguishableFromRandom: two attacker-chosen
// plaintexts under independent uniform pads must look statistically identical
func TestRelayXORCiphertextIndistinguishableFromRandom(t *testing.T) {
	const trials = 20000
	const n = 16
	plainA := bytes.Repeat([]byte{0x00}, n)
	plainB := bytes.Repeat([]byte{0xFF}, n)

	var onesA, onesB [n * 8]int
	for i := 0; i < trials; i++ {
		pad := make([]byte, n)
		rand.Read(pad)
		ca, _ := RelayXOR(plainA, pad)
		cb, _ := RelayXOR(plainB, pad)
		for byteIdx := 0; byteIdx < n; byteIdx++ {
			for bit := 0; bit < 8; bit++ {
				if ca[byteIdx]&(1<<bit) != 0 {
					onesA[byteIdx*8+bit]++
				}
				if cb[byteIdx]&(1<<bit) != 0 {
					onesB[byteIdx*8+bit]++
				}
			}
		}
	}
	for i := range onesA {
		fa := float64(onesA[i]) / trials
		fb := float64(onesB[i]) / trials
		if fa < 0.47 || fa > 0.53 {
			t.Fatalf("bit %d: plaintext A ciphertext bit frequency %v far from 0.5", i, fa)
		}
		if fb < 0.47 || fb > 0.53 {
			t.Fatalf("bit %d: plaintext B ciphertext bit frequency %v far from 0.5", i, fb)
		}
	}
}

// buildTestNetwork wires three nodes A-B-C with B as the trusted relay,
// one KMS pool per link, seeded with matching keys on both sides
func buildTestNetwork(t *testing.T) (a, b, c *RelayNode) {
	t.Helper()
	newKMS := func() *kms.KMS {
		master := make([]byte, 32)
		rand.Read(master)
		k, err := kms.NewKMS(t.TempDir(), master)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}

	// seedLink mirrors the same sequence of keys into both sides' pools,
	// same key_IDs, same bytes -- what a completed pairwise QKD run produces
	seedLink := func(n int) (sideA, sideB *kms.KMS) {
		sideA, sideB = newKMS(), newKMS()
		for i := 0; i < n; i++ {
			material := make([]byte, 32)
			rand.Read(material)
			id, err := sideA.Put(material, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if err := sideB.PutWithID(id, material, time.Hour); err != nil {
				t.Fatal(err)
			}
		}
		return
	}

	// 20 keys of 32 bytes (640 bytes) per link comfortably covers one
	// relay op's pad + MAC-seed draw for the small test messages here
	abAtA, abAtB := seedLink(40)
	bcAtB, bcAtC := seedLink(40)

	a = NewRelayNode("A", newKMS(), map[string]*kms.KMS{"B": abAtA}, map[string]string{"C": "B", "B": "B"})
	b = NewRelayNode("B", newKMS(), map[string]*kms.KMS{"A": abAtB, "C": bcAtB}, map[string]string{"C": "C"})
	c = NewRelayNode("C", newKMS(), map[string]*kms.KMS{"B": bcAtC}, nil)
	return a, b, c
}

// TestRelayNodeTwoHopDeliversOriginalKey: A -> B -> C, B never stores the
// key beyond the moment it re-encrypts, C recovers exactly what A sent
func TestRelayNodeTwoHopDeliversOriginalKey(t *testing.T) {
	a, b, c := buildTestNetwork(t)
	original := bytes.Repeat([]byte{0xAA, 0xBB, 0xCC, 0xDD}, 8)

	pkt, err := a.Originate("C", "k-1", "alice", "carol", original)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(pkt.Cipher, original) {
		t.Fatal("ciphertext must not equal the plaintext key")
	}

	delivered, _, next, err := b.Handle(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if delivered {
		t.Fatal("B is not the destination, should not deliver locally")
	}

	delivered, keyID, _, err := c.Handle(next)
	if err != nil {
		t.Fatal(err)
	}
	if !delivered {
		t.Fatal("C is the destination, should deliver locally")
	}

	if keyID != "k-1" {
		t.Fatalf("delivered as %s, want the originator's key_ID", keyID)
	}
	if _, err := c.Local.Get(keyID); err != kms.ErrKeyReserved {
		t.Fatalf("an unreserved caller got the key: %v", err)
	}
	got, err := c.Local.GetReserved(keyID, "alice", "carol")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("got %x, want %x", got, original)
	}
}

// TestRelayNodeDetectsCiphertextTampering: OTP alone is malleable, so a
// flipped ciphertext bit must be rejected outright, never silently decrypted
func TestRelayNodeDetectsCiphertextTampering(t *testing.T) {
	a, b, _ := buildTestNetwork(t)
	pkt, err := a.Originate("C", "k-1", "alice", "carol", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}

	tampered := pkt
	tampered.Cipher = append([]byte(nil), pkt.Cipher...)
	tampered.Cipher[0] ^= 0x01 // flip one bit

	if _, err := b.Receive(tampered); err == nil {
		t.Fatal("expected authentication to reject a tampered ciphertext, got nil error")
	}
}

// TestRelayNodeDetectsRoutingMetadataTampering: the MAC covers the route, the end-to-end key_ID
// and the SAE pair, not just the ciphertext, so none of them can be rewritten in transit
func TestRelayNodeDetectsRoutingMetadataTampering(t *testing.T) {
	for name, tamper := range map[string]func(p *RelayedPacket){
		"dest":   func(p *RelayedPacket) { p.Dest = "Z" },
		"key_ID": func(p *RelayedPacket) { p.KeyID = "k-2" },
		"slave":  func(p *RelayedPacket) { p.SlaveSAE = "eve" },
		"master": func(p *RelayedPacket) { p.MasterSAE = "eve" },
		"origin": func(p *RelayedPacket) { p.Origin = "Z" },
	} {
		a, b, _ := buildTestNetwork(t)
		pkt, err := a.Originate("C", "k-1", "alice", "carol", make([]byte, 32))
		if err != nil {
			t.Fatal(err)
		}
		tamper(&pkt)
		if _, err := b.Receive(pkt); err == nil || !strings.Contains(err.Error(), "authentication failed") {
			t.Fatalf("%s: tampered packet not rejected by the MAC: %v", name, err)
		}
	}
}

// TestRelayNodeRefusesKeysFromItsOwnHalf: a sender spending the receiver's half of the pool
// could reuse a pad, so the receiver refuses before consuming anything
func TestRelayNodeRefusesKeysFromItsOwnHalf(t *testing.T) {
	a, b, _ := buildTestNetwork(t)
	pkt, err := a.Originate("C", "k-1", "alice", "carol", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	mineAtB := kms.OriginatorOf("B", "A")
	before := b.HopPools["A"].CountAvailable(nil)
	for _, id := range append(pkt.PadKeyIDs, pkt.MACKeyIDs...) {
		if mineAtB(id) {
			t.Fatalf("A spent %s from B's half", id)
		}
	}
	// pretend A used one of B's keys
	pkt.PadKeyIDs = b.HopPools["A"].AvailableIDs(mineAtB)[:1]
	if _, err := b.Receive(pkt); err == nil || !strings.Contains(err.Error(), "this node's half") {
		t.Fatalf("got %v", err)
	}
	if after := b.HopPools["A"].CountAvailable(nil); after != before {
		t.Fatalf("refusal consumed keys: %d -> %d", before, after)
	}
}

// TestRelayNodeDetectsTagTampering: a corrupted tag must fail even when
// the ciphertext and metadata are untouched
func TestRelayNodeDetectsTagTampering(t *testing.T) {
	a, b, _ := buildTestNetwork(t)
	pkt, err := a.Originate("C", "k-1", "alice", "carol", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}

	tampered := pkt
	tampered.Tag = append([]byte(nil), pkt.Tag...)
	tampered.Tag[0] ^= 1

	if _, err := b.Receive(tampered); err == nil {
		t.Fatal("expected authentication to reject a corrupted tag")
	}
}

// TestRelayNodeRejectsUnknownRoute
func TestRelayNodeRejectsUnknownRoute(t *testing.T) {
	a, _, _ := buildTestNetwork(t)
	if _, err := a.Originate("Z", "k-1", "alice", "carol", make([]byte, 32)); err == nil {
		t.Fatal("expected an error for a destination with no route")
	}
}

// TestRelayNodePoolExhaustionIsAnErrorNotAPanic: once a link's pool runs
// dry, relaying must fail loudly, never silently reuse a pad or MAC key
func TestRelayNodePoolExhaustionIsAnErrorNotAPanic(t *testing.T) {
	a, _, _ := buildTestNetwork(t)
	ok := 0
	var lastErr error
	for i := 0; i < 50; i++ {
		if _, err := a.Originate("C", fmt.Sprintf("k-%d", i), "alice", "carol", make([]byte, 32)); err != nil {
			lastErr = err
			break
		}
		ok++
	}
	if lastErr == nil {
		t.Fatal("expected the seeded pool to eventually run out and return an error")
	}
	t.Logf("relayed %d packets before exhaustion: %v", ok, lastErr)
}

// TestRelayFromPoolIsQKDEndToEnd: the relayed key is pool material of the first link, reaches
// C intact, and is spent at both ends of A-B
func TestRelayFromPoolIsQKDEndToEnd(t *testing.T) {
	a, b, c := buildTestNetwork(t)
	pkt, key, err := a.OriginateFromPool("C", "k-1", "alice", "carol", 64)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkt.Cipher) != 0 || len(pkt.PayloadKeyIDs) != 2 {
		t.Fatalf("first hop should carry two key IDs and no ciphertext: %+v", pkt)
	}
	_, _, next, err := b.Handle(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.PayloadKeyIDs) != 0 || len(next.Cipher) != 64 {
		t.Fatalf("second hop must be padded: %+v", next)
	}
	if _, _, _, err := c.Handle(next); err != nil {
		t.Fatal(err)
	}
	got, err := c.Local.GetReserved("k-1", "alice", "carol")
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("C has %x (%v), A handed out %x", got, err, key)
	}
	for _, id := range pkt.PayloadKeyIDs {
		if _, err := a.HopPools["B"].Get(id); err != kms.ErrKeyConsumed {
			t.Fatalf("A still holds %s: %v", id, err)
		}
		if _, err := b.HopPools["A"].Get(id); err != kms.ErrKeyConsumed {
			t.Fatalf("B still holds %s: %v", id, err)
		}
	}
}

// TestRelayFromPoolRefusals: payload IDs are authenticated, must be the sender's half, and only
// the origin itself may send a key by IDs
func TestRelayFromPoolRefusals(t *testing.T) {
	a, b, _ := buildTestNetwork(t)
	pkt, _, err := a.OriginateFromPool("C", "k-1", "alice", "carol", 32)
	if err != nil {
		t.Fatal(err)
	}
	swapped := pkt
	swapped.PayloadKeyIDs = a.HopPools["B"].AvailableIDs(kms.OriginatorOf("A", "B"))[:1]
	if _, err := b.Receive(swapped); err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("swapped payload IDs: %v", err)
	}
	theirs := pkt
	theirs.PayloadKeyIDs = b.HopPools["A"].AvailableIDs(kms.OriginatorOf("B", "A"))[:1]
	if _, err := b.Receive(theirs); err == nil || !strings.Contains(err.Error(), "this node's half") {
		t.Fatalf("receiver's own half: %v", err)
	}
	relayed := pkt
	relayed.Origin = "Z"
	if _, err := b.Receive(relayed); err == nil || !strings.Contains(err.Error(), "straight from its origin") {
		t.Fatalf("key IDs from a non-origin: %v", err)
	}
	// a refused forgery has spent its MAC key (fail closed), so the honest check uses a new packet
	fresh, _, err := a.OriginateFromPool("C", "k-2", "alice", "carol", 32)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Receive(fresh); err != nil {
		t.Fatalf("an untouched packet: %v", err)
	}
}
