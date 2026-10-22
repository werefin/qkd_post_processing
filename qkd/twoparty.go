// Two-party post-processing: Alice and Bob as separate processes that exchange only public
// messages; Alice announces timing one way, Bob matches, and each keeps and hashes only its own bits
package qkd

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sync"
)

// Conn carries the protocol's public messages; it must be authenticated, since an attacker who
// can alter them can alter the key's length or the bits it is drawn from
type Conn interface {
	Send(kind string, v any) error
	Recv(kind string, v any) error
}

// TwoPartyConfig holds the public protocol parameters; both sides must use the same values
type TwoPartyConfig struct {
	WindowPS     int64   // Bob's coincidence window, from his coincidence filter
	ChunkSize    int     // CRC chunk size in bits
	KeyAlignBits int     // round the key down to a multiple of this, 0 = don't
	ExpectedQBER float64 // >0: size the sample by OptimalSampleFraction from this prior
	SampleShare  float64 // used when ExpectedQBER is 0
	SampleSeed   int64   // Bob's sample permutation seed, 0 = fresh random (fixed only for tests)
	// AnnouncementDigest is Bob's PublicDigest of Alice's announcement as received, for when
	// pooling has since shifted her timestamps; empty = the digest of the events RunBob is given
	AnnouncementDigest []byte
	// AnnouncementIndex: Bob holds only the announced clicks that can pair with his, and this is each one's
	// index in the whole announcement of AnnouncementEvents clicks; his pair list names those indices, so
	// Alice sees exactly what she would from the whole announcement
	AnnouncementIndex  []int32
	AnnouncementEvents int
	Asymptotic         bool
	FiniteKey          FiniteKeyConfig
	// CascadeSample: Cascade sacrifices a QBER sample; by default nothing is sacrificed and Bob announces the
	// errors correction found per basis, which bound the other basis's phase errors
	CascadeSample bool
}

// TwoPartyResult is one side's outcome; both sides get the same numbers and the same key
type TwoPartyResult struct {
	SiftedBits   int
	QBER         QBERResult
	Cascade      CascadeStats // Bob's side of the run; Alice's ECLeakedBits is her own count of what she disclosed
	ECLeakedBits int          // the parities Cascade disclosed
	// per basis (HV, DA): key bits, and the bits and errors the estimate rests on, the same on both sides
	BasisBits    [2]int
	BasisChecked [2]int
	BasisErrors  [2]int
	// Bob only, never sent: verified pairs per basis by Alice's bit (his after correction) and his own raw bit
	PairCounts    [2][2][2]int
	CRC           ChunkReconcileResult
	LeakedBits    int
	FiniteKey     *FiniteKeyReport
	FinalKeyBits  []byte
	FinalKeyBytes []byte
	KeyIDs        []string // one per 256-bit block of the key, chosen by Alice
}

const twoPartyVersion = 2 // 2: optimized Cascade only

// tpParams is the part of the configuration both sides check is identical
type tpParams struct {
	CascadeSample bool
	ChunkSize     int
	KeyAlignBits  int
	Asymptotic    bool
	EpsSec        float64
	EpsCor        float64
}

func (c TwoPartyConfig) params() tpParams {
	fk := c.FiniteKey.withDefaults()
	return tpParams{c.CascadeSample, c.ChunkSize, c.KeyAlignBits, c.Asymptotic, fk.EpsSec, fk.EpsCor}
}

type tpHello struct {
	Version int
	Events  int
	Digest  []byte
	Params  tpParams
}

type tpSift struct {
	AliceIdx   []int32 // Alice's event index of every sifted pair, in pair order
	SampleSeed int64
	SampleN    int
}

type tpBits struct{ Bits []byte }

type tpCRC struct{ CRC []uint16 }

// tpCascade is one of Bob's requests: parities of ranges of a pass, or, with Done, the end of Cascade and the
// number of parities he counted, which must match Alice's own count
type tpCascade struct {
	Pass     int
	Ranges   [][2]int32
	Done     bool
	Parities int
}

// tpCounts is what correction found: per basis, the key bits and the errors among them, a dropped chunk's bits
// counted as errors
type tpCounts struct {
	K [2]int
	E [2]int
}

// remoteCascade is Alice's oracle as Bob sees it: one round trip per request
type remoteCascade struct{ conn Conn }

