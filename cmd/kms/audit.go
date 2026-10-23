// Audit trail: one JSON line per key event, appended to disk and kept in a short in-memory tail
// for the dashboard; rotated by size so it never fills the disk. Key IDs and SAE IDs only, never key material
package main

import (
	"encoding/json"
	"fmt"
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

// audit.log moves to audit.log.1 past auditMaxBytes, each older file one up, keeping auditKeep of them (the
// -audit-max-mb and -audit-keep flags); auditMaxBytes 0 never rotates
var (
	auditMaxBytes int64 = 64 << 20
	auditKeep           = 16
)

type auditLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
	tail []auditEvent
}

func openAudit(path string) (*auditLog, error) {
	a := &auditLog{path: path}
	return a, a.open()
}

func (a *auditLog) open() error {
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	a.f, a.size = f, st.Size()
	return nil
}

// rotate moves audit.log to audit.log.1 and each older file one up, the one past auditKeep dropped; caller holds mu
func (a *auditLog) rotate() {
	a.f.Close()
	keep := max(auditKeep, 1)
	_ = os.Remove(fmt.Sprintf("%s.%d", a.path, keep))
	for i := keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", a.path, i), fmt.Sprintf("%s.%d", a.path, i+1)) // a missing one is fine
	}
	if err := os.Rename(a.path, a.path+".1"); err != nil {
		log.Printf("audit: rotate: %v", err)
	}
	if err := a.open(); err != nil {
		log.Printf("audit: reopen after rotating: %v", err)
	}
}

func (a *auditLog) log(e auditEvent) {
	e.Time = time.Now().UTC()
	line, _ := json.Marshal(e)
	a.mu.Lock()
	defer a.mu.Unlock()
	n, err := a.f.Write(append(line, '\n'))
	if err != nil {
		log.Printf("audit: %v", err)
	}
	if a.size += int64(n); auditMaxBytes > 0 && a.size >= auditMaxBytes {
		a.rotate()
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
