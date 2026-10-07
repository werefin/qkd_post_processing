package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// writeNpyV1 builds a real .npy v1.0 file byte-for-byte per numpy's documented
// format: magic, version, 2-byte header length, header dict padded to a
// 16-byte boundary with spaces and a trailing newline, then raw data
func writeNpyV1(t *testing.T, path, descr string, shape []int, data []byte) {
	t.Helper()
	shapeStr := ""
	for i, s := range shape {
		if i > 0 {
			shapeStr += ", "
		}
		shapeStr += itoa(s)
	}
	if len(shape) == 1 {
		shapeStr += ","
	}
	header := "{'descr': '" + descr + "', 'fortran_order': False, 'shape': (" + shapeStr + "), }"
	// pad so magic(6)+version(2)+lenfield(2)+header+\n is a multiple of 16
	total := 6 + 2 + 2 + len(header) + 1
	pad := (16 - total%16) % 16
	for i := 0; i < pad; i++ {
		header += " "
	}
	header += "\n"

	var buf []byte
	buf = append(buf, "\x93NUMPY"...)
	buf = append(buf, 1, 0) // version 1.0
	lenBytes := make([]byte, 2)
	binary.LittleEndian.PutUint16(lenBytes, uint16(len(header)))
	buf = append(buf, lenBytes...)
	buf = append(buf, header...)
	buf = append(buf, data...)

	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeNpyV2 is the same but with a 2.0 header (4-byte length field),
// exercising the other branch of readNpyHeader
func writeNpyV2(t *testing.T, path, descr string, shape []int, data []byte) {
	t.Helper()
	shapeStr := ""
	for i, s := range shape {
		if i > 0 {
			shapeStr += ", "
		}
		shapeStr += itoa(s)
	}
	if len(shape) == 1 {
		shapeStr += ","
	}
	header := "{'descr': '" + descr + "', 'fortran_order': False, 'shape': (" + shapeStr + "), }"
	total := 6 + 2 + 4 + len(header) + 1
	pad := (16 - total%16) % 16
	for i := 0; i < pad; i++ {
		header += " "
	}
	header += "\n"

	var buf []byte
	buf = append(buf, "\x93NUMPY"...)
	buf = append(buf, 2, 0) // version 2.0
	lenBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(lenBytes, uint32(len(header)))
	buf = append(buf, lenBytes...)
	buf = append(buf, header...)
	buf = append(buf, data...)

	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}

// TestLoadNpyInt64RoundTrip checks a hand-built real v1.0 int64 file decodes
// to exactly the values encoded, including negative numbers and large
// magnitudes at the actual recorded-timestamp scale
func TestLoadNpyInt64RoundTrip(t *testing.T) {
	want := []int64{0, 1, -1, 4607167884484608000, -4607167884484608000, 9223372036854775807, -9223372036854775808}
	data := make([]byte, len(want)*8)
	for i, v := range want {
		binary.LittleEndian.PutUint64(data[i*8:], uint64(v))
	}
	path := filepath.Join(t.TempDir(), "ts.npy")
	writeNpyV1(t, path, "<i8", []int{len(want)}, data)

	got, err := loadNpyInt64(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d values, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d: got %d, want %d", i, got[i], want[i])
		}
	}
}

// TestLoadNpyInt64CrossesChunkBoundary: real recordings are tens of millions
// of elements, far past npyReadChunkElems (64K) --> this checks values on
// both sides of a chunk boundary decode correctly, not just small arrays
func TestLoadNpyInt64CrossesChunkBoundary(t *testing.T) {
	n := npyReadChunkElems*2 + 137 // deliberately not a multiple of the chunk size
	want := make([]int64, n)
	data := make([]byte, n*8)
	for i := range want {
		want[i] = int64(i)*1000 - 12345 // distinct, includes negatives early on
		binary.LittleEndian.PutUint64(data[i*8:], uint64(want[i]))
	}
	path := filepath.Join(t.TempDir(), "ts.npy")
	writeNpyV1(t, path, "<i8", []int{n}, data)

	got, err := loadNpyInt64(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != n {
		t.Fatalf("got %d values, want %d", len(got), n)
	}
	// check every chunk boundary +/- a few elements, not just the first/last
	checkpoints := []int{0, 1, npyReadChunkElems - 1, npyReadChunkElems, npyReadChunkElems + 1, 2*npyReadChunkElems - 1, 2 * npyReadChunkElems, n - 1}
	for _, i := range checkpoints {
		if got[i] != want[i] {
			t.Fatalf("index %d: got %d, want %d", i, got[i], want[i])
		}
	}
}

// TestLoadNpyInt8CrossesChunkBoundary: same crossing check as int64, since
// loadNpyInt8 reuses a separately-sized scratch buffer of its own
func TestLoadNpyInt8CrossesChunkBoundary(t *testing.T) {
	n := npyReadChunkElems*2 + 137
	want := make([]int8, n)
	data := make([]byte, n)
	for i := range want {
		want[i] = int8(1 + i%4) // cycle through valid channels 1-4
		data[i] = byte(want[i])
	}
	path := filepath.Join(t.TempDir(), "ch.npy")
	writeNpyV1(t, path, "|i1", []int{n}, data)

	got, err := loadNpyInt8(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != n {
		t.Fatalf("got %d values, want %d", len(got), n)
	}
	checkpoints := []int{0, 1, npyReadChunkElems - 1, npyReadChunkElems, npyReadChunkElems + 1, 2*npyReadChunkElems - 1, 2 * npyReadChunkElems, n - 1}
	for _, i := range checkpoints {
		if got[i] != want[i] {
			t.Fatalf("index %d: got %d, want %d", i, got[i], want[i])
		}
	}
}

// TestLoadNpyInt8RoundTrip checks the full int8 range, including values
// that are negative when interpreted as signed but would be large as
// unsigned bytes, since channel bytes are read raw off disk
func TestLoadNpyInt8RoundTrip(t *testing.T) {
	want := []int8{0, 1, 2, 3, 4, -1, 127, -128}
	data := make([]byte, len(want))
	for i, v := range want {
		data[i] = byte(v)
	}
	path := filepath.Join(t.TempDir(), "ch.npy")
	writeNpyV1(t, path, "|i1", []int{len(want)}, data)

	got, err := loadNpyInt8(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d values, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d: got %d, want %d", i, got[i], want[i])
		}
	}
}

// TestLoadNpyInt8AcceptsAllDocumentedByteOrderVariants: a single signed
// byte has no endianness, numpy writes it as '|i1', '<i1', or '>i1'
// depending on platform/version --> all three must be accepted identically
func TestLoadNpyInt8AcceptsAllDocumentedByteOrderVariants(t *testing.T) {
	for _, descr := range []string{"|i1", "<i1", ">i1"} {
		path := filepath.Join(t.TempDir(), "ch.npy")
		writeNpyV1(t, path, descr, []int{3}, []byte{1, 2, 3})
		got, err := loadNpyInt8(path)
		if err != nil {
			t.Fatalf("descr=%q: unexpected error: %v", descr, err)
		}
		if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
			t.Fatalf("descr=%q: got %v, want [1 2 3]", descr, got)
		}
	}
}