func (r remoteCascade) Parities(pass int, ranges [][2]int32) ([]byte, error) {
	if err := r.conn.Send("cascade", tpCascade{Pass: pass, Ranges: ranges}); err != nil {
		return nil, err
	}
	var out tpBits
	if err := r.conn.Recv("cascade_parities", &out); err != nil {
		return nil, err
	}
	return out.Bits, nil
}

// cascadePlanQBER is the QBER Cascade sizes its first blocks with: the sample's if one was taken, else the
// declared expectation
func (c TwoPartyConfig) cascadePlanQBER(sampled QBERResult) float64 {
	if c.CascadeSample && sampled.SampleBits > 0 {
		return sampled.QBER
	}
	if c.ExpectedQBER > 0 {
		return c.ExpectedQBER
	}
	return 0.05
}

func (c TwoPartyConfig) byCorrection() bool { return !c.CascadeSample }

type tpVerify struct {
	Seed []byte
	Tag  []byte
}

type tpOK struct{ OK bool }

type tpPA struct {
	Length int
	Seed   []byte
	KeyIDs []string
}

// PublicDigest fixes Alice's announced events: both sides must hold exactly the same list
func PublicDigest(events []PublicEvent) []byte {
	h := sha256.New()
	var b [10]byte
	for _, e := range events {
		binary.LittleEndian.PutUint64(b[:8], uint64(e.TimestampPS))
		b[8] = byte(e.Basis)
		b[9] = 0
		if e.MultiPhoton {
			b[9] = 1
		}
		h.Write(b[:])
	}
	return h.Sum(nil)
}

// AliceClicks is what Alice's side reads of her own clicks, by index in her announcement
type AliceClicks interface {
	Len() int
	Click(i int) (bit byte, basis Basis, multiPhoton bool)
}

type eventClicks []DetectionEvent

func (e eventClicks) Len() int { return len(e) }
func (e eventClicks) Click(i int) (byte, Basis, bool) {
	return e[i].Bit, e[i].Basis, e[i].MultiPhoton
}

// RunAlice runs Alice's side over her own events (bits included); she has already announced
// StripBit(events) to Bob, and from here on only public messages leave this function
func RunAlice(conn Conn, events []DetectionEvent, cfg TwoPartyConfig) (res TwoPartyResult, err error) {
	return RunAliceClicks(conn, eventClicks(events), digestOr(cfg.AnnouncementDigest, StripBit(events)), cfg)
}

// RunAliceClicks is RunAlice over any view of her clicks, with the digest of what she announced; a block
// streamed in segments keeps only her bits and flags, never the clicks themselves
func RunAliceClicks(conn Conn, clicks AliceClicks, digest []byte, cfg TwoPartyConfig) (res TwoPartyResult, err error) {
	defer abortOnError(conn, &err)
	n := clicks.Len()
	if err = conn.Send("hello", tpHello{twoPartyVersion, n, digest, cfg.params()}); err != nil {
		return res, err
	}
	var sift tpSift
	if err = conn.Recv("sift", &sift); err != nil {
		return res, err
	}
	bits := make([]byte, len(sift.AliceIdx))
	bases := make([]Basis, len(sift.AliceIdx))
	paired := make([]uint64, (n+63)/64) // one bit per click: a pair list may name each at most once
	for i, idx := range sift.AliceIdx {
		if idx < 0 || int(idx) >= n {
			return res, fmt.Errorf("two-party: pair %d names event %d of %d", i, idx, n)
		}
		bit, basis, mp := clicks.Click(int(idx))
		if paired[idx/64]&(1<<(idx%64)) != 0 || mp {
			return res, fmt.Errorf("two-party: event %d is paired twice or is a multi-photon click", idx)
		}
		paired[idx/64] |= 1 << (idx % 64)
		bits[i], bases[i] = bit, basis
	}
	sample, rem, err := splitSample(len(bits), sift.SampleSeed, sift.SampleN)
	if err != nil {
		return res, err
	}
	res.SiftedBits = len(bits)

	mySample := pickBits(bits, sample)
	if err = conn.Send("sample", tpBits{mySample}); err != nil {
		return res, err
	}
	var peer tpBits
	if err = conn.Recv("sample", &peer); err != nil {
		return res, err
	}
	sampleTB, remTB, err := sampleSets(mySample, peer.Bits, bases, sample, rem)
	if err != nil {
		return res, err
	}
	res.QBER = sampleQBER(sampleTB)
	remBits := pickBits(bits, rem)
	// Alice only answers Cascade's requests; what she discloses is counted here, on her side
	oracle := NewCascadeAlice(remBits, sift.SampleSeed)
	for {
		var req tpCascade
		if err = conn.Recv("cascade", &req); err != nil {
			return res, err
		}
		if req.Done {
			if req.Parities != oracle.Disclosed {
				return res, fmt.Errorf("two-party: Bob counted %d Cascade parities, Alice disclosed %d", req.Parities, oracle.Disclosed)
			}
			break
		}
		par, perr := oracle.Parities(req.Pass, req.Ranges)
		if perr != nil {
			return res, perr
		}
		if err = conn.Send("cascade_parities", tpBits{par}); err != nil {
			return res, err
		}
	}
	res.ECLeakedBits = oracle.Disclosed
	return finishTwoParty(conn, true, res, cfg, remBits, nil, sampleTB, remTB)
}

