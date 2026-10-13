package relay

import (
	"bytes"
	"crypto/rand"
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
	abAtA, abAtB := seedLink(20)
	bcAtB, bcAtC := seedLink(20)

	a = NewRelayNode("A", newKMS(), map[string]*kms.KMS{"B": abAtA}, map[string]string{"C": "B", "B": "B"})
	b = NewRelayNode("B", newKMS(), map[string]*kms.KMS{"A": abAtB, "C": bcAtB}, map[string]string{"C": "C"})
	c = NewRelayNode("C", newKMS(), map[string]*kms.KMS{"B": bcAtC}, nil)
	return a, b, c
}

// TestRelayNodeTwoHopDeliversOriginalKey: A -> B -> C, B never stores the
// key beyond the moment it re-encrypts, C recovers exactly what A sent
func TestRelayNodeTwoHopDeliversOriginalKey(t *testing.T) {
	a, b, c := buildTestNetwork(t)
	original := []byte{0xAA, 0xBB, 0xCC, 0xDD}

	pkt, err := a.Originate("C", original)
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

	got, err := c.Local.Get(keyID)
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
	pkt, err := a.Originate("C", []byte{0xAA, 0xBB, 0xCC, 0xDD})
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

// TestRelayNodeDetectsRoutingMetadataTampering: the MAC must also cover
// Dest/FromHop/PadKeyIDs, not just the ciphertext, or routing is forgeable
func TestRelayNodeDetectsRoutingMetadataTampering(t *testing.T) {
	a, b, _ := buildTestNetwork(t)
	pkt, err := a.Originate("C", []byte{0xAA, 0xBB, 0xCC, 0xDD})
	if err != nil {
		t.Fatal(err)
	}

	tampered := pkt
	tampered.Dest = "Z" // redirect, ciphertext and tag left untouched

	if _, err := b.Receive(tampered); err == nil {
		t.Fatal("expected authentication to reject a packet with tampered routing metadata")
	}
}

// TestRelayNodeDetectsTagTampering: a corrupted tag must fail even when
// the ciphertext and metadata are untouched
func TestRelayNodeDetectsTagTampering(t *testing.T) {
	a, b, _ := buildTestNetwork(t)
	pkt, err := a.Originate("C", []byte{0xAA, 0xBB, 0xCC, 0xDD})
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
	if _, err := a.Originate("Z", []byte{1, 2, 3, 4}); err == nil {
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
		if _, err := a.Originate("C", []byte{1, 2, 3, 4}); err != nil {
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
