// Two-party roles: -role alice and -role bob run each site's half of the protocol on its own
// recording only, exchanging public messages through the nodes' KMS mailbox (qkd/twoparty.go)
package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"qkd_post_processing/kms"
	"qkd_post_processing/qkd"
)

const (
	publicTSSuffix    = "_public_timestamp_sequence.npy"
	publicFlagsSuffix = "_public_flags.npy"
)

// twoPartyOptions is what main hands the two-party roles
type twoPartyOptions struct {
	role, peer, session, exportDir, keystore string
	dirs                                     []string
	jsonOut                                  bool
	mpWindowPS, windowPS                     int64
	filter                                   bool
	driftCfg                                 qkd.DriftConfig
	winCfg                                   qkd.WindowConfig
	delays                                   *qkd.DetectorDelays
	expectedQBER                             float64
	timeout                                  time.Duration
	cfg                                      qkd.TwoPartyConfig
	dial                                     func() (qkd.Conn, error) // nil = this node's KMS mailbox
}

func (o twoPartyOptions) connect() (qkd.Conn, error) {
	if o.dial != nil {
		return o.dial()
	}
	return newMailboxConn(o.peer, o.session, o.timeout)
}

type twoPartyJSON struct {
	Role               string         `json:"role"`
	Peer               string         `json:"peer"`
	Session            string         `json:"session"`
	Method             string         `json:"method"`
	Recordings         int            `json:"recordings"`
	OwnClicks          int            `json:"own_clicks"`
	SiftedBits         int            `json:"sifted_bits"`
	QBER               float64        `json:"qber"`
	FilterWindowPS     int64          `json:"filter_window_ps,omitempty"`
	FilterSigmaPS      float64        `json:"filter_sigma_ps,omitempty"`
	FilterAccidentals  float64        `json:"filter_predicted_accidental_fraction,omitempty"`
	PolarBlocks        int            `json:"polar_blocks"`
	PolarAttempts      int            `json:"polar_attempts"`
	PolarBitsCorrected int            `json:"polar_bits_corrected"`
	PolarLeakedBits    int            `json:"polar_leaked_bits"`
	LDPC5GRows         int            `json:"ldpc5g_rows,omitempty"`
	LDPC5GFrames       int            `json:"ldpc5g_frames,omitempty"`
	LDPC5GFailed       int            `json:"ldpc5g_failed,omitempty"`
	LDPC5GAttempts     int            `json:"ldpc5g_attempts,omitempty"`
	LDPC5GCorrected    int            `json:"ldpc5g_bits_corrected,omitempty"`
	LDPC5GLeakedBits   int            `json:"ldpc5g_leaked_bits,omitempty"`
	ChunksTotal        int            `json:"chunks_total"`
	ChunksKept         int            `json:"chunks_kept"`
	LeakedBitsTotal    int            `json:"leaked_bits_total"`
	FiniteKey          *finiteKeyJSON `json:"finite_key,omitempty"`
	FinalKeyBits       int            `json:"final_key_bits"`
	CompressionRate    float64        `json:"compression_rate"`
	KeysSaved          int            `json:"keys_saved"`
	ElapsedMS          int64          `json:"elapsed_ms"`
	PublicFiles        []string       `json:"public_files,omitempty"`
	AnnouncementDigest string         `json:"announcement_digest,omitempty"`
	AnnouncementClicks int            `json:"announcement_clicks,omitempty"`
}

