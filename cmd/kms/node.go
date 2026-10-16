// Node-level endpoints, the key importer and the expiry sweeper: what a dashboard needs to see one
// node and its links, and the bridge from the pipeline's plaintext output into the encrypted pools
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"qkd_post_processing/kms"
)

type linkStatus struct {
	Neighbor string     `json:"neighbor"`
	PeerURL  string     `json:"peer_url"`
	Pool     kms.Status `json:"pool"`
	// Mine and Theirs split the pool's available keys by which end may originate with them
	Mine      int     `json:"mine"`
	Theirs    int     `json:"theirs"`
	Reachable *bool   `json:"reachable,omitempty"`
	RTTms     float64 `json:"rtt_ms,omitempty"`
	Error     string  `json:"error,omitempty"`
}

type nodeStatus struct {
	NodeID string            `json:"node_id"`
	Local  kms.Status        `json:"local"`
	Links  []linkStatus      `json:"links"`
	Routes map[string]string `json:"routes"`
	SAEs   []string          `json:"saes"`
}

func (s *server) status() nodeStatus {
	st := nodeStatus{NodeID: s.id, Routes: s.node.NextHop}
	st.Local, _ = s.local.Status()
	for _, nb := range s.net.neighbors(s.id) {
		p := s.pools[nb]
		ls := linkStatus{Neighbor: nb, PeerURL: s.net.Nodes[nb].URL,
			Mine: p.CountAvailable(kms.OriginatorOf(s.id, nb)), Theirs: p.CountAvailable(kms.OriginatorOf(nb, s.id))}
		ls.Pool, _ = p.Status()
		st.Links = append(st.Links, ls)
	}
	for sae, home := range s.net.SAEs {
		if home == s.id {
			st.SAEs = append(st.SAEs, sae)
		}
	}
	return st
}

// handleNode is GET /kme/v1/node, for any node of the network: counts only, never key material;
// this node's own dashboard may add ?probe=1 to check every neighbor answers over mutual TLS
func (s *server) handleNode(w http.ResponseWriter, r *http.Request) {
	c := s.who(r)
	if _, known := s.net.Nodes[c.id]; !(c.role == "kme" && known) && !s.isAdmin(r) {
		writeError(w, http.StatusForbidden, "node status is for KMEs of this network and this node's dashboard")
		return
	}
	st := s.status()
	if r.URL.Query().Get("probe") == "1" && s.isAdmin(r) {
		s.probe(r.Context(), st.Links)
	}
	writeJSON(w, http.StatusOK, st)
}

// probe asks every neighbor's /kme/v1/node in parallel, two seconds each
func (s *server) probe(ctx context.Context, links []linkStatus) {
	var wg sync.WaitGroup
	for i := range links {
		wg.Add(1)
		go func(l *linkStatus) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			start := time.Now()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, l.PeerURL+"/kme/v1/node", nil)
			if s.dev {
				req.Header.Set("X-Dev-Role", "kme")
				req.Header.Set("X-Dev-ID", s.id)
			}
			ok := false
			resp, err := s.client.Do(req)
			if err == nil {
				resp.Body.Close()
				switch {
				case resp.StatusCode != http.StatusOK:
					err = errors.New(resp.Status)
				case resp.TLS != nil && resp.TLS.PeerCertificates[0].Subject.CommonName != l.Neighbor:
					err = errors.New("answered with another node's certificate")
				default:
					ok = true
				}
			}
			l.Reachable, l.RTTms = &ok, float64(time.Since(start).Microseconds())/1000
			if err != nil {
				l.Error = err.Error()
			}
		}(&links[i])
	}
	wg.Wait()
}

// handleNetwork is GET /kme/v1/network, admin only: the shared description plus this node's routes
func (s *server) handleNetwork(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeError(w, http.StatusForbidden, "management is for this node's admin certificate")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"self": s.id, "network": s.net, "routes": s.node.NextHop})
}

// handleAudit is GET /kme/v1/audit?n=: the most recent key events, newest first
func (s *server) handleAudit(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeError(w, http.StatusForbidden, "management is for this node's admin certificate")
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 || n > auditTail {
		n = 200
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": s.audit.last(n)})
}

// keyTTL is how long imported pool keys stay usable
var keyTTL = 30 * 24 * time.Hour

var keyFileName = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.json$`)

// importKeys moves every settled ETSI record under root/<neighbor> into that link's pool, deleting
// each plaintext file once it is stored encrypted
func (s *server) importKeys(root string) {
	for nb, pool := range s.pools {
		dir := filepath.Join(root, nb)
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		imported := 0
		for _, f := range files {
			info, err := f.Info()
			// the pipeline writes records non-atomically, so a file younger than a second may be partial
			if err != nil || !keyFileName.MatchString(f.Name()) || time.Since(info.ModTime()) < time.Second {
				continue
			}
			id := strings.TrimSuffix(f.Name(), ".json")
			key, err := kms.LoadKey(dir, id)
			if err != nil {
				continue
			}
			err = pool.PutWithID(id, key, keyTTL)
			clear(key)
			if err != nil && !strings.Contains(err.Error(), "already exists") {
				log.Printf("import %s/%s: %v", nb, id, err)
				continue
			}
			os.Remove(filepath.Join(dir, f.Name()))
			imported++
		}
		if imported > 0 {
			s.audit.log(auditEvent{Event: "imported", Peer: nb, Count: imported})
		}
	}
}

// runImporter creates root/<neighbor> for every link and imports every two seconds
func (s *server) runImporter(root string) {
	for nb := range s.pools {
		os.MkdirAll(filepath.Join(root, nb), 0o700)
	}
	for {
		s.importKeys(root)
		time.Sleep(2 * time.Second)
	}
}

// runSweeper wipes expired key material from every store every ten minutes
func (s *server) runSweeper() {
	for {
		stores := map[string]*kms.KMS{"local": s.local}
		for nb, p := range s.pools {
			stores["pool "+nb] = p
		}
		for name, st := range stores {
			if n, err := st.Sweep(); err != nil {
				log.Printf("sweep %s: %v", name, err)
			} else if n > 0 {
				s.audit.log(auditEvent{Event: "expired", Peer: name, Count: n})
			}
		}
		time.Sleep(10 * time.Minute)
	}
}
