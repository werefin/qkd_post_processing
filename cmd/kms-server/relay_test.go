package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	qkd "qkd_post_processing"
)

// newKMS opens a fresh store with a random master key
func newKMS(t *testing.T) *qkd.KMS {
	t.Helper()
	master := make([]byte, 32)
	rand.Read(master)
	k, err := qkd.NewKMS(t.TempDir(), master)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// seedLink mirrors 20 identical 32-byte keys into both ends of one link, as QKD would
func seedLink(t *testing.T) (a, b *qkd.KMS) {
	t.Helper()
	a, b = newKMS(t), newKMS(t)
	for range 20 {
		key := make([]byte, 32)
		rand.Read(key)
		id, err := a.Put(key, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.PutWithID(id, key, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	return a, b
}

// chain starts A -> B -> C as three HTTP servers built by newServer, wired with client
func chain(t *testing.T, newServer func(http.Handler) *httptest.Server, client *http.Client) (originate string, localC *qkd.KMS) {
	t.Helper()
	abA, abB := seedLink(t)
	bcB, bcC := seedLink(t)
	localC = newKMS(t)

	mux := func(h *relayHandler) *http.ServeMux {
		m := http.NewServeMux()
		m.HandleFunc("POST /api/v1/relay", h.handleRelay)
		m.HandleFunc("POST /api/v1/relay/originate/{dest_node_id}", h.handleOriginate)
		return m
	}
	hC := &relayHandler{node: qkd.NewRelayNode("C", localC, map[string]*qkd.KMS{"B": bcC}, nil), client: client}
	sC := newServer(mux(hC))
	hB := &relayHandler{node: qkd.NewRelayNode("B", newKMS(t), map[string]*qkd.KMS{"A": abB, "C": bcB}, map[string]string{"C": "C"}), peerURLs: map[string]string{"C": sC.URL}, client: client}
	sB := newServer(mux(hB))
	hA := &relayHandler{node: qkd.NewRelayNode("A", newKMS(t), map[string]*qkd.KMS{"B": abA}, map[string]string{"C": "B"}), peerURLs: map[string]string{"B": sB.URL}, client: client}
	sA := newServer(mux(hA))
	t.Cleanup(func() { sA.Close(); sB.Close(); sC.Close() })
	return sA.URL + "/api/v1/relay/originate/C", localC
}

// originateAndCheck sends a key from A to C and confirms C stored exactly it under the
// key_ID that came back to A
func originateAndCheck(t *testing.T, url string, client *http.Client, localC *qkd.KMS) {
	t.Helper()
	key := []byte("0123456789abcdef0123456789abcdef")
	body, _ := json.Marshal(map[string]string{"key_bytes": base64.StdEncoding.EncodeToString(key)})
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var res relayResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("originate: status %s, decode err %v", resp.Status, err)
	}
	if res.Status != "delivered" || res.KeyID == "" {
		t.Fatalf("expected delivered with a key_ID, got %+v", res)
	}
	got, err := localC.Get(res.KeyID)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("C holds %x (err %v), want %x", got, err, key)
	}
}

// TestRelayOverHTTP: A -> B -> C over HTTP delivers the key and returns C's key_ID to A
func TestRelayOverHTTP(t *testing.T) {
	url, localC := chain(t, httptest.NewServer, http.DefaultClient)
	originateAndCheck(t, url, http.DefaultClient, localC)
}

// TestRelayOverMutualTLS: the same chain with every node demanding and verifying client
// certificates; a client without one must be refused
func TestRelayOverMutualTLS(t *testing.T) {
	caFile, certFile, keyFile := writeTestPKI(t)
	serverTLS, clientTLS, err := mutualTLS(certFile, keyFile, caFile)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}
	newServer := func(h http.Handler) *httptest.Server {
		s := httptest.NewUnstartedServer(h)
		s.TLS = serverTLS
		s.StartTLS()
		return s
	}
	url, localC := chain(t, newServer, client)
	originateAndCheck(t, url, client, localC)

	anonymous := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: clientTLS.RootCAs}}}
	if resp, err := anonymous.Post(url, "application/json", bytes.NewReader([]byte("{}"))); err == nil {
		resp.Body.Close()
		t.Fatal("a client without a certificate was accepted")
	}
}

// TestParseAssignments covers the flag syntax shared by -relay-routes/-peers/-pools
func TestParseAssignments(t *testing.T) {
	m, err := parseAssignments(" C=B , D=B ")
	if err != nil || m["C"] != "B" || m["D"] != "B" || len(m) != 2 {
		t.Fatalf("got %v, %v", m, err)
	}
	if _, err := parseAssignments("C"); err == nil {
		t.Fatal("expected an error for a missing '='")
	}
}

// writeTestPKI writes a throwaway CA and one localhost certificate it signed, for both roles
func writeTestPKI(t *testing.T) (caFile, certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "node"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	caFile, certFile, keyFile = dir+"/ca.pem", dir+"/node.pem", dir+"/node.key"
	writePEM(t, caFile, "CERTIFICATE", caDER)
	writePEM(t, certFile, "CERTIFICATE", der)
	writePEM(t, keyFile, "EC PRIVATE KEY", keyDER)
	return caFile, certFile, keyFile
}

func writePEM(t *testing.T, path, kind string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}
