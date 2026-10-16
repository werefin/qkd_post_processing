// ETSI GS QKD 014 for this node's applications: the caller is always the SAE named by its client
// certificate, never by anything in the request, and a key goes only to the pair it was made for
package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"qkd_post_processing/kms"
)

const (
	keySizeBits     = kms.ETSI014KeySizeBits // pool key size, also the default and minimum key size
	maxKeySizeBits  = 16 * keySizeBits
	maxKeysPerCall  = 128
	maxSAEIDCount   = 0 // no multicast to additional slave SAEs
	errNotLocalSAE  = "caller is not an SAE of this KME (client certificate role sae, listed under this node in the network)"
	errUnknownSlave = "unknown slave SAE"
)

type etsiKeyContainer struct {
	Keys []etsiKey `json:"keys"`
}

type etsiKey struct {
	KeyID string `json:"key_ID"`
	Key   string `json:"key"` // base64
}

type etsiKeyRequest struct {
	Number            int      `json:"number"`
	Size              int      `json:"size"`
	AdditionalSlaveID []string `json:"additional_slave_SAE_IDs"`
}

type etsiKeyIDsRequest struct {
	KeyIDs []struct {
		KeyID string `json:"key_ID"`
	} `json:"key_IDs"`
}

// slaveRoute resolves where keys for slave go: its home KME, which must be another node
func (s *server) slaveRoute(slave string) (home string, err error) {
	home = s.net.homeOf(slave)
	switch {
	case home == "":
		return "", errors.New(errUnknownSlave + " " + slave)
	case home == s.id:
		return "", fmt.Errorf("slave SAE %s is on this KME: a key pair needs SAEs on two different nodes", slave)
	case s.node.NextHop[home] == "":
		return "", fmt.Errorf("no route from %s to %s, the home of %s", s.id, home, slave)
	}
	return home, nil
}

// handleStatus is ETSI 014 Get Status: GET /api/v1/keys/{slave_SAE_ID}/status
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	master := s.localSAE(r)
	if master == "" {
		writeError(w, http.StatusUnauthorized, errNotLocalSAE)
		return
	}
	slave := r.PathValue("slave_sae_id")
	home, err := s.slaveRoute(slave)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"source_KME_ID":       s.id,
		"target_KME_ID":       home,
		"master_SAE_ID":       master,
		"slave_SAE_ID":        slave,
		"key_size":            keySizeBits,
		"stored_key_count":    s.availableToward(home),
		"max_key_count":       s.availableToward(home),
		"max_key_per_request": maxKeysPerCall,
		"max_key_size":        maxKeySizeBits,
		"min_key_size":        keySizeBits,
		"max_SAE_ID_count":    maxSAEIDCount,
	})
}

// availableToward counts default-size keys this node can deliver toward home right now: its half
// of the link pool for a neighbor, or half its half of the first hop's pool (pad plus MAC per key)
func (s *server) availableToward(home string) int {
	hop := s.node.NextHop[home]
	pool := s.pools[hop]
	if pool == nil {
		return 0
	}
	n := pool.CountAvailable(kms.OriginatorOf(s.id, hop))
	if hop != home {
		n /= 2
	}
	return n
}

