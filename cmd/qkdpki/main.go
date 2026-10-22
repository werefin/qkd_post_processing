// qkdpki: the network's private CA and one credential bundle per node (mutual-TLS certificate
// plus KMS master key), so a node needs nothing but Docker and its own bundle
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const usage = `usage:
  qkdpki ca   -out DIR [-name NAME] [-years N]
  qkdpki node -ca DIR -id NODE_ID -hosts h1,h2,... -out DIR [-years N]
  qkdpki sae  -ca DIR -id SAE_ID -out DIR [-years N]
  qkdpki controller -ca DIR -hosts h1,h2,... -out DIR [-years N]
  qkdpki net  -file network.json (add-node ID URL | add-link A B | add-sae SAE_ID NODE | remove-sae SAE_ID | show)

ca writes ca.pem and ca.key; keep ca.key offline, it can mint new nodes and applications
node writes one node's bundle: kms/ (node.pem, node.key, master.key; role kme, for the KMS only),
app/ (admin.pem, admin.key; role admin, for the dashboard) and ui/ (ui.pem, ui.key; HTTPS only)
sae writes sae.pem, sae.key and ca.pem for one application (role sae), a client certificate
whose common name is the SAE ID the KMS authorizes it as
controller writes the network controller's controller.pem and controller.key (role controller, HTTPS only) and an
operator.pem and operator.key (role operator) for whoever changes the network; agents use their node's admin certificate
net edits the network description every node shares (nodes, QKD links, SAEs), creating it if needed`

// Certificate roles, carried in the organizational unit: the KMS authorizes callers by role and
// common name, so an application certificate can never act as a node or the other way round
const (
	roleKME   = "kme"
	roleSAE   = "sae"
	roleAdmin = "admin"
	roleUI    = "ui"
	// the network controller serves with roleController and lets roleOperator change the network
	roleController = "controller"
	roleOperator   = "operator"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "ca":
		err = cmdCA(os.Args[2:])
	case "node":
		err = cmdNode(os.Args[2:])
	case "sae":
		err = cmdSAE(os.Args[2:])
	case "controller":
		err = cmdController(os.Args[2:])
	case "net":
		err = cmdNet(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "qkdpki:", err)
		os.Exit(1)
	}
}

func cmdCA(args []string) error {
	fs := flag.NewFlagSet("ca", flag.ExitOnError)
	out := fs.String("out", "", "output directory (required)")
	name := fs.String("name", "QKD network CA", "CA common name")
	years := fs.Int("years", 10, "validity in years")
	fs.Parse(args)
	if *out == "" {
		return errors.New("ca: -out is required")
	}
	if _, err := os.Stat(filepath.Join(*out, "ca.key")); err == nil {
		return fmt.Errorf("ca: %s already holds a CA, refusing to overwrite it", *out)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(), Subject: pkix.Name{CommonName: *name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(*years, 0, 0),
		IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o700); err != nil {
		return err
	}
	if err := writeKey(filepath.Join(*out, "ca.key"), key); err != nil {
		return err
	}
	if err := writePEM(filepath.Join(*out, "ca.pem"), "CERTIFICATE", der, 0o644); err != nil {
		return err
	}
	fmt.Printf("CA written to %s; keep ca.key offline\n", *out)
	return nil
}

func cmdNode(args []string) error {
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	caDir := fs.String("ca", "", "CA directory from qkdpki ca (required)")
	id := fs.String("id", "", "node ID, also the certificate common name (required)")
	hosts := fs.String("hosts", "", "comma-separated DNS names and IPs this node is reached at (required)")
	out := fs.String("out", "", "output directory (required)")
	years := fs.Int("years", 2, "validity in years")
	fs.Parse(args)
	if *caDir == "" || *id == "" || *hosts == "" || *out == "" {
		return errors.New("node: -ca, -id, -hosts and -out are required")
	}
	ca, caKey, err := loadCA(*caDir)
	if err != nil {
		return err
	}
	hostList := append(strings.Split(*hosts, ","), "localhost", "127.0.0.1")
	both := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	// three identities, one per container: kms talks to other nodes, app manages only this
	// node, ui only serves HTTPS; none can act as another
	parts := []struct {
		dir, name, role string
		hosts           []string
		usage           []x509.ExtKeyUsage
	}{
		{"kms", "node", roleKME, append(hostList, "kms"), both},
		{"app", "admin", roleAdmin, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}},
		{"ui", "ui", roleUI, hostList, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}},
	}
	for _, p := range parts {
		if err := issue(ca, caKey, filepath.Join(*out, p.dir), p.name, *id, p.role, p.hosts, p.usage, *years); err != nil {
			return err
		}
	}
	// Master key encrypts the node's stores at rest; an existing one is kept, since replacing it
	// would make every key already stored unreadable
	masterPath := filepath.Join(*out, "kms", "master.key")
	if _, err := os.Stat(masterPath); errors.Is(err, os.ErrNotExist) {
		mk := make([]byte, 32)
		if _, err := rand.Read(mk); err != nil {
			return err
		}
		if err := os.WriteFile(masterPath, []byte(base64.StdEncoding.EncodeToString(mk)), 0o600); err != nil {
			return err
		}
	}
	fmt.Printf("node %s written to %s (hosts %s)\n", *id, *out, strings.Join(hostList, ", "))
	return nil
}

