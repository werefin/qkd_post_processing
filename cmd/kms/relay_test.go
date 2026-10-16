package main

import (
	"reflect"
	"strings"
	"testing"
)

func testConfig(links [][2]string, saes map[string]string) *networkConfig {
	n := &networkConfig{Links: links, SAEs: saes, Nodes: map[string]struct {
		URL string `json:"kme_url"`
	}{}}
	for _, l := range links {
		for _, id := range l {
			n.Nodes[id] = struct {
				URL string `json:"kme_url"`
			}{"https://" + strings.ToLower(id)}
		}
	}
	return n
}

// TestNextHopsFollowShortestPaths: routes come from the links alone, the same on every run
func TestNextHopsFollowShortestPaths(t *testing.T) {
	// A - B - C - D, plus a shortcut A - D
	n := testConfig([][2]string{{"A", "B"}, {"B", "C"}, {"C", "D"}, {"A", "D"}}, nil)
	got := n.nextHops("A")
	want := map[string]string{"B": "B", "D": "D", "C": "B"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("A: got %v, want %v", got, want)
	}
	if got := n.nextHops("C"); got["A"] != "B" {
		t.Fatalf("C to A should go via B (alphabetical tie-break), got %v", got)
	}
	isolated := testConfig([][2]string{{"A", "B"}, {"C", "D"}}, nil)
	if _, ok := isolated.nextHops("A")["C"]; ok {
		t.Fatal("a route to an unconnected node")
	}
}

// TestNetworkValidation: inconsistent descriptions are refused at start-up
func TestNetworkValidation(t *testing.T) {
	bad := []*networkConfig{
		testConfig([][2]string{{"A", "A"}}, nil),
		testConfig([][2]string{{"A", "B"}, {"B", "A"}}, nil),
		testConfig([][2]string{{"A", "B"}}, map[string]string{"app": "Z"}),
		testConfig([][2]string{{"A", "B"}}, map[string]string{"B": "A"}),
	}
	unknown := testConfig([][2]string{{"A", "B"}}, nil)
	unknown.Links = append(unknown.Links, [2]string{"A", "Q"})
	bad = append(bad, unknown)
	for i, n := range bad {
		if err := n.validate(); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	if err := testConfig([][2]string{{"A", "B"}}, map[string]string{"app": "A"}).validate(); err != nil {
		t.Fatal(err)
	}
}