// RunBob runs Bob's side: he holds Alice's announced events and his own events already on
// Alice's clock and filtered, matches them, decodes his bits onto Alice's and verifies
func RunBob(conn Conn, alice []PublicEvent, bobEvents []DetectionEvent, cfg TwoPartyConfig) (res TwoPartyResult, err error) {
	defer abortOnError(conn, &err)
	var hello tpHello
	if err = conn.Recv("hello", &hello); err != nil {
		return res, err
	}
	switch {
	case hello.Version != twoPartyVersion:
		return res, fmt.Errorf("two-party: peer speaks version %d, this side %d", hello.Version, twoPartyVersion)
	case hello.Params != cfg.params():
		return res, fmt.Errorf("two-party: parameters differ: Alice %+v, Bob %+v", hello.Params, cfg.params())
	case hello.Events != announcedEvents(cfg, alice) || !bytes.Equal(hello.Digest, digestOr(cfg.AnnouncementDigest, alice)):
		return res, errors.New("two-party: Alice's announced events differ from the ones Bob received")
	case cfg.AnnouncementIndex != nil && (len(cfg.AnnouncementIndex) != len(alice) || len(cfg.AnnouncementDigest) == 0):
		return res, errors.New("two-party: a reduced announcement needs every click's index and the whole one's digest")
	}

	pairs := MatchCoincidences(alice, StripBit(bobEvents), cfg.WindowPS)
	sift := tpSift{AliceIdx: make([]int32, len(pairs))}
	bits := make([]byte, len(pairs))
	bases := make([]Basis, len(pairs))
	for i, p := range pairs {
		sift.AliceIdx[i] = int32(p.AliceIdx)
		if cfg.AnnouncementIndex != nil {
			sift.AliceIdx[i] = cfg.AnnouncementIndex[p.AliceIdx]
		}
		bits[i], bases[i] = bobEvents[p.BobIdx].Bit, alice[p.AliceIdx].Basis
	}
	share := cfg.SampleShare
	if cfg.ExpectedQBER > 0 {
		share = OptimalSampleFraction(len(pairs), cfg.ExpectedQBER, cfg.FiniteKey)
	}
	if cfg.byCorrection() {
		share = 0 // every bit is key; the errors correction finds are the estimate
	}
	sift.SampleN = max(0, min(int(float64(len(pairs))*share), len(pairs)))
	sift.SampleSeed = cfg.SampleSeed
	if sift.SampleSeed == 0 {
		if sift.SampleSeed, err = randomSeed(); err != nil {
			return res, err
		}
	}
	if err = conn.Send("sift", sift); err != nil {
		return res, err
	}
	sample, rem, err := splitSample(len(bits), sift.SampleSeed, sift.SampleN)
	if err != nil {
		return res, err
	}
	res.SiftedBits = len(bits)

	var peer tpBits
	if err = conn.Recv("sample", &peer); err != nil {
		return res, err
	}
	mySample := pickBits(bits, sample)
	if err = conn.Send("sample", tpBits{mySample}); err != nil {
		return res, err
	}
	sampleTB, remTB, err := sampleSets(peer.Bits, mySample, bases, sample, rem)
	if err != nil {
		return res, err
	}
	res.QBER = sampleQBER(sampleTB)
	remBits := pickBits(bits, rem)

	corrected, cstats, err := CascadeBob(remBits, cfg.cascadePlanQBER(res.QBER), CascadeConfig{Seed: sift.SampleSeed}, remoteCascade{conn})
	if err != nil {
		return res, err
	}
	res.Cascade = cstats
	if err = conn.Send("cascade", tpCascade{Done: true, Parities: res.Cascade.Parities}); err != nil {
		return res, err
	}
	res.ECLeakedBits = res.Cascade.Parities
	return finishTwoParty(conn, false, res, cfg, corrected, remBits, sampleTB, remTB)
}