func cmdSAE(args []string) error {
	fs := flag.NewFlagSet("sae", flag.ExitOnError)
	caDir := fs.String("ca", "", "CA directory from qkdpki ca (required)")
	id := fs.String("id", "", "SAE ID, as listed in the network's saes (required)")
	out := fs.String("out", "", "output directory (required)")
	years := fs.Int("years", 2, "validity in years")
	fs.Parse(args)
	if *caDir == "" || *id == "" || *out == "" {
		return errors.New("sae: -ca, -id and -out are required")
	}
	ca, caKey, err := loadCA(*caDir)
	if err != nil {
		return err
	}
	if err := issue(ca, caKey, *out, "sae", *id, roleSAE, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, *years); err != nil {
		return err
	}
	fmt.Printf("SAE %s written to %s\n", *id, *out)
	return nil
}

func cmdController(args []string) error {
	fs := flag.NewFlagSet("controller", flag.ExitOnError)
	caDir := fs.String("ca", "", "CA directory from qkdpki ca (required)")
	hosts := fs.String("hosts", "", "comma-separated DNS names and IPs the controller is reached at (required)")
	out := fs.String("out", "", "output directory (required)")
	years := fs.Int("years", 2, "validity in years")
	fs.Parse(args)
	if *caDir == "" || *hosts == "" || *out == "" {
		return errors.New("controller: -ca, -hosts and -out are required")
	}
	ca, caKey, err := loadCA(*caDir)
	if err != nil {
		return err
	}
	hostList := append(strings.Split(*hosts, ","), "localhost", "127.0.0.1", "controller")
	if err := issue(ca, caKey, *out, "controller", "controller", roleController, hostList,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, *years); err != nil {
		return err
	}
	if err := issue(ca, caKey, *out, "operator", "operator", roleOperator, nil,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, *years); err != nil {
		return err
	}
	fmt.Printf("controller written to %s (hosts %s), with an operator certificate\n", *out, strings.Join(hostList, ", "))
	return nil
}

// issue writes <dir>/<name>.pem, <dir>/<name>.key and <dir>/ca.pem for one certificate
func issue(ca *x509.Certificate, caKey *ecdsa.PrivateKey, dir, name, cn, role string, hosts []string, usage []x509.ExtKeyUsage, years int) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(), Subject: pkix.Name{CommonName: cn, OrganizationalUnit: []string{role}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(years, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usage,
	}
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writeKey(filepath.Join(dir, name+".key"), key); err != nil {
		return err
	}
	if err := writePEM(filepath.Join(dir, name+".pem"), "CERTIFICATE", der, 0o644); err != nil {
		return err
	}
	return writePEM(filepath.Join(dir, "ca.pem"), "CERTIFICATE", ca.Raw, 0o644)
}

func loadCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		return nil, nil, err
	}
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, nil, fmt.Errorf("%s: ca.pem or ca.key is not PEM", dir)
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func randomSerial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err)
	}
	return n
}

func writeKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return writePEM(path, "EC PRIVATE KEY", der, 0o600)
}

func writePEM(path, kind string, der []byte, mode os.FileMode) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), mode)
}

type network struct {
	Nodes map[string]struct {
		URL string `json:"kme_url"`
	} `json:"nodes"`
	Links [][2]string       `json:"links"`
	SAEs  map[string]string `json:"saes"`
}

func cmdNet(args []string) error {
	fs := flag.NewFlagSet("net", flag.ExitOnError)
	file := fs.String("file", "network.json", "network description to edit")
	fs.Parse(args)
	a := fs.Args()
	if len(a) == 0 {
		return errors.New("net: give add-node, add-link, add-sae, remove-sae or show")
	}
	n := network{Nodes: map[string]struct {
		URL string `json:"kme_url"`
	}{}, SAEs: map[string]string{}}
	if data, err := os.ReadFile(*file); err == nil {
		if err := json.Unmarshal(data, &n); err != nil {
			return fmt.Errorf("%s: %w", *file, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if n.SAEs == nil {
		n.SAEs = map[string]string{}
	}
	need := func(k int) error {
		if len(a) != k+1 {
			return fmt.Errorf("net %s takes %d arguments", a[0], k)
		}
		return nil
	}
	switch a[0] {
	case "add-node":
		if err := need(2); err != nil {
			return err
		}
		n.Nodes[a[1]] = struct {
			URL string `json:"kme_url"`
		}{a[2]}
	case "add-link":
		if err := need(2); err != nil {
			return err
		}
		for _, id := range a[1:3] {
			if _, ok := n.Nodes[id]; !ok {
				return fmt.Errorf("net: add node %s first", id)
			}
		}
		for _, l := range n.Links {
			if l == [2]string{a[1], a[2]} || l == [2]string{a[2], a[1]} {
				return nil
			}
		}
		n.Links = append(n.Links, [2]string{a[1], a[2]})
	case "add-sae":
		if err := need(2); err != nil {
			return err
		}
		if _, ok := n.Nodes[a[2]]; !ok {
			return fmt.Errorf("net: add node %s first", a[2])
		}
		if _, ok := n.Nodes[a[1]]; ok {
			return fmt.Errorf("net: %s is a node ID", a[1])
		}
		n.SAEs[a[1]] = a[2]
	case "remove-sae":
		if err := need(1); err != nil {
			return err
		}
		delete(n.SAEs, a[1])
	case "show":
	default:
		return fmt.Errorf("net: unknown action %s", a[0])
	}
	data, _ := json.MarshalIndent(n, "", "  ")
	if a[0] == "show" {
		fmt.Println(string(data))
		return nil
	}
	return os.WriteFile(*file, append(data, '\n'), 0o644)
}
