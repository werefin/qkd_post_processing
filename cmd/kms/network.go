// Network description every node shares: nodes and their KME URLs, the QKD links between
// them, and which node each application (SAE) belongs to; routes follow from the links
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

type networkConfig struct {
	Nodes map[string]struct {
		URL string `json:"kme_url"`
	} `json:"nodes"`
	Links [][2]string       `json:"links"`
	SAEs  map[string]string `json:"saes"` // SAE ID --> home node ID
}

func loadNetwork(path string) (*networkConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var n networkConfig
	if err := json.Unmarshal(data, &n); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &n, n.validate()
}

func (n *networkConfig) validate() error {
	if len(n.Nodes) == 0 {
		return fmt.Errorf("network: no nodes")
	}
	for id, node := range n.Nodes {
		if id == "" || node.URL == "" {
			return fmt.Errorf("network: node %q needs an ID and a kme_url", id)
		}
	}
	seen := map[[2]string]bool{}
	for _, l := range n.Links {
		a, b := l[0], l[1]
		if _, ok := n.Nodes[a]; !ok {
			return fmt.Errorf("network: link %s-%s: unknown node %s", a, b, a)
		}
		if _, ok := n.Nodes[b]; !ok {
			return fmt.Errorf("network: link %s-%s: unknown node %s", a, b, b)
		}
		if a == b {
			return fmt.Errorf("network: link %s-%s joins a node to itself", a, b)
		}
		if a > b {
			a, b = b, a
		}
		if seen[[2]string{a, b}] {
			return fmt.Errorf("network: link %s-%s listed twice", a, b)
		}
		seen[[2]string{a, b}] = true
	}
	for sae, home := range n.SAEs {
		if _, ok := n.Nodes[home]; !ok {
			return fmt.Errorf("network: SAE %s: unknown home node %s", sae, home)
		}
		if _, clash := n.Nodes[sae]; clash {
			return fmt.Errorf("network: SAE %s has the same ID as a node", sae)
		}
	}
	return nil
}

// neighbors lists the nodes sharing a QKD link with self, sorted
func (n *networkConfig) neighbors(self string) []string {
	var out []string
	for _, l := range n.Links {
		if l[0] == self {
			out = append(out, l[1])
		} else if l[1] == self {
			out = append(out, l[0])
		}
	}
	sort.Strings(out)
	return out
}

// nextHops maps every reachable destination to its first hop on a shortest path, by
// breadth-first search; ties go to the alphabetically first neighbor so every run agrees
func (n *networkConfig) nextHops(self string) map[string]string {
	hops := map[string]string{}
	visited := map[string]bool{self: true}
	type item struct{ node, first string }
	var queue []item
	for _, nb := range n.neighbors(self) {
		visited[nb] = true
		hops[nb] = nb
		queue = append(queue, item{nb, nb})
	}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		for _, nb := range n.neighbors(it.node) {
			if !visited[nb] {
				visited[nb] = true
				hops[nb] = it.first
				queue = append(queue, item{nb, it.first})
			}
		}
	}
	return hops
}

// homeOf is the node an SAE belongs to, "" if the SAE is unknown
func (n *networkConfig) homeOf(sae string) string {
	return n.SAEs[sae]
}
