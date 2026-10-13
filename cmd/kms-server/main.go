// kms-server: ETSI GS QKD 014 Get Key / Get Key with Key IDs / Status, plus
// multi-hop relay (relay.go). Deploy only behind mutual TLS
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
	"strconv"
	"time"

	qkd "qkd_post_processing"
)

type server struct {
	kms          *qkd.KMS
	sourceKMEID  string
	targetKMEID  string
	masterSAEID  string
	slaveSAEID   string
	defaultSize  int
	defaultCount int
}

// etsiKeyContainer is ETSI 014's "Key container" response shape
type etsiKeyContainer struct {
	Keys []etsiKey `json:"keys"`
}

type etsiKey struct {
	KeyID string `json:"key_ID"`
	Key   string `json:"key"` // base64
}

type etsiKeyIDsRequest struct {
	KeyIDs []struct {
		KeyID string `json:"key_ID"`
	} `json:"key_IDs"`
}

type etsiError struct {
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, etsiError{Message: msg})
}

// handleEncKeys is ETSI 014's "Get Key": GET /api/v1/keys/{slave_SAE_ID}/enc_keys
func (s *server) handleEncKeys(w http.ResponseWriter, r *http.Request) {
	n := s.defaultCount
	if v := r.URL.Query().Get("number"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, "number must be a positive integer")
			return
		}
		n = parsed
	}

	delivered, err := s.kms.GetNext(n)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(delivered) < n {
		log.Printf("enc_keys: requested %d, only %d available in the pool", n, len(delivered))
	}

	resp := etsiKeyContainer{Keys: make([]etsiKey, len(delivered))}
	for i, dk := range delivered {
		resp.Keys[i] = etsiKey{KeyID: dk.KeyID, Key: base64.StdEncoding.EncodeToString(dk.Key)}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDecKeys is ETSI 014's "Get Key with Key IDs": POST /api/v1/keys/{master_SAE_ID}/dec_keys
func (s *server) handleDecKeys(w http.ResponseWriter, r *http.Request) {
	var req etsiKeyIDsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if len(req.KeyIDs) == 0 {
		writeError(w, http.StatusBadRequest, "key_IDs must be non-empty")
		return
	}

	resp := etsiKeyContainer{Keys: make([]etsiKey, 0, len(req.KeyIDs))}
	for _, id := range req.KeyIDs {
		key, err := s.kms.Get(id.KeyID)
		if err != nil {
			writeError(w, http.StatusNotFound, fmt.Sprintf("key_ID %s: %v", id.KeyID, err))
			return
		}
		resp.Keys = append(resp.Keys, etsiKey{KeyID: id.KeyID, Key: base64.StdEncoding.EncodeToString(key)})
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleStatus is ETSI 014's "Get Status": GET /api/v1/keys/{slave_SAE_ID}/status
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.kms.Status()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"source_KME_ID":       s.sourceKMEID,
		"target_KME_ID":       s.targetKMEID,
		"master_SAE_ID":       s.masterSAEID,
		"slave_SAE_ID":        s.slaveSAEID,
		"key_size":            s.defaultSize,
		"stored_key_count":    st.StoredKeyCount,
		"consumed_key_count":  st.ConsumedKeyCount,
		"expired_key_count":   st.ExpiredKeyCount,
		"max_key_per_request": 128,
	})
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
	dir := flag.String("dir", "", "directory for the encrypted key store (required)")
	masterKeyFile := flag.String("master-key-file", "", "file containing the base64-encoded 32-byte at-rest encryption key (required; generate once with -generate-master-key)")
	generateMasterKey := flag.String("generate-master-key", "", "write a fresh random 32-byte master key (base64) to this path and exit; store it outside version control, losing it makes every stored key unrecoverable")
	listen := flag.String("listen", ":8443", "address to listen on")
	tlsCert := flag.String("tls-cert", "", "TLS certificate file; if empty, serves plain HTTP (development only -- never expose a key store over plain HTTP)")
	tlsKey := flag.String("tls-key", "", "TLS private key file")
	defaultCount := flag.Int("default-count", 1, "keys returned per enc_keys call when ?number= is not given")
	sourceKME := flag.String("source-kme-id", "", "this site's KME identifier, reported in /status")
	targetKME := flag.String("target-kme-id", "", "peer site's KME identifier, reported in /status")
	masterSAE := flag.String("master-sae-id", "", "master SAE identifier, reported in /status")
	slaveSAE := flag.String("slave-sae-id", "", "slave SAE identifier, used in URL paths and reported in /status")
	tlsCA := flag.String("tls-ca", "", "CA certificate file: clients must present a certificate it signed (mutual TLS), and relay peers are verified against it")
	nodeID := flag.String("node-id", "", "this node's ID in the relay network; empty disables the relay endpoints")
	relayRoutes := flag.String("relay-routes", "", "relay routing table, dest=next_hop pairs, e.g. C=B,D=B")
	relayPeers := flag.String("relay-peers", "", "neighbor base URLs, node=url pairs, e.g. B=https://b.example:8443")
	relayPools := flag.String("relay-pools", "", "per-neighbor key pool directories (that link's QKD output, same master key), node=dir pairs")
	flag.Parse()

	if *generateMasterKey != "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			log.Fatalf("generate master key: %v", err)
		}
		encoded := base64.StdEncoding.EncodeToString(key)
		if err := os.WriteFile(*generateMasterKey, []byte(encoded), 0o600); err != nil {
			log.Fatalf("write master key: %v", err)
		}
		fmt.Printf("wrote a new master key to %s -- back it up somewhere other than this host before using it\n", *generateMasterKey)
		return
	}

	if *dir == "" || *masterKeyFile == "" {
		fmt.Fprintln(os.Stderr, "-dir and -master-key-file are required (see -generate-master-key to create one)")
		flag.Usage()
		os.Exit(2)
	}

	masterKey, err := loadMasterKey(*masterKeyFile)
	if err != nil {
		log.Fatal(err)
	}
	kms, err := qkd.NewKMS(*dir, masterKey)
	if err != nil {
		log.Fatal(err)
	}

	s := &server{
		kms: kms, sourceKMEID: *sourceKME, targetKMEID: *targetKME,
		masterSAEID: *masterSAE, slaveSAEID: *slaveSAE, defaultCount: *defaultCount,
	}

	serverTLS, clientTLS, err := mutualTLS(*tlsCert, *tlsKey, *tlsCA)
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/keys/{slave_sae_id}/enc_keys", s.handleEncKeys)
	mux.HandleFunc("POST /api/v1/keys/{master_sae_id}/dec_keys", s.handleDecKeys)
	mux.HandleFunc("GET /api/v1/keys/{slave_sae_id}/status", s.handleStatus)
	if *nodeID != "" {
		client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: clientTLS}}
		rh, err := newRelayHandler(*nodeID, kms, masterKey, *relayRoutes, *relayPeers, *relayPools, client)
		if err != nil {
			log.Fatal(err)
		}
		mux.HandleFunc("POST /api/v1/relay", rh.handleRelay)
		mux.HandleFunc("POST /api/v1/relay/originate/{dest_node_id}", rh.handleOriginate)
		log.Printf("relay enabled: node %s, routes %s", *nodeID, *relayRoutes)
	}

	if serverTLS == nil {
		log.Printf("[WARN] serving plain HTTP on %s; a real deployment must use -tls-cert/-tls-key/-tls-ca (mutual TLS)", *listen)
		log.Fatal(http.ListenAndServe(*listen, mux))
	}
	if serverTLS.ClientAuth != tls.RequireAndVerifyClientCert {
		log.Printf("[WARN] TLS without -tls-ca: clients are not authenticated; set -tls-ca for mutual TLS")
	}
	srv := &http.Server{Addr: *listen, Handler: mux, TLSConfig: serverTLS, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("listening on %s (TLS)", *listen)
	log.Fatal(srv.ListenAndServeTLS("", ""))
}

// mutualTLS builds the server and relay-client TLS configs: with a CA, the server demands and
// verifies client certificates and the client verifies peers against it, presenting our own cert
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
