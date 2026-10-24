package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Settings every node of the network shares for continuous two-party QKD
type Settings struct {
	SegmentS     float64 `json:"segment_s"`
	BlockS       float64 `json:"block_s"`    // the longest block when BlockAuto, else every block's length
	BlockAuto    bool    `json:"block_auto"` // each link's blocks planned from its last ones (services/agent/blockplan.py)
	ExpectedQBER float64 `json:"expected_qber"`
	KeepBlocks   int     `json:"keep_blocks"`
}

// Node is one node as the operator wants it: the site its TimeTagger is in the node's acquisition config,
// and whether it takes part at all
type Node struct {
	Site    string `json:"site"`
	Enabled bool   `json:"enabled"`
}

// Link is one QKD link: the announcer sends its public timing to the other node, which aligns and answers;
// "auto" lets the controller choose the end whose detectors click less, so the fewer clicks cross the network
type Link struct {
	A         string `json:"a"`
	B         string `json:"b"`
	Announcer string `json:"announcer"`
	Active    bool   `json:"active"`
}

const (
	announcerAuto = "auto"
	// an automatic link changes announcer only once the other end clicks this much less: a change costs the
	// block in flight, so rates that hover near each other must not make it flap
	autoSwitchRatio = 0.8
)

// Desired is the whole network as the operator wants it, the only state kept on disk
type Desired struct {
	Revision int64           `json:"revision"`
	Settings Settings        `json:"settings"`
	Nodes    map[string]Node `json:"nodes"`
	Links    map[string]Link `json:"links"`
}

// NodeConfig is what one node's agent applies: the answer to its heartbeat
type NodeConfig struct {
	Managed      bool     `json:"managed"`
	Active       bool     `json:"active"`
	Site         string   `json:"site,omitempty"`
	AnnounceTo   []string `json:"announce_to"`
	SegmentS     float64  `json:"segment_s,omitempty"`
	BlockS       float64  `json:"block_s,omitempty"`
	BlockAuto    bool     `json:"block_auto"`
	ExpectedQBER float64  `json:"expected_qber,omitempty"`
	KeepBlocks   int      `json:"keep_blocks,omitempty"`
}

// Observed is the last heartbeat of one node, as its agent sent it
type Observed struct {
	LastSeen time.Time       `json:"last_seen"`
	Status   json.RawMessage `json:"status"`
}

var (
	defaultSettings = Settings{SegmentS: 5, BlockS: 900, BlockAuto: true, ExpectedQBER: 0.05, KeepBlocks: 2}
	validID         = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

func (s Settings) validate() error {
	if s.SegmentS < 1 || s.SegmentS > 600 || s.BlockS < 30 || s.BlockS > 86400 || math.Mod(s.BlockS, s.SegmentS) != 0 {
		return errors.New("segments of 1-600 s, blocks of 30 s to a day and a whole number of segments")
	}
	if s.ExpectedQBER <= 0 || s.ExpectedQBER >= 0.5 || s.KeepBlocks < 2 || s.KeepBlocks > 100 {
		return errors.New("expected_qber between 0 and 0.5, keep_blocks 2-100")
	}
	return nil
}

// store holds the desired network on disk and the observed one in memory, and tells subscribers of every change
type store struct {
	mu       sync.Mutex
	path     string
	desired  Desired
	observed map[string]Observed
	chosen   map[string]string // each automatic link's announcer as last chosen, in memory
	subs     map[chan struct{}]struct{}
}

func openStore(path string) (*store, error) {
	s := &store{path: path, observed: map[string]Observed{}, chosen: map[string]string{}, subs: map[chan struct{}]struct{}{},
		desired: Desired{Settings: defaultSettings, Nodes: map[string]Node{}, Links: map[string]Link{}}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, s.saveLocked()
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.desired); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if s.desired.Nodes == nil {
		s.desired.Nodes = map[string]Node{}
	}
	if s.desired.Links == nil {
		s.desired.Links = map[string]Link{}
	}
	return s, nil
}

// saveLocked writes the desired state atomically; caller holds mu
func (s *store) saveLocked() error {
	data, err := json.MarshalIndent(s.desired, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *store) notifyLocked() {
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default: // a subscriber still busy with the last change gets the next snapshot anyway
		}
	}
}

// change applies fn to a copy of the desired state and keeps it only if fn and the save succeed
func (s *store) change(fn func(d *Desired) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	before, _ := json.Marshal(s.desired)
	if err := fn(&s.desired); err != nil {
		json.Unmarshal(before, &s.desired)
		return err
	}
	s.desired.Revision++
	if err := s.saveLocked(); err != nil {
		json.Unmarshal(before, &s.desired)
		return err
	}
	s.notifyLocked()
	return nil
}

// heartbeat records one node's status and returns the config it must apply
func (s *store) heartbeat(id string, status json.RawMessage, now time.Time) (NodeConfig, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observed[id] = Observed{LastSeen: now, Status: status}
	s.notifyLocked()
	return s.desired.configFor(id, s.announcersLocked()), s.desired.Revision
}