func runTwoParty(o twoPartyOptions) {
	t0 := time.Now()
	var out twoPartyJSON
	var err error
	switch o.role {
	case "alice":
		out, err = runAliceRole(o)
	case "bob":
		out, err = runBobRole(o)
	default:
		err = fmt.Errorf("-role must be alice or bob, got %q", o.role)
	}
	if err != nil {
		fail(o.jsonOut, "%v", err)
	}
	out.ElapsedMS = time.Since(t0).Milliseconds()
	if o.jsonOut {
		json.NewEncoder(os.Stdout).Encode(out)
		return
	}
	section(fmt.Sprintf("two-party %s with %s, session %s", out.Role, out.Peer, out.Session))
	if len(out.PublicFiles) > 0 {
		field("Announcement", "%d clicks, digest %s", out.AnnouncementClicks, out.AnnouncementDigest)
		for _, f := range out.PublicFiles {
			field("Wrote", "%s", f)
		}
		return
	}
	field("Sifted bits", "%d", out.SiftedBits)
	field("QBER", "%.4f", out.QBER)
	if out.Method == "ldpc5g" {
		field("5G NR LDPC", "%d frames, %d attempts, %d failed, %d bits disclosed", out.LDPC5GFrames, out.LDPC5GAttempts, out.LDPC5GFailed, out.LDPC5GLeakedBits)
	} else {
		field("Polar", "%d blocks, %d attempts, %d bits disclosed", out.PolarBlocks, out.PolarAttempts, out.PolarLeakedBits)
	}
	field("CRC chunks kept", "%d / %d", out.ChunksKept, out.ChunksTotal)
	field("Disclosed in total", "%d bits", out.LeakedBitsTotal)
	field("Final key", "%d bits, %d records saved", out.FinalKeyBits, out.KeysSaved)
}

// aliceEvents builds Alice's own events from each directory's single recording, in directory
// order; their order is the index space the whole protocol refers to
func aliceEvents(o twoPartyOptions) (events []qkd.DetectionEvent, perRec [][]qkd.DetectionEvent, bases []string, err error) {
	for _, d := range o.dirs {
		ts, base, err := ownRecording(d)
		if err != nil {
			return nil, nil, nil, err
		}
		t, err := loadNpyInt64(ts)
		if err != nil {
			return nil, nil, nil, err
		}
		ch, err := loadNpyInt8(channelPath(ts))
		if err != nil {
			return nil, nil, nil, err
		}
		if len(t) != len(ch) || len(t) == 0 {
			return nil, nil, nil, fmt.Errorf("%s: empty recording or timestamp and channel counts differ", d)
		}
		ev, _, _ := buildEvents(t, ch, detectMultiPhoton(t, ch, o.mpWindowPS), func(ts int64) int64 { return ts })
		if o.delays != nil {
			ev = qkd.ApplyDetectorDelays(ev, o.delays.Alice)
		}
		perRec = append(perRec, ev)
		events = append(events, ev...)
		bases = append(bases, base)
	}
	return events, perRec, bases, nil
}

// ownRecording finds the one recording of this site in dir (announcements are not counted)
func ownRecording(dir string) (path, base string, err error) {
	all, _ := filepath.Glob(filepath.Join(dir, "*_timestamp_sequence.npy"))
	var own []string
	for _, p := range all {
		if !strings.HasSuffix(p, publicTSSuffix) {
			own = append(own, p)
		}
	}
	if len(own) != 1 {
		return "", "", fmt.Errorf("%s: expected exactly one own *_timestamp_sequence.npy, found %d", dir, len(own))
	}
	return own[0], strings.TrimSuffix(filepath.Base(own[0]), "_timestamp_sequence.npy"), nil
}

func runAliceRole(o twoPartyOptions) (twoPartyJSON, error) {
	out := twoPartyJSON{Role: "alice", Peer: o.peer, Session: o.session, Method: o.cfg.Method, Recordings: len(o.dirs)}
	events, perRec, bases, err := aliceEvents(o)
	if err != nil {
		return out, err
	}
	out.OwnClicks = len(events)
	digest := qkd.PublicDigest(qkd.StripBit(events))
	out.AnnouncementDigest, out.AnnouncementClicks = fmt.Sprintf("%x", digest[:8]), len(events)

	if o.exportDir != "" {
		// the announcement: timing, basis and multi-photon flag per click, never the bit
		if err := os.MkdirAll(o.exportDir, 0o755); err != nil {
			return out, err
		}
		for i, ev := range perRec {
			ts := make([]int64, len(ev))
			flags := make([]int8, len(ev))
			for j, e := range ev {
				ts[j] = e.TimestampPS
				flags[j] = int8(e.Basis)
				if e.MultiPhoton {
					flags[j] |= 2
				}
			}
			tsPath := filepath.Join(o.exportDir, bases[i]+publicTSSuffix)
			flagPath := filepath.Join(o.exportDir, bases[i]+publicFlagsSuffix)
			if err := writeNpyInt64(tsPath, ts); err != nil {
				return out, err
			}
			if err := writeNpyInt8(flagPath, flags); err != nil {
				return out, err
			}
			out.PublicFiles = append(out.PublicFiles, tsPath, flagPath)
		}
		return out, nil
	}

	conn, err := o.connect()
	if err != nil {
		return out, err
	}
	res, err := qkd.RunAlice(conn, events, o.cfg)
	if err != nil {
		return out, err
	}
	return finishRoleOutput(out, o, res)
}

