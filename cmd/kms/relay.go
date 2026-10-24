// KME to KME: a neighbor's key comes straight from the shared QKD pool once the peer confirms the
// reservation; a distant node's key is relayed hop by hop, one-time padded and MACed on every link
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"qkd_post_processing/kms"
	"qkd_post_processing/relay"
)

// reserveItem names the pool keys (from the sender's half) that make up one delivered key
type reserveItem struct {
	KeyID      string   `json:"key_ID"`
	Components []string `json:"components"`
	SizeBytes  int      `json:"size_bytes"`
}

type reserveRequest struct {
	Master string        `json:"master_SAE_ID"`
	Slave  string        `json:"slave_SAE_ID"`
	Keys   []reserveItem `json:"keys"`
}

// relayResult travels back along the path: the key_ID the destination stored
type relayResult struct {
	Status string `json:"status"`
	Dest   string `json:"dest"`
	KeyID  string `json:"key_ID,omitempty"`
}

// deliver makes up to number keys of size bytes for master/slave and has slave's home KME hold
// its copy before any is returned; on any doubt the keys are destroyed, never handed out
func (s *server) deliver(master, slave, home string, number, size int) ([]kms.DeliveredKey, error) {
	hop := s.node.NextHop[home]
	if hop == home {
		return s.deliverDirect(master, slave, home, number, size)
	}
	var keys []kms.DeliveredKey
	for range number {
		id, err := kms.NewKeyID()
		if err != nil {
			return keys, err
		}
		// the key is QKD output from this node's half of the first hop's pool; later hops carry
		// it under their own one-time pads and MACs
		pkt, key, err := s.node.OriginateFromPool(home, id, master, slave, size)
		if err != nil {
			return keys, err
		}
		var res relayResult
		if err := s.kmePost(hop, "/kme/v1/relay", pkt, &res); err != nil || res.KeyID != id {
			clear(key)
			s.audit.log(auditEvent{Event: "relay_failed", KeyID: id, Master: master, Slave: slave, Peer: hop, Detail: fmt.Sprint(err)})
			if err == nil {
				err = fmt.Errorf("%s stored %q, expected %s", home, res.KeyID, id)
			}
			return keys, err
		}
		s.audit.log(auditEvent{Event: "enc_keys", KeyID: id, Master: master, Slave: slave, Peer: home, Bits: 8 * size, Detail: "relayed via " + hop})
		keys = append(keys, kms.DeliveredKey{KeyID: id, Key: key})
	}
	return keys, nil
}

// deliverDirect takes keys from this node's half of the pool shared with home, then asks home to
// reserve the same pool keys for the pair; if home does not confirm, the taken keys are destroyed
func (s *server) deliverDirect(master, slave, home string, number, size int) ([]kms.DeliveredKey, error) {
	pool, mine := s.pools[home], kms.OriginatorOf(s.id, home)
	req := reserveRequest{Master: master, Slave: slave}
	var keys []kms.DeliveredKey
	for range number {
		material, components, err := pool.GetNextBytesMatching(size, mine)
		if err != nil {
			break
		}
		id, err := kms.NewKeyID()
		if err != nil {
			clear(material)
			break
		}
		req.Keys = append(req.Keys, reserveItem{KeyID: id, Components: components, SizeBytes: size})
		keys = append(keys, kms.DeliveredKey{KeyID: id, Key: material})
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no key left in this node's half of the %s-%s pool", s.id, home)
	}
	if err := s.kmePost(home, "/kme/v1/reserve", req, nil); err != nil {
		for _, k := range keys {
			clear(k.Key)
			s.audit.log(auditEvent{Event: "burned", KeyID: k.KeyID, Master: master, Slave: slave, Peer: home, Detail: err.Error()})
		}
		return nil, fmt.Errorf("%s did not confirm the reservation, keys destroyed: %w", home, err)
	}
	for _, k := range keys {
		s.audit.log(auditEvent{Event: "enc_keys", KeyID: k.KeyID, Master: master, Slave: slave, Peer: home, Bits: 8 * size})
	}
	return keys, nil
}

// kmePost sends body to peer over mutual TLS and checks the answer came from that very node
func (s *server) kmePost(peer, path string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return s.kmePostRaw(peer, path, "application/json", data, out)
}