// clickRate is a node's clicks per second over its four detectors as its last heartbeat reported them, while
// it records; false when unknown
func clickRate(status json.RawMessage) (float64, bool) {
	var s struct {
		Recording bool      `json:"recording"`
		Singles   []float64 `json:"singles"`
	}
	if json.Unmarshal(status, &s) != nil || !s.Recording {
		return 0, false
	}
	total := 0.0
	for _, v := range s.Singles {
		total += v
	}
	return total, total > 0
}

// announcersLocked is every link's announcer: as set, or for an automatic link the end that clicks less,
// kept until the other clicks autoSwitchRatio as much, and its first end while either rate is unknown; mu held
func (s *store) announcersLocked() map[string]string {
	out := map[string]string{}
	for name, l := range s.desired.Links {
		if l.Announcer != announcerAuto {
			out[name] = l.Announcer
			continue
		}
		cur := s.chosen[name]
		if cur != l.A && cur != l.B {
			cur = ""
		}
		ra, okA := clickRate(s.observed[l.A].Status)
		rb, okB := clickRate(s.observed[l.B].Status)
		switch {
		case !okA || !okB:
			if cur == "" {
				cur = l.A
			}
		case cur == "":
			cur = l.A
			if rb < ra {
				cur = l.B
			}
		case cur == l.A && rb < autoSwitchRatio*ra:
			cur = l.B
		case cur == l.B && ra < autoSwitchRatio*rb:
			cur = l.A
		}
		s.chosen[name] = cur
		out[name] = cur
	}
	for name := range s.chosen {
		if _, ok := out[name]; !ok || s.desired.Links[name].Announcer != announcerAuto {
			delete(s.chosen, name)
		}
	}
	return out
}

func (s *store) subscribe() (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}

// configFor is the config of node id: active while enabled with an active link, announcing on the links it
// announces to an enabled neighbor (announcers: each link's, as announcersLocked chose them; a link missing
// there announces as set, an automatic one from its first end); a node the operator never added is not managed
func (d *Desired) configFor(id string, announcers map[string]string) NodeConfig {
	n, ok := d.Nodes[id]
	if !ok {
		return NodeConfig{AnnounceTo: []string{}}
	}
	c := NodeConfig{Managed: true, Site: n.Site, AnnounceTo: []string{}, SegmentS: d.Settings.SegmentS, BlockS: d.Settings.BlockS,
		BlockAuto: d.Settings.BlockAuto, ExpectedQBER: d.Settings.ExpectedQBER, KeepBlocks: d.Settings.KeepBlocks}
	if !n.Enabled {
		return c
	}
	for name, l := range d.Links {
		other := l.B
		if l.B == id {
			other = l.A
		} else if l.A != id {
			continue
		}
		if !l.Active || !d.Nodes[other].Enabled {
			continue
		}
		c.Active = true
		if d.announcer(name, l, announcers) == id {
			c.AnnounceTo = append(c.AnnounceTo, other)
		}
	}
	sort.Strings(c.AnnounceTo)
	return c
}

func (d *Desired) announcer(name string, l Link, announcers map[string]string) string {
	if a, ok := announcers[name]; ok {
		return a
	}
	if l.Announcer == announcerAuto {
		return l.A
	}
	return l.Announcer
}

func (d *Desired) putNode(id string, n Node) error {
	if !validID.MatchString(id) {
		return errors.New("node IDs use letters, digits, - and _ only")
	}
	if n.Site == "" {
		return errors.New("site is required: the node's site in its acquisition config")
	}
	d.Nodes[id] = n
	return nil
}

func (d *Desired) deleteNode(id string) error {
	if _, ok := d.Nodes[id]; !ok {
		return errNotFound
	}
	for name, l := range d.Links {
		if l.A == id || l.B == id {
			return fmt.Errorf("link %s still uses node %s", name, id)
		}
	}
	delete(d.Nodes, id)
	return nil
}

func (d *Desired) putLink(name string, l Link) error {
	if !validID.MatchString(name) {
		return errors.New("link names use letters, digits, - and _ only")
	}
	for _, id := range []string{l.A, l.B} {
		if _, ok := d.Nodes[id]; !ok {
			return fmt.Errorf("add node %q first", id)
		}
	}
	if l.A == l.B {
		return errors.New("a link joins two different nodes")
	}
	if l.Announcer != l.A && l.Announcer != l.B && l.Announcer != announcerAuto {
		return errors.New("the announcer is one of the link's two nodes, or auto")
	}
	for other, o := range d.Links {
		if other != name && ((o.A == l.A && o.B == l.B) || (o.A == l.B && o.B == l.A)) {
			return fmt.Errorf("link %s already joins these nodes", other)
		}
	}
	d.Links[name] = l
	return nil
}

var errNotFound = errors.New("not found")

// current is a copy of the desired state
func (s *store) current() Desired {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, _ := json.Marshal(s.desired)
	var d Desired
	json.Unmarshal(data, &d)
	return d
}