// finishTwoParty is CRC verification, the correctness check and privacy amplification; alice says which side
// this is, since Alice picks every seed and key_ID, and raw is Bob's bits before correction when errors are
// counted by correction, nil otherwise
func finishTwoParty(conn Conn, alice bool, res TwoPartyResult, cfg TwoPartyConfig, own, raw []byte, sampleTB, remTB timedBits) (TwoPartyResult, error) {
	myCRC := ComputeChunkCRC(own, cfg.ChunkSize)
	var peer tpCRC
	if alice {
		if err := conn.Send("crc", tpCRC{myCRC}); err != nil {
			return res, err
		}
		if err := conn.Recv("crc", &peer); err != nil {
			return res, err
		}
	} else {
		if err := conn.Recv("crc", &peer); err != nil {
			return res, err
		}
		if err := conn.Send("crc", tpCRC{myCRC}); err != nil {
			return res, err
		}
	}
	if len(peer.CRC) != len(myCRC) {
		return res, fmt.Errorf("two-party: %d CRCs from the peer, %d here", len(peer.CRC), len(myCRC))
	}
	res.CRC = ReconcileChunks(own, myCRC, peer.CRC, cfg.ChunkSize)
	for i, b := range remTB.Basis {
		if b > 1 {
			continue
		}
		res.BasisBits[b]++
		if !alice && len(raw) == len(own) && myCRC[i/cfg.ChunkSize] == peer.CRC[i/cfg.ChunkSize] {
			res.PairCounts[b][own[i]&1][raw[i]&1]++
		}
	}
	res.LeakedBits = res.ECLeakedBits + res.CRC.LeakedBits
	surviving := res.CRC.Surviving
	n := len(surviving)

	fk := cfg.FiniteKey.withDefaults()
	budget := SplitSecurityBudget(fk.EpsSec, fk.EpsCor)
	l := 0
	if cfg.Asymptotic {
		l = SecureKeyLength(n, res.QBER.QBER, res.LeakedBits)
	} else {
		rep := FiniteKeyReport{Bits: n, TagBits: UniversalHashTagBits(budget.EpsCor)}
		if cfg.byCorrection() {
			counts, err := exchangeCounts(conn, alice, own, raw, myCRC, peer.CRC, cfg.ChunkSize, remTB)
			if err != nil {
				return res, err
			}
			res.QBER = QBERResult{ErrorBits: counts.E[0] + counts.E[1], SampleBits: counts.K[0] + counts.K[1]}
			if res.QBER.SampleBits > 0 {
				res.QBER.QBER = float64(res.QBER.ErrorBits) / float64(res.QBER.SampleBits)
			}
			tail, chain := correctionBudget(budget.EpsPE)
			rep.Phi, rep.PhiPoint = phaseErrorFromCounts(counts.K, counts.K, counts.E, n, tail)
			rep.ChainBits = chain
			res.BasisChecked, res.BasisErrors = counts.K, counts.E
		} else {
			rep.Phi, rep.PhiPoint = crossBasisPhaseError(sampleTB, remTB, n, estimationTail(budget.EpsPE))
			for i, b := range sampleTB.Basis {
				if b <= 1 {
					res.BasisChecked[b]++
					if sampleTB.Alice[i] != sampleTB.Bob[i] {
						res.BasisErrors[b]++
					}
				}
			}
		}
		rep.LeakedBits = res.LeakedBits + rep.TagBits
		res.FiniteKey = &rep
		if n == 0 {
			return res, sendOrRecvDone(conn, alice)
		}
		var v tpVerify
		if alice {
			seed, err := UniversalHashSeed(rep.TagBits, n)
			if err != nil {
				return res, err
			}
			v = tpVerify{seed, ComputeUniversalHashTag(surviving, seed, rep.TagBits)}
			if err := conn.Send("verify", v); err != nil {
				return res, err
			}
			var ok tpOK
			if err := conn.Recv("verified", &ok); err != nil {
				return res, err
			}
			rep.Verified = ok.OK
		} else {
			if err := conn.Recv("verify", &v); err != nil {
				return res, err
			}
			rep.Verified = len(v.Seed) > 0 && VerifyUniversalHash(surviving, v.Tag, v.Seed, rep.TagBits)
			if err := conn.Send("verified", tpOK{rep.Verified}); err != nil {
				return res, err
			}
		}
		res.FiniteKey = &rep
		if !rep.Verified {
			return res, nil
		}
		l = FiniteKeySecureKeyLength(n, rep.Phi, res.LeakedBits+rep.ChainBits, budget.EpsCor, budget.EpsPA) // tag in the epsCor term
	}
	if cfg.KeyAlignBits > 0 {
		l -= l % cfg.KeyAlignBits
	}
	if res.FiniteKey != nil {
		res.FiniteKey.Length = l
	}

	var pa tpPA
	if alice {
		pa.Length = l
		if l > 0 {
			seed, err := toeplitzSeed(l, n)
			if err != nil {
				return res, err
			}
			pa.Seed = seed
			for range (l + 255) / 256 {
				id, err := newTwoPartyKeyID()
				if err != nil {
					return res, err
				}
				pa.KeyIDs = append(pa.KeyIDs, id)
			}
		}
		if err := conn.Send("pa", pa); err != nil {
			return res, err
		}
	} else {
		if err := conn.Recv("pa", &pa); err != nil {
			return res, err
		}
		if pa.Length != l {
			return res, fmt.Errorf("two-party: Alice computed a %d-bit key, Bob %d", pa.Length, l)
		}
		if l > 0 && (len(pa.Seed) != l+n-1 || len(pa.KeyIDs) != (l+255)/256) {
			return res, errors.New("two-party: malformed privacy-amplification message")
		}
	}
	if l > 0 {
		res.FinalKeyBits = toeplitzHashFast(surviving, pa.Seed, l)
		res.FinalKeyBytes = packToBytes(res.FinalKeyBits)
		res.KeyIDs = pa.KeyIDs
	}
	return res, sendOrRecvDone(conn, alice)
}