// bobRecording is one directory on Bob's side: Alice's announcement and Bob's own recording,
// Bob's clicks moved onto Alice's clock with the coincidence report, pruned and filtered
type bobRecording struct {
	alice  []qkd.DetectionEvent // announced events, bits unknown (always 0)
	bob    []qkd.DetectionEvent
	filter *qkd.CoincidenceFilter
}

func loadBobRecording(dir string, o twoPartyOptions) (bobRecording, error) {
	var r bobRecording
	tsA, tsB, report, err := findRecording(dir)
	if err != nil {
		return r, err
	}
	if !strings.HasSuffix(tsA, publicTSSuffix) {
		tsA, tsB = tsB, tsA
	}
	if !strings.HasSuffix(tsA, publicTSSuffix) || strings.HasSuffix(tsB, publicTSSuffix) {
		return r, fmt.Errorf("%s: needs Alice's announcement (*%s) and one own recording", dir, publicTSSuffix)
	}
	if filepath.Base(report.InputA) != filepath.Base(tsA) {
		return r, fmt.Errorf("%s: the coincidence report must have Alice's announcement as input A", dir)
	}
	at, err := loadNpyInt64(tsA)
	if err != nil {
		return r, err
	}
	flags, err := loadNpyInt8(strings.TrimSuffix(tsA, publicTSSuffix) + publicFlagsSuffix)
	if err != nil {
		return r, err
	}
	bt, err := loadNpyInt64(tsB)
	if err != nil {
		return r, err
	}
	bch, err := loadNpyInt8(channelPath(tsB))
	if err != nil {
		return r, err
	}
	if len(at) != len(flags) || len(at) == 0 || len(bt) != len(bch) || len(bt) == 0 {
		return r, fmt.Errorf("%s: empty files or mismatched lengths", dir)
	}
	r.alice = make([]qkd.DetectionEvent, len(at))
	for i := range at {
		if flags[i]&^3 != 0 {
			return r, fmt.Errorf("%s: invalid announcement flag %d", dir, flags[i])
		}
		r.alice[i] = qkd.DetectionEvent{TimestampPS: at[i], Basis: qkd.Basis(flags[i] & 1), MultiPhoton: flags[i]&2 != 0}
	}
	a0, b0 := at[0], bt[0]
	skew := report.ClockSkew
	toAliceClock := func(ts int64) int64 {
		bRel := float64(ts - b0)
		aRel := skew.ReferenceElapsedPS + (bRel-(skew.ReferenceElapsedPS+skew.LagAtReferencePS))/(1+skew.SkewFraction)
		return a0 + int64(math.Round(aRel))
	}
	r.bob, _, _ = buildEvents(bt, bch, detectMultiPhoton(bt, bch, o.mpWindowPS), toAliceClock)
	if o.delays != nil {
		r.bob = qkd.ApplyDetectorDelays(r.bob, o.delays.Bob)
	}
	r.bob = qkd.PruneToCoincidenceRange(r.alice, r.bob, pruneRadiusPS(o.windowPS, report))
	if o.filter {
		corrected, fr, ferr := qkd.FilterCoincidences(r.alice, r.bob, o.driftCfg, o.winCfg)
		if ferr != nil {
			return r, fmt.Errorf("%s: coincidence filter: %v", dir, ferr)
		}
		r.bob, r.filter = corrected, &fr
	}
	return r, nil
}

