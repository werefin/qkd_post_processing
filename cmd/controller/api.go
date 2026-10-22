package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Certificate roles the controller answers: operators change the network, each node's admin certificate is
// that node's agent and may only send its own heartbeat and read the network
const (
	roleOperator = "operator"
	roleAdmin    = "admin"
)

const upAfter = 45 * time.Second // a node whose last heartbeat is older counts as down

type caller struct{ role, id string }

func (c *controller) who(r *http.Request) caller {
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		cert := r.TLS.PeerCertificates[0]
		if len(cert.Subject.OrganizationalUnit) == 1 {
			return caller{cert.Subject.OrganizationalUnit[0], cert.Subject.CommonName}
		}
		return caller{}
	}
	if c.dev {
		return caller{r.Header.Get("X-Dev-Role"), r.Header.Get("X-Dev-ID")}
	}
	return caller{}
}

type controller struct {
	st    *store
	dev   bool // plain HTTP: identities come from headers, development only
	now   func() time.Time
	ready func() error
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// allow wraps a handler with the roles that may call it
func (c *controller) allow(h http.HandlerFunc, roles ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		role := c.who(r).role
		for _, ok := range roles {
			if role == ok {
				h(w, r)
				return
			}
		}
		writeError(w, http.StatusForbidden, "this certificate may not call "+r.URL.Path)
	}
}

func (c *controller) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", c.handleReady)
	// southbound: each node's agent
	mux.HandleFunc("POST /v1/agents/{id}/heartbeat", c.allow(c.handleHeartbeat, roleAdmin))
	// northbound: the operator, and read-only for the nodes
	mux.HandleFunc("GET /v1/network", c.allow(c.handleNetwork, roleOperator, roleAdmin))
	mux.HandleFunc("GET /v1/events", c.allow(c.handleEvents, roleOperator, roleAdmin))
	mux.HandleFunc("GET /v1/stats", c.allow(c.handleStats, roleOperator, roleAdmin))
	mux.HandleFunc("PUT /v1/settings", c.allow(c.handleSettings, roleOperator))
	mux.HandleFunc("PUT /v1/nodes/{id}", c.allow(c.handlePutNode, roleOperator))
	mux.HandleFunc("DELETE /v1/nodes/{id}", c.allow(c.handleDeleteNode, roleOperator))
	mux.HandleFunc("PUT /v1/links/{name}", c.allow(c.handlePutLink, roleOperator))
	mux.HandleFunc("DELETE /v1/links/{name}", c.allow(c.handleDeleteLink, roleOperator))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		mux.ServeHTTP(w, r)
	})
}

func (c *controller) handleReady(w http.ResponseWriter, r *http.Request) {
	if err := c.ready(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ready": true})
}

func (c *controller) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if c.who(r).id != id {
		writeError(w, http.StatusForbidden, "a node sends only its own heartbeat")
		return
	}
	var status json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&status); err != nil || len(status) == 0 || status[0] != '{' {
		writeError(w, http.StatusBadRequest, "the status is a JSON object")
		return
	}
	cfg, rev := c.st.heartbeat(id, status, c.now())
	writeJSON(w, http.StatusOK, map[string]any{"config": cfg, "revision": rev})
}

// nodeView is one node as the network view shows it: desired, the config it gets, and what it last reported
type nodeView struct {
	Node
	Known    bool            `json:"known"`
	Up       bool            `json:"up"`
	LastSeen *time.Time      `json:"last_seen,omitempty"`
	Config   NodeConfig      `json:"config"`
	Status   json.RawMessage `json:"status,omitempty"`
}

type networkView struct {
	Revision int64               `json:"revision"`
	Settings Settings            `json:"settings"`
	Nodes    map[string]nodeView `json:"nodes"`
	Links    map[string]Link     `json:"links"`
	Time     time.Time           `json:"time"`
}

// view is the whole network; nodes that send heartbeats without having been added show as unknown
func (c *controller) view() networkView {
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	now := c.now()
	v := networkView{Revision: c.st.desired.Revision, Settings: c.st.desired.Settings, Nodes: map[string]nodeView{}, Links: map[string]Link{}, Time: now}
	for name, l := range c.st.desired.Links {
		v.Links[name] = l
	}
	for id, n := range c.st.desired.Nodes {
		v.Nodes[id] = nodeView{Node: n, Known: true, Config: c.st.desired.configFor(id)}
	}
	for id, o := range c.st.observed {
		nv := v.Nodes[id]
		seen := o.LastSeen
		nv.LastSeen, nv.Status, nv.Up = &seen, o.Status, now.Sub(o.LastSeen) < upAfter
		if !nv.Known {
			nv.Config = NodeConfig{AnnounceTo: []string{}}
		}
		v.Nodes[id] = nv
	}
	return v
}