// handleEncKeys is ETSI 014 Get Key: GET or POST /api/v1/keys/{slave_SAE_ID}/enc_keys
func (s *server) handleEncKeys(w http.ResponseWriter, r *http.Request) {
	master := s.localSAE(r)
	if master == "" {
		writeError(w, http.StatusUnauthorized, errNotLocalSAE)
		return
	}
	slave := r.PathValue("slave_sae_id")
	home, err := s.slaveRoute(slave)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req := etsiKeyRequest{Number: 1, Size: keySizeBits}
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
		if req.Number == 0 {
			req.Number = 1
		}
		if req.Size == 0 {
			req.Size = keySizeBits
		}
	} else {
		for name, dst := range map[string]*int{"number": &req.Number, "size": &req.Size} {
			if v := r.URL.Query().Get(name); v != "" {
				if *dst, err = strconv.Atoi(v); err != nil {
					writeError(w, http.StatusBadRequest, name+" must be an integer")
					return
				}
			}
		}
	}
	switch {
	case len(req.AdditionalSlaveID) > maxSAEIDCount:
		writeError(w, http.StatusBadRequest, "additional slave SAEs are not supported (max_SAE_ID_count 0)")
		return
	case req.Number < 1 || req.Number > maxKeysPerCall:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("number must be between 1 and %d", maxKeysPerCall))
		return
	case req.Size < keySizeBits || req.Size > maxKeySizeBits || req.Size%keySizeBits != 0:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("size must be a multiple of %d bits, at most %d", keySizeBits, maxKeySizeBits))
		return
	}

	keys, err := s.deliver(master, slave, home, req.Number, req.Size/8)
	if len(keys) == 0 {
		if err == nil {
			err = fmt.Errorf("no key available toward %s", slave)
		}
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	out := etsiKeyContainer{Keys: make([]etsiKey, len(keys))}
	for i, k := range keys {
		out.Keys[i] = etsiKey{KeyID: k.KeyID, Key: base64.StdEncoding.EncodeToString(k.Key)}
		clear(k.Key)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDecKeys is ETSI 014 Get Key with Key IDs: GET (?key_ID=) or POST
// /api/v1/keys/{master_SAE_ID}/dec_keys; every ID is checked before any key is delivered
func (s *server) handleDecKeys(w http.ResponseWriter, r *http.Request) {
	slave := s.localSAE(r)
	if slave == "" {
		writeError(w, http.StatusUnauthorized, errNotLocalSAE)
		return
	}
	master := r.PathValue("master_sae_id")
	var ids []string
	if r.Method == http.MethodPost {
		var req etsiKeyIDsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
		for _, k := range req.KeyIDs {
			ids = append(ids, k.KeyID)
		}
	} else if id := r.URL.Query().Get("key_ID"); id != "" {
		ids = []string{id}
	}
	if len(ids) == 0 || len(ids) > maxKeysPerCall {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("give between 1 and %d key_IDs", maxKeysPerCall))
		return
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			writeError(w, http.StatusBadRequest, "key_ID "+id+" listed twice")
			return
		}
		seen[id] = true
		if err := s.local.CheckReserved(id, master, slave); err != nil {
			status, msg := decKeyError(id, err)
			s.audit.log(auditEvent{Event: "dec_keys_refused", KeyID: id, Master: master, Slave: slave, Detail: err.Error()})
			writeError(w, status, msg)
			return
		}
	}
	out := etsiKeyContainer{Keys: make([]etsiKey, 0, len(ids))}
	for _, id := range ids {
		key, err := s.local.GetReserved(id, master, slave)
		if err != nil {
			status, msg := decKeyError(id, err)
			writeError(w, status, msg)
			return
		}
		out.Keys = append(out.Keys, etsiKey{KeyID: id, Key: base64.StdEncoding.EncodeToString(key)})
		s.audit.log(auditEvent{Event: "dec_keys", KeyID: id, Master: master, Slave: slave, Bits: 8 * len(key)})
		clear(key)
	}
	writeJSON(w, http.StatusOK, out)
}

// decKeyError never tells a caller whether a key exists for some other pair
func decKeyError(id string, err error) (int, string) {
	switch {
	case errors.Is(err, kms.ErrKeyConsumed), errors.Is(err, kms.ErrKeyExpired):
		return http.StatusGone, "key_ID " + id + " was already delivered or has expired"
	case errors.Is(err, kms.ErrKeyNotFound), errors.Is(err, kms.ErrKeyReserved):
		return http.StatusNotFound, "key_ID " + id + " not found for this SAE pair"
	}
	return http.StatusInternalServerError, err.Error()
}
