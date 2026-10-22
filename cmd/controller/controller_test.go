package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func newTestController(t *testing.T) (*controller, *httptest.Server) {
	t.Helper()
	st, err := openStore(filepath.Join(t.TempDir(), "network.json"))
	if err != nil {
		t.Fatal(err)
	}
	c := &controller{st: st, dev: true, now: time.Now, ready: func() error { return nil }}
	srv := httptest.NewServer(c.routes())
	t.Cleanup(srv.Close)
	return c, srv
}

func call(t *testing.T, srv *httptest.Server, method, path, role, id, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("X-Dev-Role", role)
	req.Header.Set("X-Dev-ID", id)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestConfigFollowsLinks(t *testing.T) {
	d := Desired{Settings: defaultSettings, Nodes: map[string]Node{}, Links: map[string]Link{}}
	for _, id := range []string{"A", "B", "C"} {
		if err := d.putNode(id, Node{Site: id, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(d.putLink("A-B", Link{A: "A", B: "B", Announcer: "A", Active: true}))
	must(d.putLink("C-A", Link{A: "C", B: "A", Announcer: "A", Active: true}))
	must(d.putLink("B-C", Link{A: "B", B: "C", Announcer: "C", Active: false}))

	if got := d.configFor("A"); !got.Managed || !got.Active || !reflect.DeepEqual(got.AnnounceTo, []string{"B", "C"}) {
		t.Fatalf("A: %+v", got)
	}
	if got := d.configFor("C"); !got.Active || len(got.AnnounceTo) != 0 {
		t.Fatalf("C answers A only, its own link is off: %+v", got)
	}
	n := d.Nodes["B"]
	n.Enabled = false
	d.Nodes["B"] = n
	if got := d.configFor("A"); !reflect.DeepEqual(got.AnnounceTo, []string{"C"}) {
		t.Fatalf("a disabled neighbor gets no blocks: %+v", got)
	}
	if got := d.configFor("B"); !got.Managed || got.Active {
		t.Fatalf("a disabled node is managed and off: %+v", got)
	}
	if got := d.configFor("Z"); got.Managed {
		t.Fatalf("a node never added is not managed: %+v", got)
	}
}

func TestLinkValidation(t *testing.T) {
	d := Desired{Nodes: map[string]Node{"A": {Site: "A"}, "B": {Site: "B"}}, Links: map[string]Link{}}
	for name, l := range map[string]Link{
		"unknown node":    {A: "A", B: "Z", Announcer: "A"},
		"self link":       {A: "A", B: "A", Announcer: "A"},
		"other announcer": {A: "A", B: "B", Announcer: "C"},
	} {
		if d.putLink("x", l) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := d.putLink("A-B", Link{A: "A", B: "B", Announcer: "B"}); err != nil {
		t.Fatal(err)
	}
	if d.putLink("B-A", Link{A: "B", B: "A", Announcer: "B"}) == nil {
		t.Error("a second link between the same nodes accepted")
	}
	if d.deleteNode("A") == nil {
		t.Error("a node still in a link deleted")
	}
}

func TestHeartbeatAndRoles(t *testing.T) {
	c, srv := newTestController(t)
	if code, _ := call(t, srv, "PUT", "/v1/nodes/A", roleAdmin, "A", `{"site":"A"}`); code != http.StatusForbidden {
		t.Fatalf("a node changed the network: %d", code)
	}
	for _, step := range []struct{ path, body string }{
		{"/v1/nodes/A", `{"site":"A"}`}, {"/v1/nodes/B", `{"site":"B"}`},
		{"/v1/links/A-B", `{"a":"A","b":"B"}`}, {"/v1/settings", `{"block_s":600}`},
	} {
		if code, out := call(t, srv, "PUT", step.path, roleOperator, "ops", step.body); code != http.StatusOK {
			t.Fatalf("%s: %d %v", step.path, code, out)
		}
	}
	if code, _ := call(t, srv, "POST", "/v1/agents/B/heartbeat", roleAdmin, "A", `{}`); code != http.StatusForbidden {
		t.Fatalf("node A sent B's heartbeat: %d", code)
	}
	code, out := call(t, srv, "POST", "/v1/agents/A/heartbeat", roleAdmin, "A", `{"recording":true,"blocks":{"B":{"ok":true,"qber":0.04,"key_bits":26000}}}`)
	if code != http.StatusOK {
		t.Fatalf("heartbeat: %d %v", code, out)
	}
	cfg := out["config"].(map[string]any)
	if cfg["managed"] != true || cfg["active"] != true || cfg["block_s"].(float64) != 600 || cfg["announce_to"].([]any)[0] != "B" {
		t.Fatalf("config %v", cfg)
	}
	s := c.stats()
	if s["nodes_up"] != 1 || s["link_qber_A-B"] != 0.04 || s["link_key_bits_A-B"] != 26000 || s["node_recording_A"] != 1 {
		t.Fatalf("stats %v", s)
	}
}

func TestStatePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "network.json")
	st, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.change(func(d *Desired) error { return d.putNode("A", Node{Site: "A", Enabled: true}) }); err != nil {
		t.Fatal(err)
	}
	if err := st.change(func(d *Desired) error { return d.putNode("bad id!", Node{Site: "x"}) }); err == nil {
		t.Fatal("invalid node accepted")
	}
	again, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if d := again.current(); d.Revision != 1 || len(d.Nodes) != 1 {
		t.Fatalf("reopened %+v", d)
	}
}
