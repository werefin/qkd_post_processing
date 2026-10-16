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
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"qkd_post_processing/kms"
)

// testPKI is a throwaway CA that issues node (kme) and application (sae) certificates
type testPKI struct {
	ca    *x509.Certificate
	caKey *ecdsa.PrivateKey
	pool  *x509.CertPool
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &testPKI{ca: ca, caKey: key, pool: pool}
}

func (p *testPKI) issue(t *testing.T, role, id string) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: id, OrganizationalUnit: []string{role}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func (p *testPKI) client(cert tls.Certificate) *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{cert}, RootCAs: p.pool, MinVersion: tls.VersionTLS13}}}
}

// testNet is a running network: KMEs by node ID, their URLs, and the PKI to make callers with
type testNet struct {
	pki   *testPKI
	nodes map[string]*server
	urls  map[string]string
}

// startNetwork runs one KME per node over mutual TLS; each link's two pools are seeded with the
// same keys under the same key_IDs, as a finished QKD run on that link would leave them
func startNetwork(t *testing.T, links [][2]string, saes map[string]string, keysPerLink int) *testNet {
	t.Helper()
	pki := newTestPKI(t)
	tn := &testNet{pki: pki, nodes: map[string]*server{}, urls: map[string]string{}}
	cfg := &networkConfig{Links: links, SAEs: saes}
	cfg.Nodes = map[string]struct {
		URL string `json:"kme_url"`
	}{}
	ids := map[string]bool{}
	for _, l := range links {
		ids[l[0]], ids[l[1]] = true, true
	}
	unstarted := map[string]*httptest.Server{}
	handlers := map[string]http.Handler{}
	var mu sync.Mutex
	for id := range ids {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			h := handlers[id]
			mu.Unlock()
			h.ServeHTTP(w, r)
		}))
		unstarted[id] = srv
		tn.urls[id] = "https://" + srv.Listener.Addr().String()
		cfg.Nodes[id] = struct {
			URL string `json:"kme_url"`
		}{tn.urls[id]}
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	for id, srv := range unstarted {
		master := make([]byte, 32)
		rand.Read(master)
		s, err := openServer(id, cfg, t.TempDir(), master, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		cert := pki.issue(t, "kme", id)
		s.client = pki.client(cert)
		tn.nodes[id] = s
		mu.Lock()
		handlers[id] = s.routes()
		mu.Unlock()
		srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, ClientCAs: pki.pool,
			ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS13}
		srv.StartTLS()
		t.Cleanup(srv.Close)
	}
	for _, l := range links {
		for range keysPerLink {
			id, _ := kms.NewKeyID()
			key := make([]byte, 32)
			rand.Read(key)
			if err := tn.nodes[l[0]].pools[l[1]].PutWithID(id, key, time.Hour); err != nil {
				t.Fatal(err)
			}
			if err := tn.nodes[l[1]].pools[l[0]].PutWithID(id, key, time.Hour); err != nil {
				t.Fatal(err)
			}
		}
	}
	return tn
}

// call sends one request as role/id to node and decodes the answer into out (if non-nil)
func (tn *testNet) call(t *testing.T, role, id, method, node, path string, body, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		rd = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, tn.urls[node]+path, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := tn.pki.client(tn.pki.issue(t, role, id)).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, data)
		}
	}
	return resp.StatusCode
}

func keyIDsBody(c etsiKeyContainer) map[string]any {
	var ids []map[string]string
	for _, k := range c.Keys {
		ids = append(ids, map[string]string{"key_ID": k.KeyID})
	}
	return map[string]any{"key_IDs": ids}
}

func lineNet(t *testing.T) *testNet {
	return startNetwork(t, [][2]string{{"A", "B"}, {"B", "C"}},
		map[string]string{"alice": "A", "alice2": "A", "bob": "B", "mallory": "B", "carol": "C"}, 60)
}

