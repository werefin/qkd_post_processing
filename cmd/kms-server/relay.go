// HTTP layer for qkd_post_processing.RelayNode: a node authenticates and decrypts one hop's
// packet, then delivers locally or forwards, passing the destination's answer back
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	qkd "qkd_post_processing"
)

// relayHandler wraps a RelayNode, its neighbors' base URLs, and the (mutual TLS) client
// used to reach them
type relayHandler struct {
	node     *qkd.RelayNode
	peerURLs map[string]string // neighbor node ID -> its kms-server base URL
	client   *http.Client
}

// relayResult is what travels back along the path: the destination's key_ID, so the
// originating application can tell its peer which key to fetch
type relayResult struct {
	Status string `json:"status"`
	Dest   string `json:"dest"`
	KeyID  string `json:"key_ID,omitempty"`
}

// handleRelay is POST /api/v1/relay: one hop's RelayedPacket, from a neighbor
func (h *relayHandler) handleRelay(w http.ResponseWriter, r *http.Request) {
	var pkt qkd.RelayedPacket
	if err := json.NewDecoder(r.Body).Decode(&pkt); err != nil {
		writeError(w, http.StatusBadRequest, "invalid packet: "+err.Error())
		return
	}
	delivered, keyID, next, err := h.node.Handle(pkt)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if delivered {
		log.Printf("relay: delivered key %s from %s", keyID, pkt.FromHop)
		writeJSON(w, http.StatusOK, relayResult{Status: "delivered", Dest: pkt.Dest, KeyID: keyID})
		return
	}
	res, err := h.forward(pkt.Dest, next)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleOriginate is POST /api/v1/relay/originate/{dest_node_id}: the local application asks
// this node to send key_bytes (base64) to dest; the answer carries dest's key_ID
func (h *relayHandler) handleOriginate(w http.ResponseWriter, r *http.Request) {
	dest := r.PathValue("dest_node_id")
	var req struct {
		KeyBytes string `json:"key_bytes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	keyBytes, err := base64.StdEncoding.DecodeString(req.KeyBytes)
	if err != nil || len(keyBytes) == 0 {
		writeError(w, http.StatusBadRequest, "key_bytes must be non-empty base64")
		return
	}
	pkt, err := h.node.Originate(dest, keyBytes)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := h.forward(dest, pkt)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	log.Printf("relay: originated %d bytes toward %s, delivered as %s", len(keyBytes), dest, res.KeyID)
	writeJSON(w, http.StatusOK, res)
}

// forward POSTs pkt to the next hop toward dest and returns that hop's answer
func (h *relayHandler) forward(dest string, pkt qkd.RelayedPacket) (relayResult, error) {
	hop := h.node.NextHop[dest]
	url, ok := h.peerURLs[hop]
	if !ok {
		return relayResult{}, fmt.Errorf("no peer URL configured for %s (next hop toward %s)", hop, dest)
	}
	body, err := json.Marshal(pkt)
	if err != nil {
		return relayResult{}, err
	}
	resp, err := h.client.Post(url+"/api/v1/relay", "application/json", bytes.NewReader(body))
	if err != nil {
		return relayResult{}, fmt.Errorf("forwarding to %s: %w", hop, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e etsiError
		json.NewDecoder(resp.Body).Decode(&e)
		return relayResult{}, fmt.Errorf("%s answered %s: %s", hop, resp.Status, e.Message)
	}
	var res relayResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return relayResult{}, fmt.Errorf("reading %s's answer: %w", hop, err)
	}
	return res, nil
}

// parseAssignments reads "a=x,b=y" into a map; empty input gives an empty map
func parseAssignments(spec string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(spec) == "" {
		return out, nil
	}
	for _, part := range strings.Split(spec, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("expected name=value, got %q", part)
		}
		out[k] = v
	}
	return out, nil
}

// newRelayHandler builds the node from flags: every route's next hop must have both a key
// pool directory (that link's QKD output) and a peer URL
func newRelayHandler(nodeID string, local *qkd.KMS, masterKey []byte, routesSpec, peersSpec, poolsSpec string, client *http.Client) (*relayHandler, error) {
	routes, err := parseAssignments(routesSpec)
	if err != nil {
		return nil, fmt.Errorf("-relay-routes: %w", err)
	}
	peers, err := parseAssignments(peersSpec)
	if err != nil {
		return nil, fmt.Errorf("-relay-peers: %w", err)
	}
	poolDirs, err := parseAssignments(poolsSpec)
	if err != nil {
		return nil, fmt.Errorf("-relay-pools: %w", err)
	}
	pools := map[string]*qkd.KMS{}
	for hop, dir := range poolDirs {
		if pools[hop], err = qkd.NewKMS(dir, masterKey); err != nil {
			return nil, fmt.Errorf("pool for %s: %w", hop, err)
		}
	}
	for dest, hop := range routes {
		if pools[hop] == nil || peers[hop] == "" {
			return nil, fmt.Errorf("route to %s goes via %s, which needs both -relay-pools and -relay-peers entries", dest, hop)
		}
	}
	return &relayHandler{node: qkd.NewRelayNode(nodeID, local, pools, routes), peerURLs: peers, client: client}, nil
}