func runBobRole(o twoPartyOptions) (twoPartyJSON, error) {
	out := twoPartyJSON{Role: "bob", Peer: o.peer, Session: o.session, Method: o.cfg.Method, Recordings: len(o.dirs)}
	var recs []loadedRecording
	var announced []qkd.DetectionEvent
	for _, d := range o.dirs {
		r, err := loadBobRecording(d, o)
		if err != nil {
			return out, err
		}
		announced = append(announced, r.alice...)
		recs = append(recs, loadedRecording{alice: r.alice, bob: r.bob, filter: r.filter})
		out.OwnClicks += len(r.bob)
	}
	digest := qkd.PublicDigest(qkd.StripBit(announced))
	out.AnnouncementDigest, out.AnnouncementClicks = fmt.Sprintf("%x", digest[:8]), len(announced)
	alice, bob := poolRecordings(recs)

	var filt *qkd.CoincidenceFilter
	if o.filter {
		if len(recs) == 1 {
			filt = recs[0].filter
		} else {
			corrected, fr, err := qkd.FilterCoincidences(alice, bob, o.driftCfg, o.winCfg)
			if err != nil {
				return out, fmt.Errorf("coincidence filter on the pooled recordings: %v", err)
			}
			bob, filt = corrected, &fr
		}
	}
	cfg := o.cfg
	cfg.AnnouncementDigest = digest
	cfg.WindowPS = o.windowPS
	if cfg.WindowPS == 0 && filt != nil {
		cfg.WindowPS = filt.WindowPS
	}
	if cfg.WindowPS == 0 {
		return out, errors.New("no coincidence window: enable the filter or give -window")
	}
	if !cfg.Asymptotic {
		cfg.ExpectedQBER = o.expectedQBER
		if cfg.ExpectedQBER <= 0 && filt != nil {
			cfg.ExpectedQBER = filt.PredictedQBER
		}
		if cfg.ExpectedQBER <= 0 {
			cfg.ExpectedQBER = 0.05
		}
	}
	if filt != nil {
		out.FilterWindowPS, out.FilterSigmaPS, out.FilterAccidentals = filt.WindowPS, filt.Fit.SigmaPS, filt.AccidentalFraction
	}

	conn, err := o.connect()
	if err != nil {
		return out, err
	}
	res, err := qkd.RunBob(conn, qkd.StripBit(alice), bob, cfg)
	if err != nil {
		return out, err
	}
	return finishRoleOutput(out, o, res)
}

func finishRoleOutput(out twoPartyJSON, o twoPartyOptions, res qkd.TwoPartyResult) (twoPartyJSON, error) {
	out.SiftedBits, out.QBER, out.Method = res.SiftedBits, res.QBER.QBER, res.Method
	out.LDPC5GRows, out.LDPC5GFrames, out.LDPC5GFailed = res.LDPC.M, res.LDPC.Frames, res.LDPC.Failed
	out.LDPC5GAttempts, out.LDPC5GCorrected, out.LDPC5GLeakedBits = res.LDPC.Attempts, res.LDPC.BitsCorrected, res.LDPC.LeakedBits
	out.PolarBlocks, out.PolarAttempts, out.PolarBitsCorrected, out.PolarLeakedBits = res.PolarBlocks, res.PolarAttempts, res.PolarBitsCorrected, res.PolarLeakedBits
	out.ChunksTotal, out.ChunksKept, out.LeakedBitsTotal = res.CRC.ChunksTotal, res.CRC.ChunksKept, res.LeakedBits
	out.FiniteKey = toFiniteKeyJSON(res.FiniteKey)
	out.FinalKeyBits = len(res.FinalKeyBits)
	if n := len(res.CRC.Surviving); n > 0 {
		out.CompressionRate = float64(out.FinalKeyBits) / float64(n)
	}
	if o.keystore != "" && len(res.FinalKeyBytes) > 0 {
		stored, err := kms.SaveKeys256WithIDs(o.keystore, res.FinalKeyBytes, res.KeyIDs)
		out.KeysSaved = len(stored)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// mailboxConn is qkd.Conn over this node's KMS: messages go to the neighbor's dashboard
// through the two KMEs' mutual TLS, so every message is authenticated hop by hop
type mailboxConn struct {
	base, peer, session string
	client              *http.Client
	timeout             time.Duration
}

// newMailboxConn reads KMS_URL, KMS_CA, KMS_ADMIN_CERT and KMS_ADMIN_KEY, as the dashboard sets them
func newMailboxConn(peer, session string, timeout time.Duration) (*mailboxConn, error) {
	base, caFile := os.Getenv("KMS_URL"), os.Getenv("KMS_CA")
	cert, key := os.Getenv("KMS_ADMIN_CERT"), os.Getenv("KMS_ADMIN_KEY")
	if base == "" || caFile == "" || cert == "" || key == "" || peer == "" || session == "" {
		return nil, errors.New("two-party needs -peer, -session and KMS_URL, KMS_CA, KMS_ADMIN_CERT, KMS_ADMIN_KEY")
	}
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return nil, err
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s: no certificates", caFile)
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{pair}, RootCAs: pool, MinVersion: tls.VersionTLS13}}
	return &mailboxConn{base: strings.TrimRight(base, "/"), peer: peer, session: session,
		client: &http.Client{Transport: tr, Timeout: 6 * time.Minute}, timeout: timeout}, nil
}

