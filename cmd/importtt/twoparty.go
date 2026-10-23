// Two-party roles: -role alice and -role bob run each site's half of the protocol on its own
// recording only, exchanging public messages through the nodes' KMS mailbox (qkd/twoparty.go)
package main

import (
	"bytes"
	"crypto/sha256"
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
	// Alice's squashed channel per click beside her recording, so her announcement and her bits share the random choices
	squashedSuffix = "_squashed.npy"
	// a block streamed in segments: Alice keeps her bits and flags only, Bob each kept click's index
	aliceBitsSuffix   = "_alice_bits.npy"
	aliceFlagsSuffix  = "_alice_flags.npy"
	publicIndexSuffix = "_public_index.npy"
)

// twoPartyOptions is what main hands the two-party roles
type twoPartyOptions struct {
	role, peer, session, exportDir, keystore string
	dirs                                     []string
	streamDigest                             []byte // a block streamed in segments: the whole announcement's digest
	streamClicks                             int    // and how many clicks it holds
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
	CascadeParities    int            `json:"cascade_parities,omitempty"`
	CascadeRounds      int            `json:"cascade_rounds,omitempty"`
	CascadeCorrected   int            `json:"cascade_corrections,omitempty"`
	ECLeakedBits       int            `json:"ec_leaked_bits"`
	BasisBits          [2]int         `json:"basis_bits"`            // key bits per basis, HV then DA
	BasisChecked       [2]int         `json:"basis_checked"`         // estimation bits per basis
	BasisErrors        [2]int         `json:"basis_errors"`          // errors among them
	PairCounts         map[string]int `json:"pair_counts,omitempty"` // Bob only: verified pairs by Alice's then Bob's polarization
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
	field("Cascade", "%d parities in %d round trips, %d corrections", out.CascadeParities, out.CascadeRounds, out.CascadeCorrected)
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
		sq, err := ownSquashed(ts, t, ch, o.mpWindowPS)
		if err != nil {
			return nil, nil, nil, err
		}
		ev, _, _ := buildEvents(t, ch, sq, nil, func(ts int64) int64 { return ts }, o.aliceDelays())
		perRec = append(perRec, ev)
		events = append(events, ev...)
		bases = append(bases, base)
	}
	return events, perRec, bases, nil
}

// streamedClicks is Alice's view of a streamed block: one packed bit per announced click and its 2-bit flag
// (basis, multi-photon), four to a byte, in announcement order
type streamedClicks struct {
	n           int
	bits, flags []int8
}

func (c streamedClicks) Len() int { return c.n }
func (c streamedClicks) Click(i int) (byte, qkd.Basis, bool) {
	bit := byte(c.bits[i/8]) >> (7 - i%8) & 1
	f := byte(c.flags[i/4]) >> (2 * (i % 4)) & 3
	return bit, qkd.Basis(f & 1), f&2 != 0
}

