// Post-processing mailbox: carries the public messages of two-party key extraction between the
// dashboards of two neighbors, over the KMEs' mutual TLS; no key material ever passes through it
package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"
)

const (
	ppMaxMessage = 128 << 20 // an announcement of one recording's timing fits comfortably
	ppMaxQueued  = 512 << 20
	ppIdleExpiry = time.Hour
)

var ppName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

type ppMessage struct {
	Kind string          `json:"kind"`
	Body json.RawMessage `json:"body"`
}

type ppQueue struct {
	msgs []ppMessage
	last time.Time
}

// mailbox holds messages from neighbors, queued per sending node and session, until this
// node's dashboard fetches them
type mailbox struct {
	mu     sync.Mutex
	queues map[[2]string]*ppQueue
	bytes  int
	notify chan struct{}
}

func newMailbox() *mailbox {
	return &mailbox{queues: map[[2]string]*ppQueue{}, notify: make(chan struct{})}
}

func (m *mailbox) put(from, session string, msg ppMessage) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expire()
	if m.bytes+len(msg.Body) > ppMaxQueued {
		return false
	}
	k := [2]string{from, session}
	q := m.queues[k]
	if q == nil {
		q = &ppQueue{}
		m.queues[k] = q
	}
	q.msgs, q.last = append(q.msgs, msg), time.Now()
	m.bytes += len(msg.Body)
	close(m.notify)
	m.notify = make(chan struct{})
	return true
}

// take pops the oldest message of a session, waiting up to wait for one to arrive
func (m *mailbox) take(from, session string, wait time.Duration) (ppMessage, bool) {
	deadline := time.Now().Add(wait)
	for {
		m.mu.Lock()
		if q := m.queues[[2]string{from, session}]; q != nil && len(q.msgs) > 0 {
			msg := q.msgs[0]
			q.msgs, q.last = q.msgs[1:], time.Now()
			m.bytes -= len(msg.Body)
			m.mu.Unlock()
			return msg, true
		}
		ch := m.notify
		m.mu.Unlock()
		left := time.Until(deadline)
		if left <= 0 {
			return ppMessage{}, false
		}
		select {
		case <-ch:
		case <-time.After(left):
		}
	}
}

// expire drops sessions nobody touched for an hour; m.mu held
func (m *mailbox) expire() {
	for k, q := range m.queues {
		if time.Since(q.last) > ppIdleExpiry {
			for _, msg := range q.msgs {
				m.bytes -= len(msg.Body)
			}
			delete(m.queues, k)
		}
	}
}

type ppSession struct {
	Peer      string `json:"peer"`
	Session   string `json:"session"`
	Pending   int    `json:"pending"`
	FirstKind string `json:"first_kind,omitempty"`
}

func (m *mailbox) sessions() []ppSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expire()
	out := []ppSession{}
	for k, q := range m.queues {
		s := ppSession{Peer: k[0], Session: k[1], Pending: len(q.msgs)}
		if len(q.msgs) > 0 {
			s.FirstKind = q.msgs[0].Kind
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Peer+out[i].Session < out[j].Peer+out[j].Session })
	return out
}

// handlePPSend is POST /kme/v1/pp/send from this node's dashboard: forwards one message to a neighbor
func (s *server) handlePPSend(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeError(w, http.StatusForbidden, "management is for this node's admin certificate")
		return
	}
	var req struct {
		Peer, Session string
		ppMessage
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid message: "+err.Error())
		return
	}
	if s.pools[req.Peer] == nil || !ppName.MatchString(req.Session) || !ppName.MatchString(req.Kind) {
		writeError(w, http.StatusBadRequest, "the peer must be a neighbor, session and kind short names")
		return
	}
	fwd := struct {
		Session string `json:"session"`
		ppMessage
	}{req.Session, req.ppMessage}
	if err := s.kmePost(req.Peer, "/kme/v1/pp/inbox", fwd, nil); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

// handlePPInbox is POST /kme/v1/pp/inbox from a neighbor's KME
func (s *server) handlePPInbox(w http.ResponseWriter, r *http.Request) {
	c := s.who(r)
	if c.role != "kme" || s.pools[c.id] == nil {
		writeError(w, http.StatusForbidden, "post-processing messages are accepted from neighbors only")
		return
	}
	var req struct {
		Session string `json:"session"`
		ppMessage
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !ppName.MatchString(req.Session) || !ppName.MatchString(req.Kind) {
		writeError(w, http.StatusBadRequest, "invalid message")
		return
	}
	if !s.mail.put(c.id, req.Session, req.ppMessage) {
		writeError(w, http.StatusServiceUnavailable, "mailbox full")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "queued"})
}

// handlePPRecv is GET /kme/v1/pp/recv?peer=&session=&wait=: the next message, or 204 after wait seconds
func (s *server) handlePPRecv(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeError(w, http.StatusForbidden, "management is for this node's admin certificate")
		return
	}
	wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	wait = max(0, min(wait, 60))
	msg, ok := s.mail.take(r.URL.Query().Get("peer"), r.URL.Query().Get("session"), time.Duration(wait)*time.Second)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

// handlePPSessions is GET /kme/v1/pp/sessions: sessions with queued messages, so the dashboard
// notices a neighbor starting a two-party extraction
func (s *server) handlePPSessions(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeError(w, http.StatusForbidden, "management is for this node's admin certificate")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": s.mail.sessions()})
}
