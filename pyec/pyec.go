// Package pyec runs ec/ldpc5g.py as a pool of service processes and implements
// qkd.LDPC5GBackend over them: the matrix work of 5G NR LDPC, done by py3gpp and ldpc
package pyec

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
)

type worker struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

// Pool is safe for concurrent use: each call takes one idle process, and a new one starts only when
// every process is busy, up to the limit, since each costs about half a second of imports to start
type Pool struct {
	idle           chan *worker
	mu             sync.Mutex
	all            []*worker
	limit          int
	python, script string
}

// Script finds ec/ldpc5g.py: QKD_EC_SCRIPT, else next to or above the running binary, else /app/ec
func Script() (string, error) {
	if s := os.Getenv("QKD_EC_SCRIPT"); s != "" {
		return s, nil
	}
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		dirs = append(dirs, d, filepath.Dir(d), filepath.Dir(filepath.Dir(d)))
	}
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd, filepath.Dir(wd), filepath.Dir(filepath.Dir(wd)))
	}
	dirs = append(dirs, "/app")
	for _, d := range dirs {
		p := filepath.Join(d, "ec", "ldpc5g.py")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("pyec: ec/ldpc5g.py not found; set QKD_EC_SCRIPT")
}

// Start prepares up to n service processes (0 = one per core) with QKD_EC_PYTHON, default python3,
// starting one now; the rest start as concurrent calls need them
func Start(n int) (*Pool, error) {
	script, err := Script()
	if err != nil {
		return nil, err
	}
	python := os.Getenv("QKD_EC_PYTHON")
	if python == "" {
		python = "python3"
	}
	if n <= 0 {
		n = runtime.NumCPU()
	}
	p := &Pool{idle: make(chan *worker, n), limit: n, python: python, script: script}
	w, err := p.spawn()
	if err != nil {
		return nil, err
	}
	p.idle <- w
	// a first request proves the libraries import, instead of failing in the middle of a run
	if _, err := p.call(map[string]any{"op": "shape", "m": 4}); err != nil {
		p.Close()
		return nil, fmt.Errorf("pyec: %v (are ldpc and py3gpp installed? see ec/requirements.txt)", err)
	}
	return p, nil
}

func (p *Pool) spawn() (*worker, error) {
	cmd := exec.Command(p.python, p.script, "serve")
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("pyec: start %s: %w", p.python, err)
	}
	w := &worker{cmd: cmd, in: in, out: bufio.NewReaderSize(out, 1<<20)}
	p.mu.Lock()
	p.all = append(p.all, w)
	p.mu.Unlock()
	return w, nil
}

// acquire takes an idle process, starts another if all are busy and the limit allows, else waits
func (p *Pool) acquire() (*worker, error) {
	select {
	case w := <-p.idle:
		return w, nil
	default:
	}
	p.mu.Lock()
	grow := len(p.all) < p.limit
	if grow {
		p.limit-- // reserve the slot before starting, so concurrent callers never overshoot
	}
	p.mu.Unlock()
	if grow {
		w, err := p.spawn()
		p.mu.Lock()
		p.limit++
		p.mu.Unlock()
		if err == nil {
			return w, nil
		}
	}
	return <-p.idle, nil
}

// Close ends every service process
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, w := range p.all {
		w.in.Close()
		w.cmd.Wait()
	}
}

func (p *Pool) call(req map[string]any) (map[string]any, error) {
	w, err := p.acquire()
	if err != nil {
		return nil, err
	}
	defer func() { p.idle <- w }()
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if err := binary.Write(w.in, binary.BigEndian, uint32(len(body))); err != nil {
		return nil, fmt.Errorf("pyec: service gone: %w", err)
	}
	if _, err := w.in.Write(body); err != nil {
		return nil, fmt.Errorf("pyec: service gone: %w", err)
	}
	var n uint32
	if err := binary.Read(w.out, binary.BigEndian, &n); err != nil {
		return nil, fmt.Errorf("pyec: service gone: %w", err)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(w.out, buf); err != nil {
		return nil, err
	}
	var resp map[string]any
	if err := json.Unmarshal(buf, &resp); err != nil {
		return nil, err
	}
	if e, ok := resp["error"].(string); ok {
		return nil, errors.New("pyec: " + e)
	}
	return resp, nil
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func unb64(v any) ([]byte, error) {
	s, ok := v.(string)
	if !ok {
		return nil, errors.New("pyec: missing array in answer")
	}
	return base64.StdEncoding.DecodeString(s)
}

// Syndrome is H x over GF(2)
func (p *Pool) Syndrome(m, z int, x []byte) ([]byte, error) {
	r, err := p.call(map[string]any{"op": "syndrome", "m": m, "z": z, "x": b64(x)})
	if err != nil {
		return nil, err
	}
	return unb64(r["s"])
}

// Rank is the GF(2) rank of the listed columns of H
func (p *Pool) Rank(m, z int, cols []int) (int, error) {
	r, err := p.call(map[string]any{"op": "rank", "m": m, "z": z, "cols": cols})
	if err != nil {
		return 0, err
	}
	f, ok := r["rank"].(float64)
	if !ok {
		return 0, errors.New("pyec: missing rank in answer")
	}
	return int(f), nil
}

// Decode is belief propagation (exact sum-product, serial schedule) toward syndrome s
func (p *Pool) Decode(m, z int, y []byte, probs []float64, s []byte, maxIter int) ([]byte, bool, error) {
	pb := make([]byte, 8*len(probs))
	for i, v := range probs {
		binary.LittleEndian.PutUint64(pb[8*i:], math.Float64bits(v))
	}
	r, err := p.call(map[string]any{"op": "decode", "m": m, "z": z, "y": b64(y), "probs": b64(pb), "s": b64(s), "max_iter": maxIter})
	if err != nil {
		return nil, false, err
	}
	x, err := unb64(r["x"])
	if err != nil {
		return nil, false, err
	}
	conv, _ := r["converged"].(bool)
	return x, conv, nil
}