// runAliceStreamed runs Alice's half of a block streamed in segments: her clicks were announced and dropped as
// each segment closed, and only her bits and flags are left
func runAliceStreamed(o twoPartyOptions, out twoPartyJSON, bitsPath string) (twoPartyJSON, error) {
	if len(o.streamDigest) == 0 || o.streamClicks <= 0 {
		return out, errors.New("a streamed block needs -announcement-digest and -announcement-clicks")
	}
	bits, err := loadNpyInt8(bitsPath)
	if err != nil {
		return out, err
	}
	flags, err := loadNpyInt8(strings.TrimSuffix(bitsPath, aliceBitsSuffix) + aliceFlagsSuffix)
	if err != nil {
		return out, err
	}
	n := o.streamClicks
	if len(bits) != (n+7)/8 || len(flags) != (n+3)/4 {
		return out, fmt.Errorf("%d clicks need %d bytes of bits and %d of flags, found %d and %d", n, (n+7)/8, (n+3)/4, len(bits), len(flags))
	}
	out.OwnClicks = n
	out.AnnouncementDigest, out.AnnouncementClicks = fmt.Sprintf("%x", o.streamDigest[:8]), n
	conn, err := o.connect()
	if err != nil {
		return out, err
	}
	res, err := qkd.RunAliceClicks(conn, streamedClicks{n, bits, flags}, o.streamDigest, o.cfg)
	if err != nil {
		return out, err
	}
	return finishRoleOutput(out, o, res)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// ownSquashed is Alice's squashed channel per click: from beside her recording when an earlier run made it, else
// squashed now and kept there; no more secret than the channel file next to it
func ownSquashed(tsPath string, t []int64, ch []int8, windowPS int64) ([]int8, error) {
	side := strings.TrimSuffix(tsPath, "_timestamp_sequence.npy") + squashedSuffix
	if fileExists(side) {
		sq, err := loadNpyInt8(side)
		if err != nil {
			return nil, err
		}
		if len(sq) != len(t) {
			return nil, fmt.Errorf("%s: %d squashed channels for %d clicks", side, len(sq), len(t))
		}
		return sq, nil
	}
	sq, _, err := squashClicks(t, ch, windowPS, squashRandom())
	if err != nil {
		return nil, err
	}
	// written beside and renamed, so a run that stops halfway leaves no partial choices behind
	if err := writeNpyInt8(side+".tmp", sq); err != nil {
		return nil, err
	}
	return sq, os.Rename(side+".tmp", side)
}

func (o twoPartyOptions) aliceDelays() *[2][2]float64 {
	if o.delays == nil {
		return nil
	}
	return &o.delays.Alice
}

// exportPublic writes Alice's announcement straight from her squashed clicks: timing (detector delays applied) and
// basis per detection, never the bit; no event list is built, so a segment costs little memory
func exportPublic(o twoPartyOptions, out twoPartyJSON) (twoPartyJSON, error) {
	if err := os.MkdirAll(o.exportDir, 0o755); err != nil {
		return out, err
	}
	h := sha256.New()
	var rec [10]byte
	for _, d := range o.dirs {
		tsFile, base, err := ownRecording(d)
		if err != nil {
			return out, err
		}
		t, err := loadNpyInt64(tsFile)
		if err != nil {
			return out, err
		}
		ch, err := loadNpyInt8(channelPath(tsFile))
		if err != nil {
			return out, err
		}
		if len(t) != len(ch) || len(t) == 0 {
			return out, fmt.Errorf("%s: empty recording or timestamp and channel counts differ", d)
		}
		sq, err := ownSquashed(tsFile, t, ch, o.mpWindowPS)
		if err != nil {
			return out, err
		}
		ts := make([]int64, 0, len(t))
		flags := make([]int8, 0, len(t))
		for i, c := range sq {
			basis, _, ok := channelToBasisBit(c)
			if !ok {
				continue
			}
			at := t[i] - delayOf(o.aliceDelays(), ch[i])
			f := int8(basis)
			ts, flags = append(ts, at), append(flags, f)
			// the digest Bob checks, as qkd.PublicDigest computes it over the same events
			binary.LittleEndian.PutUint64(rec[:8], uint64(at))
			rec[8], rec[9] = byte(basis), byte(f>>1)
			h.Write(rec[:])
		}
		tsPath, flagPath := filepath.Join(o.exportDir, base+publicTSSuffix), filepath.Join(o.exportDir, base+publicFlagsSuffix)
		if err := writeNpyInt64(tsPath, ts); err != nil {
			return out, err
		}
		if err := writeNpyInt8(flagPath, flags); err != nil {
			return out, err
		}
		out.OwnClicks += len(ts)
		out.PublicFiles = append(out.PublicFiles, tsPath, flagPath)
	}
	digest := h.Sum(nil)
	out.AnnouncementDigest, out.AnnouncementClicks = fmt.Sprintf("%x", digest[:8]), out.OwnClicks
	return out, nil
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
	out := twoPartyJSON{Role: "alice", Peer: o.peer, Session: o.session, Method: "cascade", Recordings: len(o.dirs)}
	if o.exportDir != "" {
		return exportPublic(o, out)
	}
	if bits, _ := filepath.Glob(filepath.Join(o.dirs[0], "*"+aliceBitsSuffix)); len(bits) == 1 && len(o.dirs) == 1 {
		return runAliceStreamed(o, out, bits[0])
	}
	events, _, _, err := aliceEvents(o)
	if err != nil {
		return out, err
	}
	out.OwnClicks = len(events)
	digest := qkd.PublicDigest(qkd.StripBit(events))
	out.AnnouncementDigest, out.AnnouncementClicks = fmt.Sprintf("%x", digest[:8]), len(events)

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
	index  []int64 // a streamed block: each announced click's index in the whole announcement
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
	if idx := strings.TrimSuffix(tsA, publicTSSuffix) + publicIndexSuffix; fileExists(idx) {
		if r.index, err = loadNpyInt64(idx); err != nil {
			return r, err
		}
		if len(r.index) != len(at) {
			return r, fmt.Errorf("%s: %d indices for %d announced clicks", dir, len(r.index), len(at))
		}
	}
	a0, b0 := at[0], bt[0]
	skew := report.ClockSkew
	toAliceClock := func(ts int64) int64 {
		bRel := float64(ts - b0)
		aRel := skew.ReferenceElapsedPS + (bRel-(skew.ReferenceElapsedPS+skew.LagAtReferencePS))/(1+skew.SkewFraction)
		return a0 + int64(math.Round(aRel))
	}
	sq, _, err := squashClicks(bt, bch, o.mpWindowPS, squashRandom())
	if err != nil {
		return r, err
	}
	var delays *[2][2]float64
	if o.delays != nil {
		delays = &o.delays.Bob
	}
	r.bob, _, _ = buildEvents(bt, bch, sq, nil, toAliceClock, delays)
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
	out := twoPartyJSON{Role: "bob", Peer: o.peer, Session: o.session, Method: "cascade", Recordings: len(o.dirs)}
	var recs []loadedRecording
	var announced []qkd.DetectionEvent
	for _, d := range o.dirs {
		r, err := loadBobRecording(d, o)
		if err != nil {
			return out, err
		}
		announced = append(announced, r.alice...)
		recs = append(recs, loadedRecording{alice: r.alice, bob: r.bob, filter: r.filter, index: r.index})
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
	if streamed := recs[0].index; streamed != nil {
		// Bob holds only the announced clicks that can pair; the pair list still names the whole announcement's
		if len(o.dirs) != 1 || len(o.streamDigest) == 0 || o.streamClicks <= 0 {
			return out, errors.New("a streamed block is one directory, with -announcement-digest and -announcement-clicks")
		}
		cfg.AnnouncementDigest, cfg.AnnouncementEvents = o.streamDigest, o.streamClicks
		cfg.AnnouncementIndex = make([]int32, len(streamed))
		for i, v := range streamed {
			if v < 0 || v >= int64(o.streamClicks) || (i > 0 && v <= streamed[i-1]) {
				return out, fmt.Errorf("announced click index %d out of order or range", v)
			}
			cfg.AnnouncementIndex[i] = int32(v)
		}
		out.AnnouncementDigest, out.AnnouncementClicks = fmt.Sprintf("%x", o.streamDigest[:8]), o.streamClicks
	}
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
	out.SiftedBits, out.QBER = res.SiftedBits, res.QBER.QBER
	// Alice's ECLeakedBits is her own count of the parities she disclosed; Bob's run statistics are his
	out.CascadeParities, out.CascadeRounds, out.CascadeCorrected = res.Cascade.Parities, res.Cascade.Rounds, res.Cascade.Corrections
	out.ECLeakedBits = res.ECLeakedBits
	out.BasisBits, out.BasisChecked, out.BasisErrors = res.BasisBits, res.BasisChecked, res.BasisErrors
	if o.role == "bob" {
		// channels 1-4 are H, V, D, A: bit 0 and 1 of the HV basis, then of the DA basis
		names := [2][2]string{{"H", "V"}, {"D", "A"}}
		out.PairCounts = map[string]int{}
		for b := range 2 {
			for a := range 2 {
				for m := range 2 {
					out.PairCounts[names[b][a]+names[b][m]] = res.PairCounts[b][a][m]
				}
			}
		}
	}
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