// TestNeighborKeyDelivery: alice's key for bob reaches bob only, once, through his own KME
func TestNeighborKeyDelivery(t *testing.T) {
	tn := lineNet(t)
	var enc etsiKeyContainer
	if code := tn.call(t, "sae", "alice", "GET", "A", "/api/v1/keys/bob/enc_keys?number=3", nil, &enc); code != 200 || len(enc.Keys) != 3 {
		t.Fatalf("enc_keys: %d, %d keys", code, len(enc.Keys))
	}
	if code := tn.call(t, "sae", "mallory", "POST", "B", "/api/v1/keys/alice/dec_keys", keyIDsBody(enc), nil); code != 404 {
		t.Fatalf("mallory got %d for bob's keys", code)
	}
	var dec etsiKeyContainer
	if code := tn.call(t, "sae", "bob", "POST", "B", "/api/v1/keys/alice/dec_keys", keyIDsBody(enc), &dec); code != 200 {
		t.Fatalf("dec_keys: %d", code)
	}
	for i := range enc.Keys {
		if dec.Keys[i].KeyID != enc.Keys[i].KeyID || dec.Keys[i].Key != enc.Keys[i].Key {
			t.Fatalf("key %d differs: %+v vs %+v", i, enc.Keys[i], dec.Keys[i])
		}
	}
	if code := tn.call(t, "sae", "bob", "GET", "B", "/api/v1/keys/alice/dec_keys?key_ID="+enc.Keys[0].KeyID, nil, nil); code != 410 {
		t.Fatalf("second delivery answered %d, want 410", code)
	}
}

// TestRelayedKeyDelivery: alice and carol share no link; B relays a 512-bit key and never stores it
func TestRelayedKeyDelivery(t *testing.T) {
	tn := lineNet(t)
	var enc, dec etsiKeyContainer
	if code := tn.call(t, "sae", "alice", "POST", "A", "/api/v1/keys/carol/enc_keys", map[string]int{"number": 2, "size": 512}, &enc); code != 200 {
		t.Fatalf("enc_keys: %d", code)
	}
	if raw, _ := base64.StdEncoding.DecodeString(enc.Keys[0].Key); len(raw) != 64 {
		t.Fatalf("key is %d bytes, want 64", len(raw))
	}
	if code := tn.call(t, "sae", "carol", "POST", "C", "/api/v1/keys/alice/dec_keys", keyIDsBody(enc), &dec); code != 200 {
		t.Fatalf("dec_keys: %d", code)
	}
	for i := range enc.Keys {
		if dec.Keys[i] != enc.Keys[i] {
			t.Fatalf("key %d differs", i)
		}
	}
	if st, _ := tn.nodes["B"].local.Status(); st != (kms.Status{}) {
		t.Fatalf("the relay node stored something: %+v", st)
	}
}

// TestCallerIdentityIsEnforced: who may call what follows from the certificate alone
func TestCallerIdentityIsEnforced(t *testing.T) {
	tn := lineNet(t)
	cases := []struct {
		role, id, method, node, path string
		want                         int
	}{
		{"kme", "A", "GET", "A", "/api/v1/keys/bob/enc_keys", 401},        // a node is not an application
		{"sae", "mallory", "GET", "A", "/api/v1/keys/bob/enc_keys", 401},  // an application of another node
		{"sae", "nobody", "GET", "A", "/api/v1/keys/bob/enc_keys", 401},   // not in the network
		{"sae", "alice", "GET", "A", "/api/v1/keys/alice2/enc_keys", 400}, // both SAEs on one node
		{"sae", "alice", "GET", "A", "/api/v1/keys/ghost/enc_keys", 400},  // unknown slave
		{"sae", "alice", "GET", "A", "/api/v1/keys/bob/enc_keys?size=100", 400},
		{"sae", "alice", "POST", "A", "/kme/v1/reserve", 403}, // applications cannot coordinate pools
		{"kme", "C", "POST", "A", "/kme/v1/reserve", 403},     // C shares no pool with A
		{"kme", "B", "GET", "A", "/kme/v1/audit", 403},        // management is for A's own dashboard
		{"kme", "A", "GET", "A", "/kme/v1/audit", 403},        // even A's KME identity is not its admin
		{"admin", "B", "GET", "A", "/kme/v1/audit", 403},      // nor another node's dashboard
		{"admin", "A", "GET", "A", "/kme/v1/audit", 200},
		{"admin", "A", "GET", "A", "/kme/v1/network", 200},
		{"admin", "A", "POST", "B", "/kme/v1/reserve", 403}, // a dashboard cannot act as its node
		{"admin", "A", "GET", "A", "/kme/v1/node?probe=1", 200},
		{"sae", "alice", "GET", "A", "/kme/v1/node", 403},
		{"kme", "B", "GET", "A", "/kme/v1/node", 200},
	}
	for _, c := range cases {
		if got := tn.call(t, c.role, c.id, c.method, c.node, c.path, map[string]any{}, nil); got != c.want {
			t.Errorf("%s %s as %s/%s: %d, want %d", c.method, c.path, c.role, c.id, got, c.want)
		}
	}
}