func (c *mailboxConn) Send(kind string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	msg, _ := json.Marshal(map[string]any{"peer": c.peer, "session": c.session, "kind": kind, "body": json.RawMessage(body)})
	resp, err := c.client.Post(c.base+"/kme/v1/pp/send", "application/json", bytes.NewReader(msg))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("sending %s: %s %s", kind, resp.Status, bytes.TrimSpace(b))
	}
	return nil
}

func (c *mailboxConn) Recv(kind string, v any) error {
	deadline := time.Now().Add(c.timeout)
	q := url.Values{"peer": {c.peer}, "session": {c.session}, "wait": {"30"}}
	for time.Now().Before(deadline) {
		resp, err := c.client.Get(c.base + "/kme/v1/pp/recv?" + q.Encode())
		if err != nil {
			return err
		}
		data, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		switch {
		case rerr != nil:
			return rerr
		case resp.StatusCode == http.StatusNoContent:
			continue
		case resp.StatusCode != http.StatusOK:
			return fmt.Errorf("receiving %s: %s %s", kind, resp.Status, bytes.TrimSpace(data))
		}
		var m struct {
			Kind string          `json:"kind"`
			Body json.RawMessage `json:"body"`
		}
		if err := json.Unmarshal(data, &m); err != nil {
			return err
		}
		return qkd.DecodeMessage(m.Kind, m.Body, kind, v)
	}
	return fmt.Errorf("no %s message from %s within %s", kind, c.peer, c.timeout)
}

// writeNpyInt64 and writeNpyInt8 write flat little-endian arrays in numpy's version 1.0 format
func writeNpyInt64(path string, v []int64) error {
	data := make([]byte, 8*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint64(data[8*i:], uint64(x))
	}
	return writeNpy(path, "<i8", len(v), data)
}

func writeNpyInt8(path string, v []int8) error {
	data := make([]byte, len(v))
	for i, x := range v {
		data[i] = byte(x)
	}
	return writeNpy(path, "|i1", len(v), data)
}

func writeNpy(path, descr string, n int, data []byte) error {
	header := fmt.Sprintf("{'descr': '%s', 'fortran_order': False, 'shape': (%d,), }", descr, n)
	// header padded with spaces to a multiple of 64 bytes including the 10-byte preamble, newline last
	pad := 64 - (10+len(header)+1)%64
	header += strings.Repeat(" ", pad%64) + "\n"
	var b bytes.Buffer
	b.WriteString("\x93NUMPY\x01\x00")
	binary.Write(&b, binary.LittleEndian, uint16(len(header)))
	b.WriteString(header)
	b.Write(data)
	return os.WriteFile(path, b.Bytes(), 0o644)
}
