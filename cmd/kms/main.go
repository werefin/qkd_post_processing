// kms: the QKD key management system, one instance (KME) per node. Serves ETSI GS QKD 014 to its
// applications (SAEs), keeps link pools in step with neighbor KMEs and relays keys across the network
package main

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"qkd_post_processing/kms"
	"qkd_post_processing/relay"
)

// server is one KME: its delivery store (keys reserved for SAE pairs), one pool per QKD link,
// the relay node over those pools, and the audit trail
type server struct {
	id     string
	net    *networkConfig
	local  *kms.KMS
	pools  map[string]*kms.KMS
	node   *relay.RelayNode
	client *http.Client
	audit  *auditLog
	mail   *mailbox
	dev    bool // plain HTTP: identities come from headers, development only

	deliveryTTL time.Duration
	dataDir     string
}

// caller is who sent a request: role "kme" (a node) or "sae" (an application), from the client
// certificate's organizational unit and common name
type caller struct{ role, id string }

func (s *server) who(r *http.Request) caller {
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		cert := r.TLS.PeerCertificates[0]
		if len(cert.Subject.OrganizationalUnit) == 1 {
			return caller{cert.Subject.OrganizationalUnit[0], cert.Subject.CommonName}
		}
		return caller{}
	}
	if s.dev {
		return caller{r.Header.Get("X-Dev-Role"), r.Header.Get("X-Dev-ID")}
	}
	return caller{}
}

// localSAE is the calling application if it belongs to this KME, "" otherwise
func (s *server) localSAE(r *http.Request) string {
	c := s.who(r)
	if c.role == "sae" && c.id != "" && s.net.homeOf(c.id) == s.id {
		return c.id
	}
	return ""
}

// isAdmin: management calls come only from this node's admin certificate (its dashboard), which
// cannot act as the node toward other KMEs
func (s *server) isAdmin(r *http.Request) bool {
	c := s.who(r)
	return c.role == "admin" && c.id == s.id
}

type etsiError struct {
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, etsiError{Message: msg})
}

func loadMasterKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read master key file: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(string(data))
	if err != nil {
		return nil, fmt.Errorf("master key file must contain base64-encoded bytes: %w", err)
	}
	return key, nil
}

func main() {
	nodeID := flag.String("node-id", "", "this node's ID in -network (required)")
	networkFile := flag.String("network", "", "network description shared by every node: nodes, links, SAEs (required)")
	dataDir := flag.String("data", "", "data directory: local/ (reserved keys), pools/<neighbor>/, audit.log (required)")
	masterKeyFile := flag.String("master-key-file", "", "file holding the base64 32-byte at-rest encryption key (required; qkdpki node writes one)")
	generateMasterKey := flag.String("generate-master-key", "", "write a fresh random 32-byte master key (base64) to this path and exit")
	listen := flag.String("listen", ":8443", "address to listen on")
	tlsCert := flag.String("tls-cert", "", "this node's certificate (role kme)")
	tlsKey := flag.String("tls-key", "", "this node's private key")
	tlsCA := flag.String("tls-ca", "", "network CA: every caller and every peer must present a certificate it signed")
	plainHTTP := flag.Bool("insecure-plain-http", false, "serve plain HTTP with identities from X-Dev-Role/X-Dev-ID headers; development only, never with real keys")
	importRoot := flag.String("import-root", "", "pipeline output to import: <root>/<neighbor>/ into that link's pool; each plaintext record is deleted once encrypted")
	flag.DurationVar(&keyTTL, "key-ttl", keyTTL, "how long imported pool keys stay usable")
	deliveryTTL := flag.Duration("delivery-ttl", 24*time.Hour, "how long a key reserved for an SAE pair waits for its slave SAE")
	flag.Parse()

	if *generateMasterKey != "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			log.Fatalf("generate master key: %v", err)
		}
		if err := os.WriteFile(*generateMasterKey, []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
			log.Fatalf("write master key: %v", err)
		}
		fmt.Printf("wrote a new master key to %s; back it up off this host before use\n", *generateMasterKey)
		return
	}
	if *nodeID == "" || *networkFile == "" || *dataDir == "" || *masterKeyFile == "" {
		fmt.Fprintln(os.Stderr, "-node-id, -network, -data and -master-key-file are required")
		flag.Usage()
		os.Exit(2)
	}
	if !*plainHTTP && (*tlsCert == "" || *tlsKey == "" || *tlsCA == "") {
		log.Fatal("-tls-cert, -tls-key and -tls-ca are required (mutual TLS); -insecure-plain-http exists for development only")
	}

	s, err := newServer(*nodeID, *networkFile, *dataDir, *masterKeyFile, *deliveryTTL)
	if err != nil {
		log.Fatal(err)
	}
	s.dev = *plainHTTP
	serverTLS, clientTLS, err := mutualTLS(*tlsCert, *tlsKey, *tlsCA)
	if err != nil {
		log.Fatal(err)
	}
	// long enough for a large post-processing announcement to cross a slow link
	s.client = &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{TLSClientConfig: clientTLS}}

	if *importRoot != "" {
		go s.runImporter(*importRoot)
		log.Printf("importing pipeline keys from %s", *importRoot)
	}
	go s.runSweeper()

	log.Printf("KME %s: neighbors %v, routes %v, %d SAEs on this node", s.id, s.net.neighbors(s.id), s.node.NextHop, s.localSAECount())
	if serverTLS == nil {
		log.Printf("[WARN] plain HTTP on %s with header identities: development only", *listen)
		log.Fatal(http.ListenAndServe(*listen, s.routes()))
	}
	srv := &http.Server{Addr: *listen, Handler: s.routes(), TLSConfig: serverTLS,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 5 * time.Minute, WriteTimeout: 5 * time.Minute}
	log.Printf("listening on %s (mutual TLS)", *listen)
	log.Fatal(srv.ListenAndServeTLS("", ""))
}