// exchangeCounts: Bob counts, per basis, the bits correction changed in chunks whose CRCs matched and every bit
// of a dropped chunk, and announces them; Alice checks the per-basis totals against her own
func exchangeCounts(conn Conn, alice bool, own, raw []byte, myCRC, peerCRC []uint16, chunk int, remTB timedBits) (tpCounts, error) {
	var mine [2]int
	for _, b := range remTB.Basis {
		if b <= 1 {
			mine[b]++
		}
	}
	var c tpCounts
	if alice {
		if err := conn.Recv("counts", &c); err != nil {
			return c, err
		}
		if c.K != mine || c.E[0] < 0 || c.E[1] < 0 || c.E[0] > c.K[0] || c.E[1] > c.K[1] {
			return c, fmt.Errorf("two-party: Bob's error counts %+v do not fit Alice's %v bits per basis", c, mine)
		}
		return c, nil
	}
	if len(raw) != len(own) || len(remTB.Basis) != len(own) {
		return c, errors.New("two-party: counting errors needs Bob's bits before and after correction")
	}
	for i, b := range remTB.Basis {
		if b > 1 {
			continue
		}
		c.K[b]++
		ci := i / chunk
		if myCRC[ci] != peerCRC[ci] || raw[i] != own[i] {
			c.E[b]++
		}
	}
	return c, conn.Send("counts", c)
}

// sendOrRecvDone closes the protocol: Bob confirms he reached the end
func sendOrRecvDone(conn Conn, alice bool) error {
	if alice {
		return conn.Recv("done", &struct{}{})
	}
	return conn.Send("done", struct{}{})
}