// TestStatusReportsThePair: ETSI status names both KMEs and counts this node's half of the pool
func TestStatusReportsThePair(t *testing.T) {
	tn := lineNet(t)
	var st map[string]any
	tn.call(t, "sae", "alice", "GET", "A", "/api/v1/keys/carol/status", nil, &st)
	mine := tn.nodes["A"].pools["B"].CountAvailable(kms.OriginatorOf("A", "B"))
	if st["source_KME_ID"] != "A" || st["target_KME_ID"] != "C" || st["master_SAE_ID"] != "alice" || int(st["stored_key_count"].(float64)) != mine/2 {
		t.Fatalf("got %v (A's half holds %d)", st, mine)
	}
}

// TestBothEndsAtOnce: requests from both ends of a link at the same time never share a pool key
func TestBothEndsAtOnce(t *testing.T) {
	tn := startNetwork(t, [][2]string{{"A", "B"}}, map[string]string{"alice": "A", "bob": "B"}, 200)
	var mu sync.Mutex
	seen := map[string]string{}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			from, to, node, peer := "alice", "bob", "A", "B"
			if i%2 == 1 {
				from, to, node, peer = "bob", "alice", "B", "A"
			}
			var enc, dec etsiKeyContainer
			if code := tn.call(t, "sae", from, "GET", node, "/api/v1/keys/"+to+"/enc_keys?number=4", nil, &enc); code != 200 {
				t.Errorf("enc %s: %d", from, code)
				return
			}
			if code := tn.call(t, "sae", to, "POST", peer, "/api/v1/keys/"+from+"/dec_keys", keyIDsBody(enc), &dec); code != 200 {
				t.Errorf("dec %s: %d", to, code)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for j, k := range enc.Keys {
				if dec.Keys[j].Key != k.Key {
					t.Errorf("%s: ends disagree", k.KeyID)
				}
				if prev, dup := seen[k.Key]; dup {
					t.Errorf("key material delivered twice: %s and %s", prev, k.KeyID)
				}
				seen[k.Key] = k.KeyID
			}
		}(i)
	}
	wg.Wait()
	if len(seen) != 80 {
		t.Fatalf("%d distinct keys, want 80", len(seen))
	}
}

