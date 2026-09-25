package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// npyHeader is the subset of a .npy header this reader needs: the dtype
// descriptor, array shape, and memory layout. Sufficient for the flat
// int8/int64 arrays tt_record_dual.py writes, not a general npy reader
type npyHeader struct {
	descr        string
	shape        []int
	fortranOrder bool
}

var (
	npyDescrRe   = regexp.MustCompile(`'descr':\s*'([^']+)'`)
	npyShapeRe   = regexp.MustCompile(`'shape':\s*\(([^)]*)\)`)
	npyFortranRe = regexp.MustCompile(`'fortran_order':\s*(True|False)`)
)

// readNpyHeader parses the magic string, version, and header dict that
// precede a .npy file's raw array data (numpy's documented format)
func readNpyHeader(f *os.File) (npyHeader, error) {
	magic := make([]byte, 6)
	if _, err := io.ReadFull(f, magic); err != nil {
		return npyHeader{}, err
	}
	if string(magic) != "\x93NUMPY" {
		return npyHeader{}, fmt.Errorf("not an npy file (bad magic)")
	}

	version := make([]byte, 2)
	if _, err := io.ReadFull(f, version); err != nil {
		return npyHeader{}, err
	}

	var headerLen int
	if version[0] == 1 {
		lenBytes := make([]byte, 2)
		if _, err := io.ReadFull(f, lenBytes); err != nil {
			return npyHeader{}, err
		}
		headerLen = int(binary.LittleEndian.Uint16(lenBytes))
	} else {
		lenBytes := make([]byte, 4)
		if _, err := io.ReadFull(f, lenBytes); err != nil {
			return npyHeader{}, err
		}
		headerLen = int(binary.LittleEndian.Uint32(lenBytes))
	}

	headerBytes := make([]byte, headerLen)
	if _, err := io.ReadFull(f, headerBytes); err != nil {
		return npyHeader{}, err
	}
	headerStr := string(headerBytes)

	descrMatch := npyDescrRe.FindStringSubmatch(headerStr)
	if descrMatch == nil {
		return npyHeader{}, fmt.Errorf("npy header missing 'descr'")
	}
	shapeMatch := npyShapeRe.FindStringSubmatch(headerStr)
	if shapeMatch == nil {
		return npyHeader{}, fmt.Errorf("npy header missing 'shape'")
	}
	fortranMatch := npyFortranRe.FindStringSubmatch(headerStr)

	var shape []int
	for _, part := range strings.Split(shapeMatch[1], ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return npyHeader{}, fmt.Errorf("npy header: bad shape entry %q: %w", part, err)
		}
		if n < 0 {
			return npyHeader{}, fmt.Errorf("npy header: negative shape entry %d", n)
		}
		shape = append(shape, n)
	}

	return npyHeader{
		descr:        descrMatch[1],
		shape:        shape,
		fortranOrder: fortranMatch != nil && fortranMatch[1] == "True",
	}, nil
}

// loadNpyInt64 loads a 1-D little-endian int64 array (numpy dtype '<i8'),
// the format tt_record_dual.py uses for *_timestamp_sequence.npy
func loadNpyInt64(path string) ([]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	hdr, err := readNpyHeader(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if hdr.descr != "<i8" {
		return nil, fmt.Errorf("%s: expected dtype '<i8', got %q", path, hdr.descr)
	}
	if hdr.fortranOrder {
		return nil, fmt.Errorf("%s: fortran-ordered arrays are not supported", path)
	}
	if len(hdr.shape) != 1 {
		return nil, fmt.Errorf("%s: expected a 1-D array, got shape %v", path, hdr.shape)
	}

	n := hdr.shape[0]
	raw := make([]byte, n*8)
	if _, err := io.ReadFull(f, raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	out := make([]int64, n)
	for i := range out {
		out[i] = int64(binary.LittleEndian.Uint64(raw[i*8:]))
	}
	return out, nil
}

// loadNpyInt8 loads a 1-D int8 array (numpy dtype '|i1'), the format
// tt_record_dual.py uses for *_channel_sequence.npy
func loadNpyInt8(path string) ([]int8, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	hdr, err := readNpyHeader(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if hdr.descr != "|i1" && hdr.descr != "<i1" && hdr.descr != ">i1" {
		return nil, fmt.Errorf("%s: expected an int8 dtype, got %q", path, hdr.descr)
	}
	if hdr.fortranOrder {
		return nil, fmt.Errorf("%s: fortran-ordered arrays are not supported", path)
	}
	if len(hdr.shape) != 1 {
		return nil, fmt.Errorf("%s: expected a 1-D array, got shape %v", path, hdr.shape)
	}

	n := hdr.shape[0]
	raw := make([]byte, n)
	if _, err := io.ReadFull(f, raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	out := make([]int8, n)
	for i, b := range raw {
		out[i] = int8(b)
	}
	return out, nil
}