// abortOnError tells the peer why this side stopped, so it does not wait for messages that never come
func abortOnError(conn Conn, err *error) {
	if *err != nil && !errors.Is(*err, ErrPeerAborted) {
		conn.Send("abort", map[string]string{"reason": (*err).Error()})
	}
}

// ErrPeerAborted is returned when the peer stopped the protocol
var ErrPeerAborted = errors.New("two-party: the peer aborted")

// announcedEvents is how many clicks Alice announced: all Bob holds, or the whole of a reduced announcement
func announcedEvents(cfg TwoPartyConfig, alice []PublicEvent) int {
	if cfg.AnnouncementIndex != nil {
		return cfg.AnnouncementEvents
	}
	return len(alice)
}

func digestOr(d []byte, events []PublicEvent) []byte {
	if len(d) > 0 {
		return d
	}
	return PublicDigest(events)
}

func randomSeed() (int64, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return 0, err
	}
	return n.Int64(), nil
}

func newTwoPartyKeyID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// splitSample is siftSample's public permutation: the first sampleN positions are sacrificed
func splitSample(n int, seed int64, sampleN int) (sample, rem []int, err error) {
	if sampleN < 0 || sampleN > n {
		return nil, nil, fmt.Errorf("two-party: sample of %d out of %d pairs", sampleN, n)
	}
	order := siftSampleOrder(n, seed)
	return order[:sampleN], order[sampleN:], nil
}

func pickBits(bits []byte, idx []int) []byte {
	out := make([]byte, len(idx))
	for i, j := range idx {
		out[i] = bits[j]
	}
	return out
}

// sampleSets builds the sample (both sides' revealed bits, with bases) and the bases of the
// kept bits, which is all crossBasisPhaseError reads
func sampleSets(aliceSample, bobSample []byte, bases []Basis, sample, rem []int) (timedBits, timedBits, error) {
	if len(aliceSample) != len(sample) || len(bobSample) != len(sample) {
		return timedBits{}, timedBits{}, fmt.Errorf("two-party: sample has %d and %d bits, expected %d", len(aliceSample), len(bobSample), len(sample))
	}
	s := timedBits{Alice: aliceSample, Bob: bobSample, Basis: make([]Basis, len(sample))}
	for i, j := range sample {
		s.Basis[i] = bases[j]
	}
	r := timedBits{Basis: make([]Basis, len(rem))}
	for i, j := range rem {
		r.Basis[i] = bases[j]
	}
	return s, r, nil
}

func sampleQBER(s timedBits) QBERResult {
	errs := 0
	for i := range s.Alice {
		if s.Alice[i] != s.Bob[i] {
			errs++
		}
	}
	q := 0.0
	if len(s.Alice) > 0 {
		q = float64(errs) / float64(len(s.Alice))
	}
	return QBERResult{ErrorBits: errs, SampleBits: len(s.Alice), QBER: q}
}

// PipeConns connects two in-process sides, for tests and for running both parties in one place
func PipeConns() (alice, bob Conn) {
	ab, ba := make(chan pipeMsg, 64), make(chan pipeMsg, 64)
	return &pipeConn{out: ab, in: ba}, &pipeConn{out: ba, in: ab}
}

type pipeMsg struct {
	kind string
	data []byte
}

type pipeConn struct {
	out, in chan pipeMsg
	mu      sync.Mutex
	Log     []string // kinds sent, in order
}

func (c *pipeConn) Send(kind string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.Log = append(c.Log, kind)
	c.mu.Unlock()
	c.out <- pipeMsg{kind, data}
	return nil
}

func (c *pipeConn) Recv(kind string, v any) error {
	m := <-c.in
	return DecodeMessage(m.kind, m.data, kind, v)
}

// DecodeMessage checks a received message is the expected kind (an abort becomes ErrPeerAborted)
// and decodes it; transports share it so every one enforces the same order
func DecodeMessage(gotKind string, data []byte, wantKind string, v any) error {
	if gotKind == "abort" {
		var a struct{ Reason string }
		json.Unmarshal(data, &a)
		return fmt.Errorf("%w: %s", ErrPeerAborted, a.Reason)
	}
	if gotKind != wantKind {
		return fmt.Errorf("two-party: expected a %q message, got %q", wantKind, gotKind)
	}
	return json.Unmarshal(data, v)
}