// TestLoadNpyRejectsWrongDtype checks a timestamp array mistakenly written
// as int32 (or any dtype other than exactly '<i8') is rejected rather than
// silently misread with the wrong element width
func TestLoadNpyRejectsWrongDtype(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ts.npy")
	writeNpyV1(t, path, "<i4", []int{2}, []byte{1, 0, 0, 0, 2, 0, 0, 0})
	if _, err := loadNpyInt64(path); err == nil {
		t.Fatal("expected an error for a non-'<i8' dtype, got nil")
	}
}

// TestLoadNpyRejectsFortranOrder checks a fortran-ordered array (which this
// reader explicitly doesn't support) is rejected, not silently misread as
// if it were row-major
func TestLoadNpyRejectsFortranOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ts.npy")
	header := "{'descr': '<i8', 'fortran_order': True, 'shape': (1,), }"
	total := 6 + 2 + 2 + len(header) + 1
	pad := (16 - total%16) % 16
	for i := 0; i < pad; i++ {
		header += " "
	}
	header += "\n"
	var buf []byte
	buf = append(buf, "\x93NUMPY"...)
	buf = append(buf, 1, 0)
	lenBytes := make([]byte, 2)
	binary.LittleEndian.PutUint16(lenBytes, uint16(len(header)))
	buf = append(buf, lenBytes...)
	buf = append(buf, header...)
	buf = append(buf, 0, 0, 0, 0, 0, 0, 0, 0)
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadNpyInt64(path); err == nil {
		t.Fatal("expected an error for fortran_order=True, got nil")
	}
}

// TestLoadNpyRejectsMultiDimensionalShape checks a 2-D array is rejected,
// since the reader flattens assuming exactly one dimension
func TestLoadNpyRejectsMultiDimensionalShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ts.npy")
	writeNpyV1(t, path, "<i8", []int{2, 3}, make([]byte, 48))
	if _, err := loadNpyInt64(path); err == nil {
		t.Fatal("expected an error for a 2-D shape, got nil")
	}
}

// TestLoadNpyRejectsBadMagic checks a file that isn't a .npy file at all
// (wrong magic bytes) is rejected cleanly rather than parsed as garbage
func TestLoadNpyRejectsBadMagic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notnpy.npy")
	if err := os.WriteFile(path, []byte("not a numpy file at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadNpyInt64(path); err == nil {
		t.Fatal("expected an error for bad magic bytes, got nil")
	}
}

// TestLoadNpyInt64AcceptsVersion2Header exercises the 4-byte header-length
// branch of readNpyHeader, used for .npy files with headers too long for
// the v1.0 2-byte length field (large shape/dtype metadata)
func TestLoadNpyInt64AcceptsVersion2Header(t *testing.T) {
	want := []int64{10, 20, 30}
	data := make([]byte, len(want)*8)
	for i, v := range want {
		binary.LittleEndian.PutUint64(data[i*8:], uint64(v))
	}
	path := filepath.Join(t.TempDir(), "ts.npy")
	writeNpyV2(t, path, "<i8", []int{len(want)}, data)

	got, err := loadNpyInt64(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d: got %d, want %d", i, got[i], want[i])
		}
	}
}

// TestLoadNpyEmptyArray checks the zero-length edge case doesn't panic or
// misbehave (a recording side with zero clicks is a real, if degenerate, case)
func TestLoadNpyEmptyArray(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ts.npy")
	writeNpyV1(t, path, "<i8", []int{0}, nil)
	got, err := loadNpyInt64(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 values, got %d", len(got))
	}
}
