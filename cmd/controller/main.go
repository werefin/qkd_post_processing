// controller: QKD network's central controller --> holds the network as the operator wants it (nodes, links,
// settings), hands each node's agent its config in answer to the agent's heartbeat, and shows the whole network
package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func main() {
	stateFile := flag.String("state", "", "desired network state, created if missing (required)")
	listen := flag.String("listen", ":8080", "address to listen on")
	tlsCert := flag.String("tls-cert", "", "controller certificate (qkdpki controller)")
	tlsKey := flag.String("tls-key", "", "controller private key")
	tlsCA := flag.String("tls-ca", "", "network CA: agents and operators present certificates it signed")
	plainHTTP := flag.Bool("insecure-plain-http", false, "serve plain HTTP with identities from X-Dev-Role/X-Dev-ID headers; development only")
	flag.Parse()

	if *stateFile == "" {
		fmt.Fprintln(os.Stderr, "-state is required")
		flag.Usage()
		os.Exit(2)
	}
	if !*plainHTTP && (*tlsCert == "" || *tlsKey == "" || *tlsCA == "") {
		log.Fatal("-tls-cert, -tls-key and -tls-ca are required (mutual TLS); -insecure-plain-http exists for development only")
	}
	st, err := openStore(*stateFile)
	if err != nil {
		log.Fatal(err)
	}
	c := &controller{st: st, dev: *plainHTTP, now: time.Now, ready: func() error { return writable(filepath.Dir(*stateFile)) }}
	d := st.current()
	log.Printf("controller: %d nodes, %d links, revision %d", len(d.Nodes), len(d.Links), d.Revision)
	if *plainHTTP {
		log.Printf("[WARN] plain HTTP on %s with header identities: development only", *listen)
		log.Fatal(http.ListenAndServe(*listen, c.routes()))
	}
	cfg, err := serverTLS(*tlsCert, *tlsKey, *tlsCA)
	if err != nil {
		log.Fatal(err)
	}
	// no write timeout: the event stream stays open
	srv := &http.Server{Addr: *listen, Handler: c.routes(), TLSConfig: cfg, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: time.Minute}
	log.Printf("listening on %s (mutual TLS)", *listen)
	log.Fatal(srv.ListenAndServeTLS("", ""))
}

// serverTLS demands a client certificate signed by the network CA from every caller
func serverTLS(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS key pair: %w", err)
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read -tls-ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("-tls-ca: no PEM certificates found")
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert,
		MinVersion: tls.VersionTLS13}, nil
}

func writable(dir string) error {
	f, err := os.CreateTemp(dir, ".ready")
	if err != nil {
		return fmt.Errorf("state directory not writable: %w", err)
	}
	f.Close()
	return os.Remove(f.Name())
}