// TestReserveRefusesBadRequests: a neighbor cannot spend this node's half, reuse a key, or
// reserve for an SAE pair that does not match the two nodes
func TestReserveRefusesBadRequests(t *testing.T) {
	tn := lineNet(t)
	b := tn.nodes["B"]
	poolAtB := b.pools["A"]
	aHalf := poolAtB.AvailableIDs(kms.OriginatorOf("A", "B"))
	bHalf := poolAtB.AvailableIDs(kms.OriginatorOf("B", "A"))
	before := poolAtB.CountAvailable(nil)
	reserve := func(master, slave string, comps ...string) int {
		id, _ := kms.NewKeyID()
		return tn.call(t, "kme", "A", "POST", "B", "/kme/v1/reserve", reserveRequest{Master: master, Slave: slave,
			Keys: []reserveItem{{KeyID: id, Components: comps, SizeBytes: 32 * len(comps)}}}, nil)
	}
	if code := reserve("alice", "bob", bHalf[0]); code != 400 {
		t.Fatalf("B's own half: %d", code)
	}
	if code := reserve("alice", "bob", aHalf[0], aHalf[0]); code != 400 {
		t.Fatalf("repeated component: %d", code)
	}
	if code := reserve("carol", "bob", aHalf[0]); code != 400 {
		t.Fatalf("master not at A: %d", code)
	}
	if code := reserve("alice", "carol", aHalf[0]); code != 400 {
		t.Fatalf("slave not at B: %d", code)
	}
	if after := poolAtB.CountAvailable(nil); after != before {
		t.Fatalf("refused reservations consumed %d pool keys", before-after)
	}
	if code := reserve("alice", "bob", aHalf[0]); code != 200 {
		t.Fatalf("valid reservation: %d", code)
	}
	if code := reserve("alice", "bob", aHalf[0]); code != 409 {
		t.Fatalf("spent component: %d", code)
	}
}

