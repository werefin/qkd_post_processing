// Audit trail: one JSON line per key event, appended to disk and kept in a short in-memory tail
// for the dashboard. Key IDs and SAE IDs only, never key material
package main

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

type auditEvent struct {
	Time   time.Time `json:"time"`
	Event  string    `json:"event"`
	KeyID  string    `json:"key_ID,omitempty"`
	Master string    `json:"master_SAE_ID,omitempty"`
	Slave  string    `json:"slave_SAE_ID,omitempty"`
	Peer   string    `json:"peer,omitempty"`
	Bits   int       `json:"bits,omitempty"`
	Count  int       `json:"count,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

const auditTail = 1000

type auditLog struct {
	mu   sync.Mutex
	f    *os.File
	tail []auditEvent
}

func openAudit(path string) (*auditLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &auditLog{f: f}, nil
}

func (a *auditLog) log(e auditEvent) {
	e.Time = time.Now().UTC()
	line, _ := json.Marshal(e)
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.f.Write(append(line, '\n')); err != nil {
		log.Printf("audit: %v", err)
	}
	a.tail = append(a.tail, e)
	if len(a.tail) > auditTail {
		a.tail = a.tail[len(a.tail)-auditTail:]
	}
}

// last returns up to n most recent events, newest first
func (a *auditLog) last(n int) []auditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n > len(a.tail) {
		n = len(a.tail)
	}
	out := make([]auditEvent, n)
	for i := range n {
		out[i] = a.tail[len(a.tail)-1-i]
	}
	return out
}