// newServer opens the stores this node's place in the network calls for
func newServer(id, networkFile, dataDir, masterKeyFile string, deliveryTTL time.Duration) (*server, error) {
	net, err := loadNetwork(networkFile)
	if err != nil {
		return nil, err
	}
	if _, ok := net.Nodes[id]; !ok {
		return nil, fmt.Errorf("node %s is not in %s", id, networkFile)
	}
	masterKey, err := loadMasterKey(masterKeyFile)
	if err != nil {
		return nil, err
	}
	return openServer(id, net, dataDir, masterKey, deliveryTTL)
}

func openServer(id string, net *networkConfig, dataDir string, masterKey []byte, deliveryTTL time.Duration) (*server, error) {
	local, err := kms.NewKMS(filepath.Join(dataDir, "local"), masterKey)
	if err != nil {
		return nil, err
	}
	pools := map[string]*kms.KMS{}
	for _, nb := range net.neighbors(id) {
		if pools[nb], err = kms.NewKMS(filepath.Join(dataDir, "pools", nb), masterKey); err != nil {
			return nil, err
		}
	}
	audit, err := openAudit(filepath.Join(dataDir, "audit.log"))
	if err != nil {
		return nil, err
	}
	relay.SetDeliveryTTL(deliveryTTL)
	return &server{id: id, net: net, local: local, pools: pools, audit: audit, mail: newMailbox(), deliveryTTL: deliveryTTL, dataDir: dataDir,
		node: relay.NewRelayNode(id, local, pools, net.nextHops(id)), client: http.DefaultClient}, nil
}

func (s *server) localSAECount() int {
	n := 0
	for _, home := range s.net.SAEs {
		if home == s.id {
			n++
		}
	}
	return n
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	// ETSI GS QKD 014, for this node's applications
	mux.HandleFunc("GET /api/v1/keys/{slave_sae_id}/status", s.handleStatus)
	mux.HandleFunc("GET /api/v1/keys/{slave_sae_id}/enc_keys", s.handleEncKeys)
	mux.HandleFunc("POST /api/v1/keys/{slave_sae_id}/enc_keys", s.handleEncKeys)
	mux.HandleFunc("GET /api/v1/keys/{master_sae_id}/dec_keys", s.handleDecKeys)
	mux.HandleFunc("POST /api/v1/keys/{master_sae_id}/dec_keys", s.handleDecKeys)
	// KME to KME, node certificates only
	mux.HandleFunc("POST /kme/v1/reserve", s.handleReserve)
	mux.HandleFunc("POST /kme/v1/relay", s.handleRelay)
	mux.HandleFunc("GET /kme/v1/node", s.handleNode)
	// management, this node's own certificate only
	mux.HandleFunc("GET /kme/v1/network", s.handleNetwork)
	mux.HandleFunc("GET /kme/v1/audit", s.handleAudit)
	// two-party post-processing messages between neighbors' dashboards
	mux.HandleFunc("POST /kme/v1/pp/send", s.handlePPSend)
	mux.HandleFunc("POST /kme/v1/pp/inbox", s.handlePPInbox)
	mux.HandleFunc("GET /kme/v1/pp/recv", s.handlePPRecv)
	mux.HandleFunc("GET /kme/v1/pp/sessions", s.handlePPSessions)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := int64(1 << 20)
		if strings.HasPrefix(r.URL.Path, "/kme/v1/pp/") {
			limit = ppMaxMessage + 1<<20
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		mux.ServeHTTP(w, r)
	})
}

// mutualTLS builds the server and peer-client TLS configs: the server demands and verifies client
// certificates, the client verifies peers against the same CA and presents this node's certificate
func mutualTLS(certFile, keyFile, caFile string) (server, client *tls.Config, err error) {
	if certFile == "" {
		if caFile != "" {
			return nil, nil, fmt.Errorf("-tls-ca needs -tls-cert and -tls-key")
		}
		return nil, nil, nil
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("load TLS key pair: %w", err)
	}
	server = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	client = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read -tls-ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, nil, fmt.Errorf("-tls-ca: no PEM certificates found")
		}
		server.ClientCAs, server.ClientAuth = pool, tls.RequireAndVerifyClientCert
		client.RootCAs = pool
	}
	return server, client, nil
}