// TestRelayOnlyFromTheNamedSender: a node cannot inject a packet in a neighbor's name
func TestRelayOnlyFromTheNamedSender(t *testing.T) {
	tn := lineNet(t)
	pkt, err := tn.nodes["A"].node.Originate("C", "k-1", "alice", "carol", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if code := tn.call(t, "kme", "C", "POST", "B", "/kme/v1/relay", pkt, nil); code != 403 {
		t.Fatalf("C posing as A: %d", code)
	}
	if code := tn.call(t, "kme", "A", "POST", "B", "/kme/v1/relay", pkt, nil); code != 200 {
		t.Fatalf("A itself: %d", code)
	}
}

// TestAuditHoldsNoKeyMaterial: every delivered key is in the audit trail by ID, never by value
func TestAuditHoldsNoKeyMaterial(t *testing.T) {
	tn := lineNet(t)
	var enc etsiKeyContainer
	tn.call(t, "sae", "alice", "GET", "A", "/api/v1/keys/carol/enc_keys?number=2", nil, &enc)
	tn.call(t, "sae", "carol", "POST", "C", "/api/v1/keys/alice/dec_keys", keyIDsBody(enc), nil)
	var ev struct{ Events []auditEvent }
	tn.call(t, "admin", "C", "GET", "C", "/kme/v1/audit", nil, &ev)
	for _, n := range []string{"A", "B", "C"} {
		data, _ := os.ReadFile(filepath.Join(tn.nodes[n].dataDir, "audit.log"))
		for _, k := range enc.Keys {
			raw, _ := base64.StdEncoding.DecodeString(k.Key)
			if bytes.Contains(data, []byte(k.Key)) || bytes.Contains(data, raw) || bytes.Contains(data, []byte(fmt.Sprintf("%x", raw))) {
				t.Fatalf("%s's audit log holds key material", n)
			}
		}
		if n != "B" && !strings.Contains(string(data), enc.Keys[0].KeyID) {
			t.Fatalf("%s's audit log lacks %s", n, enc.Keys[0].KeyID)
		}
	}
	if len(ev.Events) == 0 || ev.Events[0].Event != "dec_keys" {
		t.Fatalf("C's newest event: %+v", ev.Events)
	}
}

// TestImportKeys: settled records move into the right link's pool and their plaintext is deleted;
// a record still being written and a stray file are left alone
func TestImportKeys(t *testing.T) {
	tn := lineNet(t)
	b := tn.nodes["B"]
	root := t.TempDir()
	stored, _, err := kms.SaveKeys256(filepath.Join(root, "A"), bytes.Repeat([]byte{7}, 64))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	for _, s := range stored {
		os.Chtimes(filepath.Join(root, "A", s.KeyID+".json"), old, old)
	}
	fresh, _ := kms.SaveKey(filepath.Join(root, "A"), []byte("partial"))
	os.WriteFile(filepath.Join(root, "A", "notes.txt"), []byte("x"), 0o600)
	unlinked, _ := kms.SaveKey(filepath.Join(root, "Z"), bytes.Repeat([]byte{1}, 32))
	os.Chtimes(filepath.Join(root, "Z", unlinked.KeyID+".json"), old, old)

	b.importKeys(root)

	for _, s := range stored {
		if _, err := os.Stat(filepath.Join(root, "A", s.KeyID+".json")); !os.IsNotExist(err) {
			t.Fatalf("plaintext %s still on disk", s.KeyID)
		}
		got, err := b.pools["A"].Get(s.KeyID)
		if err != nil || !bytes.Equal(got, bytes.Repeat([]byte{7}, 32)) {
			t.Fatalf("pool A: %x %v", got, err)
		}
	}
	for _, p := range []string{filepath.Join("A", fresh.KeyID+".json"), filepath.Join("A", "notes.txt"), filepath.Join("Z", unlinked.KeyID+".json")} {
		if _, err := os.Stat(filepath.Join(root, p)); err != nil {
			t.Fatalf("%s should be left alone: %v", p, err)
		}
	}
}

// TestPostProcessingMailbox: A's dashboard reaches B's dashboard through the two KMEs, only
// between neighbors, only with the admin certificate, and an empty session answers 204
func TestPostProcessingMailbox(t *testing.T) {
	tn := lineNet(t)
	send := map[string]any{"peer": "B", "session": "s1", "kind": "offer", "body": map[string]int{"n": 3}}
	if code := tn.call(t, "admin", "A", "POST", "A", "/kme/v1/pp/send", send, nil); code != 200 {
		t.Fatalf("send: %d", code)
	}
	var sessions struct{ Sessions []ppSession }
	tn.call(t, "admin", "B", "GET", "B", "/kme/v1/pp/sessions", nil, &sessions)
	if len(sessions.Sessions) != 1 || sessions.Sessions[0].Peer != "A" || sessions.Sessions[0].FirstKind != "offer" {
		t.Fatalf("sessions at B: %+v", sessions)
	}
	var msg ppMessage
	if code := tn.call(t, "admin", "B", "GET", "B", "/kme/v1/pp/recv?peer=A&session=s1&wait=1", nil, &msg); code != 200 || msg.Kind != "offer" || string(msg.Body) != `{"n":3}` {
		t.Fatalf("recv: %d %+v", code, msg)
	}
	if code := tn.call(t, "admin", "B", "GET", "B", "/kme/v1/pp/recv?peer=A&session=s1&wait=1", nil, nil); code != 204 {
		t.Fatalf("empty session: %d", code)
	}
	toC := map[string]any{"peer": "C", "session": "s1", "kind": "offer", "body": 1}
	for _, c := range []struct {
		role, id, node, path string
		body                 any
		want                 int
	}{
		{"admin", "A", "A", "/kme/v1/pp/send", toC, 400},    // C is not A's neighbor
		{"sae", "alice", "A", "/kme/v1/pp/send", send, 403}, // applications cannot use it
		{"kme", "A", "A", "/kme/v1/pp/send", send, 403},     // nor the node identity itself
		{"kme", "C", "A", "/kme/v1/pp/inbox", map[string]any{"session": "s1", "kind": "x", "body": 1}, 403},
		{"admin", "A", "B", "/kme/v1/pp/recv?peer=A&session=s1", nil, 403}, // B's mail is B's dashboard's
	} {
		method := "POST"
		if c.body == nil {
			method = "GET"
		}
		if got := tn.call(t, c.role, c.id, method, c.node, c.path, c.body, nil); got != c.want {
			t.Errorf("%s %s as %s/%s: %d, want %d", method, c.path, c.role, c.id, got, c.want)
		}
	}
}