// kmePostRaw is kmePost with a body already encoded as contentType
func (s *server) kmePostRaw(peer, path, contentType string, data []byte, out any) error {
	node, ok := s.net.Nodes[peer]
	if !ok {
		return fmt.Errorf("unknown node %s", peer)
	}
	req, err := http.NewRequest(http.MethodPost, node.URL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	if s.dev {
		req.Header.Set("X-Dev-Role", "kme")
		req.Header.Set("X-Dev-ID", s.id)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", peer, err)
	}
	defer resp.Body.Close()
	if resp.TLS != nil {
		cert := resp.TLS.PeerCertificates[0]
		if cert.Subject.CommonName != peer || len(cert.Subject.OrganizationalUnit) != 1 || cert.Subject.OrganizationalUnit[0] != "kme" {
			return fmt.Errorf("%s answered with the certificate of %q", peer, cert.Subject.CommonName)
		}
	} else if !s.dev {
		return fmt.Errorf("%s: refusing a peer without TLS", peer)
	}
	if resp.StatusCode != http.StatusOK {
		var e etsiError
		json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&e)
		return fmt.Errorf("%s answered %s: %s", peer, resp.Status, e.Message)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// handleReserve is POST /kme/v1/reserve from a neighbor: hold the named pool keys for one SAE pair;
// every check runs before any pool key is touched
func (s *server) handleReserve(w http.ResponseWriter, r *http.Request) {
	c := s.who(r)
	pool := s.pools[c.id]
	if c.role != "kme" || pool == nil {
		writeError(w, http.StatusForbidden, "only a neighbor KME may reserve keys of a shared pool")
		return
	}
	var req reserveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if s.net.homeOf(req.Master) != c.id || s.net.homeOf(req.Slave) != s.id {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("master %s must belong to %s and slave %s to %s", req.Master, c.id, req.Slave, s.id))
		return
	}
	if len(req.Keys) == 0 || len(req.Keys) > maxKeysPerCall {
		writeError(w, http.StatusBadRequest, "between 1 and 128 keys per reservation")
		return
	}
	theirs := kms.OriginatorOf(c.id, s.id)
	seen := map[string]bool{}
	for _, it := range req.Keys {
		if it.SizeBytes <= 0 || it.SizeBytes%kms.ETSI014KeySizeBytes != 0 || len(it.Components)*kms.ETSI014KeySizeBytes != it.SizeBytes {
			writeError(w, http.StatusBadRequest, "key "+it.KeyID+": components do not match size_bytes")
			return
		}
		if err := s.local.CheckReserved(it.KeyID, req.Master, req.Slave); err != kms.ErrKeyNotFound {
			writeError(w, http.StatusConflict, "key_ID "+it.KeyID+" is already in use")
			return
		}
		for _, comp := range it.Components {
			if !theirs(comp) || seen[comp] {
				writeError(w, http.StatusBadRequest, "pool key "+comp+" is not "+c.id+"'s to use, or is listed twice")
				return
			}
			seen[comp] = true
			if err := pool.CheckReserved(comp, "", ""); err != nil {
				writeError(w, http.StatusConflict, "pool key "+comp+": "+err.Error())
				return
			}
		}
	}
	for _, it := range req.Keys {
		material, err := pool.GetBytes(it.Components, it.SizeBytes)
		if err == nil {
			err = s.local.PutReserved(it.KeyID, material, req.Master, req.Slave, s.deliveryTTL)
			clear(material)
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.audit.log(auditEvent{Event: "reserved", KeyID: it.KeyID, Master: req.Master, Slave: req.Slave, Peer: c.id, Bits: 8 * it.SizeBytes})
	}
	writeJSON(w, http.StatusOK, map[string]int{"reserved": len(req.Keys)})
}

// handleRelay is POST /kme/v1/relay: one hop's packet, accepted only from the neighbor it names
// as sender; the destination stores the key for its SAE pair, others pass it on
func (s *server) handleRelay(w http.ResponseWriter, r *http.Request) {
	var pkt relay.RelayedPacket
	if err := json.NewDecoder(r.Body).Decode(&pkt); err != nil {
		writeError(w, http.StatusBadRequest, "invalid packet: "+err.Error())
		return
	}
	c := s.who(r)
	if c.role != "kme" || c.id != pkt.FromHop || s.pools[c.id] == nil {
		writeError(w, http.StatusForbidden, "relay packets are accepted only from the neighbor that sent them")
		return
	}
	if pkt.Dest == s.id {
		if s.net.homeOf(pkt.SlaveSAE) != s.id || s.net.homeOf(pkt.MasterSAE) != pkt.Origin {
			writeError(w, http.StatusBadRequest, "the SAE pair does not match the origin and this destination")
			return
		}
	} else if s.node.NextHop[pkt.Dest] == "" {
		writeError(w, http.StatusBadRequest, "no route to "+pkt.Dest)
		return
	}
	delivered, keyID, next, err := s.node.Handle(pkt)
	if err != nil {
		s.audit.log(auditEvent{Event: "relay_refused", KeyID: pkt.KeyID, Peer: pkt.FromHop, Detail: err.Error()})
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if delivered {
		s.audit.log(auditEvent{Event: "reserved", KeyID: keyID, Master: pkt.MasterSAE, Slave: pkt.SlaveSAE, Peer: pkt.Origin, Bits: 8 * len(pkt.Cipher), Detail: "relayed, last hop " + pkt.FromHop})
		writeJSON(w, http.StatusOK, relayResult{Status: "delivered", Dest: pkt.Dest, KeyID: keyID})
		return
	}
	hop := s.node.NextHop[pkt.Dest]
	s.audit.log(auditEvent{Event: "relay_forwarded", KeyID: pkt.KeyID, Peer: pkt.FromHop + "-->" + hop})
	var res relayResult
	if err := s.kmePost(hop, "/kme/v1/relay", next, &res); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}