func (c *controller) handleNetwork(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, c.view())
}

// handleEvents streams the network view as server-sent events, on every change and at least every 15 s
func (c *controller) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	changed, done := c.st.subscribe()
	defer done()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		data, _ := json.Marshal(c.view())
		if _, err := fmt.Fprintf(w, "event: network\ndata: %s\n\n", data); err != nil {
			return
		}
		flusher.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-changed:
		case <-tick.C:
		}
	}
}

// statusSummary is the part of an agent's status the stats read
type statusSummary struct {
	Recording *bool `json:"recording"`
	Alerts    []struct {
		Level string `json:"level"`
	} `json:"alerts"`
	Blocks map[string]struct {
		OK      bool     `json:"ok"`
		QBER    *float64 `json:"qber"`
		KeyBits *float64 `json:"key_bits"`
	} `json:"blocks"`
}

// stats are flat numbers for monitoring: the network's, then per node and per link as the announcer reports it
func (c *controller) stats() map[string]float64 {
	v := c.view()
	b := func(x bool) float64 {
		if x {
			return 1
		}
		return 0
	}
	out := map[string]float64{"nodes": float64(len(v.Nodes)), "revision": float64(v.Revision)}
	up, active := 0.0, 0.0
	for id, n := range v.Nodes {
		up += b(n.Up)
		out["node_up_"+id] = b(n.Up)
		var s statusSummary
		if json.Unmarshal(n.Status, &s) != nil {
			continue
		}
		if s.Recording != nil {
			out["node_recording_"+id] = b(*s.Recording)
		}
		for _, a := range s.Alerts {
			out["node_alerts_"+a.Level+"_"+id]++
		}
	}
	for name, l := range v.Links {
		active += b(l.Active)
		out["link_active_"+name] = b(l.Active)
		var s statusSummary
		if json.Unmarshal(v.Nodes[l.Announcer].Status, &s) != nil {
			continue
		}
		other := l.B
		if l.Announcer == l.B {
			other = l.A
		}
		if blk, ok := s.Blocks[other]; ok {
			out["link_block_ok_"+name] = b(blk.OK)
			if blk.QBER != nil {
				out["link_qber_"+name] = *blk.QBER
			}
			if blk.KeyBits != nil {
				out["link_key_bits_"+name] = *blk.KeyBits
			}
		}
	}
	out["nodes_up"], out["links_active"] = up, active
	return out
}

func (c *controller) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, c.stats())
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

// answer reports a change's outcome: the new network view, or why it was refused
func (c *controller) answer(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, "no such node or link")
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeJSON(w, http.StatusOK, c.view())
	}
}

func (c *controller) handleSettings(w http.ResponseWriter, r *http.Request) {
	s := c.st.current().Settings
	if !decode(w, r, &s) {
		return
	}
	c.answer(w, c.st.change(func(d *Desired) error {
		if err := s.validate(); err != nil {
			return err
		}
		d.Settings = s
		return nil
	}))
}

func (c *controller) handlePutNode(w http.ResponseWriter, r *http.Request) {
	n := Node{Enabled: true}
	if !decode(w, r, &n) {
		return
	}
	c.answer(w, c.st.change(func(d *Desired) error { return d.putNode(r.PathValue("id"), n) }))
}

func (c *controller) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	c.answer(w, c.st.change(func(d *Desired) error { return d.deleteNode(r.PathValue("id")) }))
}

func (c *controller) handlePutLink(w http.ResponseWriter, r *http.Request) {
	l := Link{Active: true}
	if !decode(w, r, &l) {
		return
	}
	if l.Announcer == "" {
		l.Announcer = l.A
	}
	c.answer(w, c.st.change(func(d *Desired) error { return d.putLink(r.PathValue("name"), l) }))
}

func (c *controller) handleDeleteLink(w http.ResponseWriter, r *http.Request) {
	c.answer(w, c.st.change(func(d *Desired) error {
		if _, ok := d.Links[r.PathValue("name")]; !ok {
			return errNotFound
		}
		delete(d.Links, r.PathValue("name"))
		return nil
	}))
}
