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
	"runtime"
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
	RateMargin   float64 // polar starting rate margin below each block's capacity
	ListSize     int     // SCL list size, 0 = plain successive cancellation
	BlockLength  int     // polar block length, a power of two; 0 = the largest covering the key
	ChunkSize    int     // CRC chunk size in bits
	KeyAlignBits int     // round the key down to a multiple of this, 0 = don't
	ExpectedQBER float64 // >0: size the sample by OptimalSampleFraction from this prior
	SampleShare  float64 // used when ExpectedQBER is 0
	TimingBins   int     // >0: Bob decodes with per-bit LLRs from his own timing; codes stay flat
	SampleSeed   int64   // Bob's sample permutation seed, 0 = fresh random (fixed only for tests)
	// AnnouncementDigest is Bob's PublicDigest of Alice's announcement as received, for when
	// pooling has since shifted her timestamps; empty = the digest of the events RunBob is given
	AnnouncementDigest []byte
	Asymptotic         bool
	FiniteKey          FiniteKeyConfig
	// Method is the error correction, "polar" (the default) or "ldpc5g"; LDPC does its matrix work
	Method string
	LDPC   LDPC5GBackend
}

func (c TwoPartyConfig) method() string {
	if c.Method == "" {
		return "polar"
	}
	return c.Method
}

// TwoPartyResult is one side's outcome; both sides get the same numbers and the same key
type TwoPartyResult struct {
	SiftedBits         int
	QBER               QBERResult
	PolarBlocks        int
	PolarAttempts      int
	PolarBitsCorrected int // Bob only: bits his decoder flipped
	PolarLeakedBits    int
	Method             string
	LDPC               LDPC5GResult
	ECLeakedBits       int // what error correction disclosed, whichever method ran
	CRC                ChunkReconcileResult
	LeakedBits         int
	FiniteKey          *FiniteKeyReport
	FinalKeyBits       []byte
	FinalKeyBytes      []byte
	KeyIDs             []string // one per 256-bit block of the key, chosen by Alice
}

const twoPartyVersion = 1

// tpParams is the part of the configuration both sides check is identical
type tpParams struct {
	Method       string
	RateMargin   float64
	ListSize     int
	BlockLength  int
	ChunkSize    int
	KeyAlignBits int
	TimingBins   int
	Asymptotic   bool
	EpsSec       float64
	EpsCor       float64
}

