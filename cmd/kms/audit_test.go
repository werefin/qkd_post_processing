package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestAuditRotates: past the size limit audit.log moves to audit.log.1 and the older files one up, never more than
// auditKeep of them, and every event of the files kept is still there, in order
func TestAuditRotates(t *testing.T) {
	oldMax, oldKeep := auditMaxBytes, auditKeep
	auditMaxBytes, auditKeep = 2000, 3
	defer func() { auditMaxBytes, auditKeep = oldMax, oldKeep }()

	path := filepath.Join(t.TempDir(), "audit.log")
	a, err := openAudit(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 200 {
		a.log(auditEvent{Event: "imported", Peer: "B", Count: i})
	}

	var counts []int
	for i := auditKeep; i >= 0; i-- {
		name := path
		if i > 0 {
			name = fmt.Sprintf("%s.%d", path, i)
		}
		f, err := os.Open(name)
		if err != nil {
			t.Fatalf("%s missing: %v", filepath.Base(name), err)
		}
		st, _ := f.Stat()
		if st.Size() > auditMaxBytes+200 {
			t.Fatalf("%s holds %d bytes, past the %d limit", filepath.Base(name), st.Size(), auditMaxBytes)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var e auditEvent
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
				t.Fatalf("%s: %v", filepath.Base(name), err)
			}
			counts = append(counts, e.Count)
		}
		if err := sc.Err(); err != nil {
			t.Fatalf("%s: %v", filepath.Base(name), err)
		}
		f.Close()
	}
	if _, err := os.Stat(fmt.Sprintf("%s.%d", path, auditKeep+1)); err == nil {
		t.Fatalf("more than %d rotated files kept", auditKeep)
	}
	for i := 1; i < len(counts); i++ {
		if counts[i] != counts[i-1]+1 {
			t.Fatalf("events out of order or lost across a rotation: %d then %d", counts[i-1], counts[i])
		}
	}
	if counts[len(counts)-1] != 199 || len(counts) >= 200 {
		t.Fatalf("kept %d events ending at %d: want the newest, the oldest dropped", len(counts), counts[len(counts)-1])
	}
	if got := a.last(1); len(got) != 1 || got[0].Count != 199 {
		t.Fatalf("in-memory tail lost across rotations: %+v", got)
	}
}