func (c TwoPartyConfig) params() tpParams {
	fk := c.FiniteKey.withDefaults()
	return tpParams{c.method(), c.RateMargin, c.ListSize, c.BlockLength, c.ChunkSize, c.KeyAlignBits, c.TimingBins, c.Asymptotic, fk.EpsSec, fk.EpsCor}
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

type tpPolarBlock struct {
	Index  int
	K      int
	Frozen []byte
	Seed   []byte
	Tag    []byte
}

type tpPolarRound struct{ Blocks []tpPolarBlock }

type tpFailed struct{ Failed []int }

type tpCRC struct{ CRC []uint16 }

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

// RunAlice runs Alice's side over her own events (bits included); she has already announced
// StripBit(events) to Bob, and from here on only public messages leave this function
func RunAlice(conn Conn, events []DetectionEvent, cfg TwoPartyConfig) (res TwoPartyResult, err error) {
	defer abortOnError(conn, &err)
	pub := StripBit(events)
	if err = conn.Send("hello", tpHello{twoPartyVersion, len(pub), PublicDigest(pub), cfg.params()}); err != nil {
		return res, err
	}
	var sift tpSift
	if err = conn.Recv("sift", &sift); err != nil {
		return res, err
	}
	bits := make([]byte, len(sift.AliceIdx))
	bases := make([]Basis, len(sift.AliceIdx))
	paired := make([]bool, len(events))
	for i, idx := range sift.AliceIdx {
		if idx < 0 || int(idx) >= len(events) {
			return res, fmt.Errorf("two-party: pair %d names event %d of %d", i, idx, len(events))
		}
		if paired[idx] || events[idx].MultiPhoton {
			return res, fmt.Errorf("two-party: event %d is paired twice or is a multi-photon click", idx)
		}
		paired[idx] = true
		bits[i], bases[i] = events[idx].Bit, events[idx].Basis
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
	res.Method = cfg.method()
	if res.Method == "ldpc5g" {
		if res.LDPC, err = aliceLDPC5G(conn, cfg, remBits, res.QBER.QBER, sift.SampleSeed); err != nil {
			return res, err
		}
		res.ECLeakedBits = res.LDPC.LeakedBits
		return finishTwoParty(conn, true, res, cfg, remBits, sampleTB, remTB)
	}

	blockLen := polarBlockLength(cfg.BlockLength, len(remBits))
	if blockLen < 2 {
		return res, errors.New("two-party: nothing left to reconcile after the QBER sample")
	}
	plan := newPolarPlan(remBits, res.QBER.QBER, blockLen, cfg.RateMargin)
	attempt := make([]int, plan.blocks)
	pending := make([]int, plan.blocks)
	for i := range pending {
		pending[i] = i
	}
	for len(pending) > 0 {
		round := tpPolarRound{}
		for _, bi := range pending {
			blk, berr := plan.aliceBlock(bi, attempt[bi])
			if berr != nil {
				return res, berr
			}
			round.Blocks = append(round.Blocks, blk)
			res.PolarAttempts++
		}
		if err = conn.Send("polar", round); err != nil {
			return res, err
		}
		var failed tpFailed
		if err = conn.Recv("polar_failed", &failed); err != nil {
			return res, err
		}
		next, ferr := plan.retry(pending, failed.Failed, attempt)
		if ferr != nil {
			return res, ferr
		}
		pending = next
	}
	res.PolarBlocks, res.PolarLeakedBits = plan.blocks, plan.leaked(attempt)
	res.ECLeakedBits = res.PolarLeakedBits
	return finishTwoParty(conn, true, res, cfg, remBits, sampleTB, remTB)
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
	case hello.Events != len(alice) || !bytes.Equal(hello.Digest, digestOr(cfg.AnnouncementDigest, alice)):
		return res, errors.New("two-party: Alice's announced events differ from the ones Bob received")
	}

	pairs := MatchCoincidences(alice, StripBit(bobEvents), cfg.WindowPS)
	sift := tpSift{AliceIdx: make([]int32, len(pairs))}
	bits := make([]byte, len(pairs))
	bases := make([]Basis, len(pairs))
	dts := make([]int64, len(pairs))
	for i, p := range pairs {
		sift.AliceIdx[i] = int32(p.AliceIdx)
		bits[i], bases[i] = bobEvents[p.BobIdx].Bit, alice[p.AliceIdx].Basis
		dts[i] = bobEvents[p.BobIdx].TimestampPS - alice[p.AliceIdx].TimestampPS
	}
	share := cfg.SampleShare
	if cfg.ExpectedQBER > 0 {
		share = OptimalSampleFraction(len(pairs), cfg.ExpectedQBER, cfg.FiniteKey)
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

	// soft decoding uses Bob's own timing only; Alice's codes are built from the flat QBER
	q := make([]float64, len(remBits))
	for i := range q {
		q[i] = res.QBER.QBER
	}
	if cfg.TimingBins > 0 && len(sample) > 0 {
		sampleTB.Dt = pickInt64(dts, sample)
		if m, merr := EstimateTimingErrorModel(sampleTB.Alice, sampleTB.Bob, sampleTB.Dt, cfg.WindowPS, cfg.TimingBins); merr == nil {
			q = m.Rates(pickInt64(dts, rem))
		}
	}

	res.Method = cfg.method()
	if res.Method == "ldpc5g" {
		var corrected []byte
		if corrected, res.LDPC, err = bobLDPC5G(conn, cfg, remBits, q, res.QBER.QBER, sift.SampleSeed); err != nil {
			return res, err
		}
		res.ECLeakedBits = res.LDPC.LeakedBits
		return finishTwoParty(conn, false, res, cfg, corrected, sampleTB, remTB)
	}

	blockLen := polarBlockLength(cfg.BlockLength, len(remBits))
	if blockLen < 2 {
		return res, errors.New("two-party: nothing left to reconcile after the QBER sample")
	}
	plan := newPolarPlan(remBits, res.QBER.QBER, blockLen, cfg.RateMargin)
	corrected := make([]byte, len(remBits))
	attempt := make([]int, plan.blocks)
	pending := make([]int, plan.blocks)
	for i := range pending {
		pending[i] = i
	}
	for len(pending) > 0 {
		var round tpPolarRound
		if err = conn.Recv("polar", &round); err != nil {
			return res, err
		}
		if len(round.Blocks) != len(pending) {
			return res, fmt.Errorf("two-party: polar round has %d blocks, expected %d", len(round.Blocks), len(pending))
		}
		var failed []int
		for i, blk := range round.Blocks {
			bi := pending[i]
			ok, derr := plan.bobBlock(blk, bi, attempt[bi], q, cfg.ListSize, corrected)
			if derr != nil {
				return res, derr
			}
			res.PolarAttempts++
			if !ok {
				failed = append(failed, bi)
			}
		}
		if err = conn.Send("polar_failed", tpFailed{failed}); err != nil {
			return res, err
		}
		if pending, err = plan.retry(pending, failed, attempt); err != nil {
			return res, err
		}
	}
	for i := range remBits {
		if corrected[i] != remBits[i] {
			res.PolarBitsCorrected++
		}
	}
	res.PolarBlocks, res.PolarLeakedBits = plan.blocks, plan.leaked(attempt)
	res.ECLeakedBits = res.PolarLeakedBits
	return finishTwoParty(conn, false, res, cfg, corrected, sampleTB, remTB)
}

// finishTwoParty is CRC verification, the correctness check and privacy amplification;
// alice says which side this is, since Alice picks every seed and key_ID
func finishTwoParty(conn Conn, alice bool, res TwoPartyResult, cfg TwoPartyConfig, own []byte, sampleTB, remTB timedBits) (TwoPartyResult, error) {
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
		rep.Phi, rep.PhiPoint = crossBasisPhaseError(sampleTB, remTB, n, budget.EpsPE)
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
		l = FiniteKeySecureKeyLength(n, rep.Phi, rep.LeakedBits, budget.EpsCor, budget.EpsPA)
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

func pickInt64(v []int64, idx []int) []int64 {
	out := make([]int64, len(idx))
	for i, j := range idx {
		out[i] = v[j]
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

// PolarMaxDefaultBlock caps the default block length: measured, the construction degrades beyond
// 2^16 and at 2^20 SCL decoding fails whole blocks (doc/ec_comparison.md)
const PolarMaxDefaultBlock = 1 << 16

func polarBlockLength(cfgLen, n int) int {
	if cfgLen > 0 {
		return cfgLen
	}
	return min(largestPowerOfTwoAtMost(n), PolarMaxDefaultBlock)
}

// polarPlan is PolarReconcileSoft's block bookkeeping with a flat rate, shared by both sides so
// they agree on every block's code without exchanging anything but the frozen values
type polarPlan struct {
	n, blocks int
	own       []byte
	qber      float64
	margin    float64
	uMeans    [][]float64
	capacity  []float64
	blen      []int
}

func newPolarPlan(own []byte, qber float64, n int, margin float64) *polarPlan {
	p := &polarPlan{n: n, own: own, qber: qber, margin: margin}
	p.blocks = (len(own) + n - 1) / n
	means := make([]float64, n)
	for bi := range p.blocks {
		blen := min(n, len(own)-bi*n)
		capacity := 0.0
		for i := range n {
			q := qber
			if i >= blen {
				q = qMinTiming // padding is known 0 on both sides
			}
			means[i] = gaInitialMean(q)
			capacity += 1 - binaryEntropy(q)
		}
		p.uMeans = append(p.uMeans, gaMeansFrom(means))
		p.capacity = append(p.capacity, capacity)
		p.blen = append(p.blen, blen)
	}
	return p
}

func (p *polarPlan) k(bi, attempt int) int {
	m := p.margin + float64(attempt)*polarAdaptiveStep
	return max(1, min(p.n-1, int(p.capacity[bi]-m*float64(p.blen[bi]))))
}

func (p *polarPlan) block(bi int) []byte {
	b := make([]byte, p.n)
	copy(b, p.own[bi*p.n:bi*p.n+p.blen[bi]])
	return b
}

// aliceBlock is Alice's disclosure for one attempt: the frozen values and a fresh block hash
func (p *polarPlan) aliceBlock(bi, attempt int) (tpPolarBlock, error) {
	k := p.k(bi, attempt)
	code := codeFromMeans(p.uMeans[bi], k)
	u, err := PolarTransform(p.block(bi))
	if err != nil {
		return tpPolarBlock{}, err
	}
	frozen := make([]byte, len(code.FrozenIdx))
	for i, idx := range code.FrozenIdx {
		frozen[i] = u[idx]
	}
	seed, err := UniversalHashSeed(polarAdaptiveTagBits, p.blen[bi])
	if err != nil {
		return tpPolarBlock{}, err
	}
	tag := ComputeUniversalHashTag(p.own[bi*p.n:bi*p.n+p.blen[bi]], seed, polarAdaptiveTagBits)
	return tpPolarBlock{Index: bi, K: k, Frozen: frozen, Seed: seed, Tag: tag}, nil
}

// bobBlock decodes one attempt into corrected and reports whether its hash matched
func (p *polarPlan) bobBlock(blk tpPolarBlock, bi, attempt int, q []float64, listSize int, corrected []byte) (bool, error) {
	k := p.k(bi, attempt)
	if blk.Index != bi || blk.K != k {
		return false, fmt.Errorf("two-party: polar block %d attempt %d: Alice used k=%d, expected %d", bi, attempt, blk.K, k)
	}
	code := codeFromMeans(p.uMeans[bi], k)
	if len(blk.Frozen) != len(code.FrozenIdx) {
		return false, fmt.Errorf("two-party: polar block %d: %d frozen values, expected %d", bi, len(blk.Frozen), len(code.FrozenIdx))
	}
	qb := make([]float64, p.n)
	for i := range p.n {
		qb[i] = qMinTiming
		if i < p.blen[bi] {
			qb[i] = q[bi*p.n+i]
		}
	}
	u, err := PolarDecodeLLR(code, SoftLLRs(p.block(bi), qb), blk.Frozen, PolarDecodeConfig{ListSize: listSize})
	if err != nil {
		return false, err
	}
	x, _ := PolarTransform(u)
	copy(corrected[bi*p.n:], x[:p.blen[bi]])
	return len(blk.Seed) > 0 && VerifyUniversalHash(x[:p.blen[bi]], blk.Tag, blk.Seed, polarAdaptiveTagBits), nil
}

// retry advances the failed blocks to their next attempt; a block stops at k=1 or the last
// attempt and is then left to the CRC stage, exactly as PolarReconcileSoft does
func (p *polarPlan) retry(pending, failed []int, attempt []int) ([]int, error) {
	inPending := map[int]bool{}
	for _, bi := range pending {
		inPending[bi] = true
	}
	var next []int
	for _, bi := range failed {
		if !inPending[bi] {
			return nil, fmt.Errorf("two-party: block %d reported failed but was not sent", bi)
		}
		if p.k(bi, attempt[bi]) == 1 || attempt[bi] == polarAdaptiveMaxSteps {
			continue
		}
		attempt[bi]++
		next = append(next, bi)
	}
	return next, nil
}

// leaked is the disclosure: the final frozen count of every block plus, per attempt, the tag and
// Bob's verdict on it
func (p *polarPlan) leaked(attempt []int) int {
	total := 0
	for bi := range p.blocks {
		total += p.n - p.k(bi, attempt[bi]) + (polarAdaptiveTagBits+1)*(attempt[bi]+1) // tag and verdict
	}
	return total
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

type tpLDPCFrame struct {
	Syndrome, TagSeed, Tag []byte
}

type tpLDPCStart struct {
	Digest []byte
	Frames []tpLDPCFrame
}

type tpLDPCRound struct {
	Failed []int // frames to reveal more for
	Final  []int // frames that failed with everything revealed
}

type tpLDPCReveal struct {
	Values [][]byte // per frame of the last Failed list, in its order
}

// aliceLDPC5G sends each frame's syndrome and tag, then the next slice of punctured values for every
// frame Bob reports failed, until none is left
func aliceLDPC5G(conn Conn, cfg TwoPartyConfig, rem []byte, qber float64, seed int64) (LDPC5GResult, error) {
	var res LDPC5GResult
	if cfg.LDPC == nil {
		return res, errors.New("two-party: ldpc5g needs a backend")
	}
	p, err := PlanLDPC5G(cfg.LDPC, qber, seed, len(rem))
	if err != nil {
		return res, err
	}
	kl := len(p.Key)
	n := (len(rem) + kl - 1) / kl
	frames := make([]*ldpc5gAlice, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for f := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			frames[f], errs[f] = p.aliceFrame(cfg.LDPC, rem[f*kl:min((f+1)*kl, len(rem))])
		}()
	}
	wg.Wait()
	start := tpLDPCStart{Digest: p.Digest()}
	for f := range n {
		if errs[f] != nil {
			return res, errs[f]
		}
		start.Frames = append(start.Frames, tpLDPCFrame{frames[f].syndrome, frames[f].tagSeed, frames[f].tag})
	}
	if err := conn.Send("ldpc5g", start); err != nil {
		return res, err
	}
	attempts := make([]int, n)
	round := make([]int, n)
	for i := range round {
		round[i] = i
	}
	for len(round) > 0 {
		for _, f := range round {
			attempts[f]++
		}
		var r tpLDPCRound
		if err := conn.Recv("ldpc5g_failed", &r); err != nil {
			return res, err
		}
		res.Failed += len(r.Final)
		in := map[int]bool{}
		for _, f := range round {
			in[f] = true
		}
		var reveal tpLDPCReveal
		for _, f := range r.Failed {
			if !in[f] || frames[f].revealed == len(p.Punctured) {
				return res, fmt.Errorf("two-party: frame %d cannot be revealed further", f)
			}
			reveal.Values = append(reveal.Values, p.reveal(frames[f]))
		}
		if len(r.Failed) == 0 {
			break
		}
		if err := conn.Send("ldpc5g_reveal", reveal); err != nil {
			return res, err
		}
		round = r.Failed
	}
	res.M, res.Z, res.FrameKeyBits, res.Credit, res.Frames = p.M, p.Z, kl, p.Credit, n
	for f := range n {
		res.Attempts += attempts[f]
		res.Revealed += frames[f].revealed
		res.LeakedBits += p.Leak(frames[f].revealed, attempts[f])
	}
	return res, nil
}

// bobLDPC5G decodes every pending frame in parallel, reports which failed, and applies Alice's
// reveals; a frame is accepted only when its syndrome holds and its tag matches
func bobLDPC5G(conn Conn, cfg TwoPartyConfig, rem []byte, q []float64, qber float64, seed int64) ([]byte, LDPC5GResult, error) {
	var res LDPC5GResult
	if cfg.LDPC == nil {
		return nil, res, errors.New("two-party: ldpc5g needs a backend")
	}
	p, err := PlanLDPC5G(cfg.LDPC, qber, seed, len(rem))
	if err != nil {
		return nil, res, err
	}
	var start tpLDPCStart
	if err := conn.Recv("ldpc5g", &start); err != nil {
		return nil, res, err
	}
	kl := len(p.Key)
	n := (len(rem) + kl - 1) / kl
	if !bytes.Equal(start.Digest, p.Digest()) || len(start.Frames) != n {
		return nil, res, errors.New("two-party: Alice's LDPC frame layout differs from Bob's")
	}
	frames := make([]*ldpc5gBob, n)
	for f := range n {
		lo, hi := f*kl, min((f+1)*kl, len(rem))
		fr := start.Frames[f]
		if frames[f], err = p.bobFrame(rem[lo:hi], q[lo:hi], fr.Syndrome, fr.TagSeed, fr.Tag); err != nil {
			return nil, res, err
		}
	}
	round := make([]int, n)
	for i := range round {
		round[i] = i
	}
	for len(round) > 0 {
		errs := make([]error, len(round))
		var wg sync.WaitGroup
		sem := make(chan struct{}, runtime.NumCPU())
		for i, f := range round {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				errs[i] = p.attempt(cfg.LDPC, frames[f])
				<-sem
			}()
		}
		wg.Wait()
		var r tpLDPCRound
		for i, f := range round {
			if errs[i] != nil {
				return nil, res, errs[i]
			}
			switch fr := frames[f]; {
			case fr.ok:
			case fr.revealed == len(p.Punctured):
				r.Final = append(r.Final, f)
			default:
				r.Failed = append(r.Failed, f)
			}
		}
		if err := conn.Send("ldpc5g_failed", r); err != nil {
			return nil, res, err
		}
		res.Failed += len(r.Final)
		if len(r.Failed) == 0 {
			break
		}
		var reveal tpLDPCReveal
		if err := conn.Recv("ldpc5g_reveal", &reveal); err != nil {
			return nil, res, err
		}
		if len(reveal.Values) != len(r.Failed) {
			return nil, res, errors.New("two-party: reveal does not match the failed frames")
		}
		for i, f := range r.Failed {
			if err := p.apply(frames[f], reveal.Values[i]); err != nil {
				return nil, res, err
			}
		}
		round = r.Failed
	}
	corrected := make([]byte, 0, len(rem))
	res.M, res.Z, res.FrameKeyBits, res.Credit, res.Frames = p.M, p.Z, kl, p.Credit, n
	for f, fr := range frames {
		corrected = append(corrected, fr.out...)
		res.Attempts += fr.attempts
		res.Revealed += fr.revealed
		res.LeakedBits += p.Leak(fr.revealed, fr.attempts)
		for i, b := range fr.out {
			if b != rem[f*kl+i] {
				res.BitsCorrected++
			}
		}
	}
	return corrected, res, nil
}
