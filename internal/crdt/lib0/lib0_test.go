package lib0_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/mesutokul/ycollab/internal/crdt/lib0"
)

// Golden vectors produced by lib0 itself; see tools/fixturegen/lib0.mjs.
const vectorsPath = "../../../testdata/fixtures/lib0/vectors.json"

type vectors struct {
	VarUint []struct {
		Value json.Number `json:"value"`
		Hex   string      `json:"hex"`
	} `json:"varUint"`
	VarInt []struct {
		Value      json.Number `json:"value"`
		Hex        string      `json:"hex"`
		DecodeOnly bool        `json:"decodeOnly"`
		Note       string      `json:"note"`
	} `json:"varInt"`
	VarString []struct {
		Value       string `json:"value"`
		UTF16Length int    `json:"utf16Length"`
		ByteLength  int    `json:"byteLength"`
		Hex         string `json:"hex"`
	} `json:"varString"`
	VarUint8Array []struct {
		Bytes []int  `json:"bytes"`
		Hex   string `json:"hex"`
	} `json:"varUint8Array"`
	Invalid []struct {
		Kind   string `json:"kind"`
		Hex    string `json:"hex"`
		Reason string `json:"reason"`
	} `json:"invalid"`
	GoStricter []struct {
		Kind   string `json:"kind"`
		Hex    string `json:"hex"`
		Reason string `json:"reason"`
	} `json:"goStricter"`
}

func loadVectors(t *testing.T) *vectors {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(vectorsPath))
	if err != nil {
		t.Fatalf("read golden vectors: %v (run `npm run generate` in tools/fixturegen)", err)
	}
	var v vectors
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("parse golden vectors: %v", err)
	}
	return &v
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q in fixture: %v", s, err)
	}
	return b
}

func TestVarUintGolden(t *testing.T) {
	v := loadVectors(t)
	if len(v.VarUint) == 0 {
		t.Fatal("no varUint vectors")
	}
	for _, tc := range v.VarUint {
		want, err := strconv.ParseUint(tc.Value.String(), 10, 64)
		if err != nil {
			t.Fatalf("bad fixture value %q: %v", tc.Value, err)
		}
		wantBytes := mustHex(t, tc.Hex)

		e := lib0.NewEncoder()
		e.WriteVarUint(want)
		if err := e.Err(); err != nil {
			t.Errorf("WriteVarUint(%d): unexpected error %v", want, err)
			continue
		}
		if got := e.Bytes(); !bytes.Equal(got, wantBytes) {
			t.Errorf("WriteVarUint(%d) = %x, want %x", want, got, wantBytes)
		}

		d := lib0.NewDecoder(wantBytes)
		got, err := d.ReadVarUint()
		if err != nil {
			t.Errorf("ReadVarUint(%x): %v", wantBytes, err)
			continue
		}
		if got != want {
			t.Errorf("ReadVarUint(%x) = %d, want %d", wantBytes, got, want)
		}
		if !d.Done() {
			t.Errorf("ReadVarUint(%x) left %d bytes unread", wantBytes, d.Remaining())
		}
	}
}

func TestVarIntGolden(t *testing.T) {
	v := loadVectors(t)
	if len(v.VarInt) == 0 {
		t.Fatal("no varInt vectors")
	}
	for _, tc := range v.VarInt {
		want, err := strconv.ParseInt(tc.Value.String(), 10, 64)
		if err != nil {
			t.Fatalf("bad fixture value %q: %v", tc.Value, err)
		}
		wantBytes := mustHex(t, tc.Hex)

		if !tc.DecodeOnly {
			e := lib0.NewEncoder()
			e.WriteVarInt(want)
			if err := e.Err(); err != nil {
				t.Errorf("WriteVarInt(%d): unexpected error %v", want, err)
				continue
			}
			if got := e.Bytes(); !bytes.Equal(got, wantBytes) {
				t.Errorf("WriteVarInt(%d) = %x, want %x", want, got, wantBytes)
			}
		}

		d := lib0.NewDecoder(wantBytes)
		got, err := d.ReadVarInt()
		if err != nil {
			t.Errorf("ReadVarInt(%x): %v", wantBytes, err)
			continue
		}
		if got != want {
			t.Errorf("ReadVarInt(%x) = %d, want %d (%s)", wantBytes, got, want, tc.Note)
		}
		if !d.Done() {
			t.Errorf("ReadVarInt(%x) left %d bytes unread", wantBytes, d.Remaining())
		}
	}
}

// The sign bit lives at 0x40 of the first byte only, and the first byte carries
// six value bits while every later byte carries seven. Spelled out as explicit
// bytes so a refactor that reintroduces zigzag encoding fails loudly.
func TestVarIntBitLayout(t *testing.T) {
	cases := []struct {
		value int64
		want  []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{-1, []byte{0x41}},
		{63, []byte{0x3f}},
		{-63, []byte{0x7f}},
		{64, []byte{0x80, 0x01}},  // 6 value bits exhausted, continue
		{-64, []byte{0xc0, 0x01}}, // same, sign bit set
		{8191, []byte{0xbf, 0x7f}},
		{8192, []byte{0x80, 0x80, 0x01}},
	}
	for _, tc := range cases {
		e := lib0.NewEncoder()
		e.WriteVarInt(tc.value)
		if got := e.Bytes(); !bytes.Equal(got, tc.want) {
			t.Errorf("WriteVarInt(%d) = % x, want % x", tc.value, got, tc.want)
		}
		got, err := lib0.NewDecoder(tc.want).ReadVarInt()
		if err != nil || got != tc.value {
			t.Errorf("ReadVarInt(% x) = %d, %v; want %d, nil", tc.want, got, err, tc.value)
		}
	}
}

func TestVarStringGolden(t *testing.T) {
	v := loadVectors(t)
	if len(v.VarString) == 0 {
		t.Fatal("no varString vectors")
	}
	for _, tc := range v.VarString {
		wantBytes := mustHex(t, tc.Hex)

		e := lib0.NewEncoder()
		e.WriteVarString(tc.Value)
		if err := e.Err(); err != nil {
			t.Errorf("WriteVarString(%q): unexpected error %v", tc.Value, err)
			continue
		}
		if got := e.Bytes(); !bytes.Equal(got, wantBytes) {
			t.Errorf("WriteVarString(%q) = %x, want %x", tc.Value, got, wantBytes)
		}

		d := lib0.NewDecoder(wantBytes)
		got, err := d.ReadVarString()
		if err != nil {
			t.Errorf("ReadVarString(%x): %v", wantBytes, err)
			continue
		}
		if got != tc.Value {
			t.Errorf("ReadVarString(%x) = %q, want %q", wantBytes, got, tc.Value)
		}
		if !d.Done() {
			t.Errorf("ReadVarString(%q) left %d bytes unread", tc.Value, d.Remaining())
		}

		// The length prefix counts UTF-8 bytes...
		if len(tc.Value) != tc.ByteLength {
			t.Errorf("len(%q) = %d, fixture says %d", tc.Value, len(tc.Value), tc.ByteLength)
		}
		// ...while Yjs struct lengths count UTF-16 code units.
		if got := lib0.UTF16Length(tc.Value); got != tc.UTF16Length {
			t.Errorf("UTF16Length(%q) = %d, want %d", tc.Value, got, tc.UTF16Length)
		}
	}
}

func TestVarUint8ArrayGolden(t *testing.T) {
	v := loadVectors(t)
	if len(v.VarUint8Array) == 0 {
		t.Fatal("no varUint8Array vectors")
	}
	for _, tc := range v.VarUint8Array {
		payload := make([]byte, len(tc.Bytes))
		for i, b := range tc.Bytes {
			payload[i] = byte(b)
		}
		wantBytes := mustHex(t, tc.Hex)

		e := lib0.NewEncoder()
		e.WriteVarUint8Array(payload)
		if err := e.Err(); err != nil {
			t.Errorf("WriteVarUint8Array(len %d): unexpected error %v", len(payload), err)
			continue
		}
		if got := e.Bytes(); !bytes.Equal(got, wantBytes) {
			t.Errorf("WriteVarUint8Array(len %d) = %x, want %x", len(payload), got, wantBytes)
		}

		d := lib0.NewDecoder(wantBytes)
		got, err := d.ReadVarUint8Array()
		if err != nil {
			t.Errorf("ReadVarUint8Array(%x): %v", wantBytes, err)
			continue
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("ReadVarUint8Array = %x, want %x", got, payload)
		}
		if !d.Done() {
			t.Errorf("ReadVarUint8Array left %d bytes unread", d.Remaining())
		}
	}
}

func readByKind(kind string, b []byte) error {
	d := lib0.NewDecoder(b)
	var err error
	switch kind {
	case "varUint":
		_, err = d.ReadVarUint()
	case "varInt":
		_, err = d.ReadVarInt()
	case "varString":
		_, err = d.ReadVarString()
	case "varUint8Array":
		_, err = d.ReadVarUint8Array()
	default:
		return errors.New("unknown kind " + kind)
	}
	return err
}

// Input lib0 itself rejects.
func TestInvalidInputRejected(t *testing.T) {
	v := loadVectors(t)
	if len(v.Invalid) == 0 {
		t.Fatal("no invalid vectors")
	}
	for _, tc := range v.Invalid {
		if err := readByKind(tc.Kind, mustHex(t, tc.Hex)); err == nil {
			t.Errorf("%s(%q) succeeded, want error (%s)", tc.Kind, tc.Hex, tc.Reason)
		}
	}
}

// Input lib0 mis-reads instead of rejecting; we are deliberately stricter.
func TestGoStricterThanLib0(t *testing.T) {
	v := loadVectors(t)
	if len(v.GoStricter) == 0 {
		t.Fatal("no goStricter vectors")
	}
	for _, tc := range v.GoStricter {
		err := readByKind(tc.Kind, mustHex(t, tc.Hex))
		if !errors.Is(err, lib0.ErrUnexpectedEOF) {
			t.Errorf("%s(%q) = %v, want ErrUnexpectedEOF (%s)", tc.Kind, tc.Hex, err, tc.Reason)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	uints := []uint64{0, 1, 63, 64, 127, 128, 16383, 16384, 1 << 20, 1 << 27, 1 << 28, 1<<32 - 1, 1 << 32, lib0.MaxSafeInteger}
	ints := []int64{0, 1, -1, 63, -63, 64, -64, 1 << 20, -(1 << 20), 1 << 40, -(1 << 40), lib0.MaxSafeInteger, -lib0.MaxSafeInteger}
	strs := []string{"", "a", "text", "🎉 mixed ünï 日本", string(make([]byte, 0))}
	blobs := [][]byte{{}, {0}, {0, 1, 2, 255}, bytes.Repeat([]byte{0xAB}, 500)}

	e := lib0.NewEncoder()
	for _, v := range uints {
		e.WriteVarUint(v)
	}
	for _, v := range ints {
		e.WriteVarInt(v)
	}
	for _, s := range strs {
		e.WriteVarString(s)
	}
	for _, b := range blobs {
		e.WriteVarUint8Array(b)
	}
	e.WriteUint8(0xA8)
	if err := e.Err(); err != nil {
		t.Fatalf("encode: %v", err)
	}

	d := lib0.NewDecoder(e.Bytes())
	for _, want := range uints {
		got, err := d.ReadVarUint()
		if err != nil || got != want {
			t.Fatalf("ReadVarUint = %d, %v; want %d", got, err, want)
		}
	}
	for _, want := range ints {
		got, err := d.ReadVarInt()
		if err != nil || got != want {
			t.Fatalf("ReadVarInt = %d, %v; want %d", got, err, want)
		}
	}
	for _, want := range strs {
		got, err := d.ReadVarString()
		if err != nil || got != want {
			t.Fatalf("ReadVarString = %q, %v; want %q", got, err, want)
		}
	}
	for _, want := range blobs {
		got, err := d.ReadVarUint8Array()
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("ReadVarUint8Array = %x, %v; want %x", got, err, want)
		}
	}
	info, err := d.ReadUint8()
	if err != nil || info != 0xA8 {
		t.Fatalf("ReadUint8 = %#x, %v; want 0xa8", info, err)
	}
	if !d.Done() {
		t.Fatalf("%d bytes left unread", d.Remaining())
	}
}

func TestWriteOutOfRange(t *testing.T) {
	cases := []struct {
		name  string
		write func(*lib0.Encoder)
	}{
		{"varUint above MaxSafeInteger", func(e *lib0.Encoder) { e.WriteVarUint(lib0.MaxSafeInteger + 1) }},
		{"varUint max uint64", func(e *lib0.Encoder) { e.WriteVarUint(^uint64(0)) }},
		{"varInt above MaxSafeInteger", func(e *lib0.Encoder) { e.WriteVarInt(lib0.MaxSafeInteger + 1) }},
		{"varInt below -MaxSafeInteger", func(e *lib0.Encoder) { e.WriteVarInt(-lib0.MaxSafeInteger - 1) }},
		{"varInt min int64", func(e *lib0.Encoder) { e.WriteVarInt(-1 << 63) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := lib0.NewEncoder()
			tc.write(e)
			if !errors.Is(e.Err(), lib0.ErrIntegerOutOfRange) {
				t.Fatalf("Err() = %v, want ErrIntegerOutOfRange", e.Err())
			}
			if e.Len() != 0 {
				t.Fatalf("wrote %d bytes for a rejected value: %x", e.Len(), e.Bytes())
			}
		})
	}
}

func TestEncoderErrorIsSticky(t *testing.T) {
	e := lib0.NewEncoder()
	e.WriteVarUint(7)
	before := e.Len()
	e.WriteVarUint(lib0.MaxSafeInteger + 1)
	e.WriteVarString("ignored")
	e.WriteUint8(0xFF)
	if !errors.Is(e.Err(), lib0.ErrIntegerOutOfRange) {
		t.Fatalf("Err() = %v, want ErrIntegerOutOfRange", e.Err())
	}
	if e.Len() != before {
		t.Fatalf("writes after the error changed the buffer: %x", e.Bytes())
	}
	e.Reset()
	if e.Err() != nil || e.Len() != 0 {
		t.Fatalf("Reset left err=%v len=%d", e.Err(), e.Len())
	}
}

// MaxSafeInteger decodes, one more byte of continuation does not - matching
// where lib0 places its range check.
func TestMaxSafeIntegerBoundary(t *testing.T) {
	e := lib0.NewEncoder()
	e.WriteVarUint(lib0.MaxSafeInteger)
	got, err := lib0.NewDecoder(e.Bytes()).ReadVarUint()
	if err != nil || got != lib0.MaxSafeInteger {
		t.Fatalf("ReadVarUint(MaxSafeInteger) = %d, %v", got, err)
	}

	over := append(bytes.Repeat([]byte{0xFF}, 8), 0x7F)
	if _, err := lib0.NewDecoder(over).ReadVarUint(); !errors.Is(err, lib0.ErrIntegerOutOfRange) {
		t.Fatalf("ReadVarUint(%x) = %v, want ErrIntegerOutOfRange", over, err)
	}
}

func TestDecoderDoesNotCopy(t *testing.T) {
	e := lib0.NewEncoder()
	e.WriteVarUint8Array([]byte{1, 2, 3})
	buf := e.Bytes()

	got, err := lib0.NewDecoder(buf).ReadVarUint8Array()
	if err != nil {
		t.Fatal(err)
	}
	got[0] = 9
	if buf[1] != 9 {
		t.Fatal("ReadVarUint8Array copied; the documented contract is that it aliases the input")
	}
}

func FuzzVarUintRoundTrip(f *testing.F) {
	for _, seed := range []uint64{0, 1, 127, 128, 16384, 1 << 40, lib0.MaxSafeInteger} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, v uint64) {
		e := lib0.NewEncoder()
		e.WriteVarUint(v)
		if v > lib0.MaxSafeInteger {
			if !errors.Is(e.Err(), lib0.ErrIntegerOutOfRange) {
				t.Fatalf("WriteVarUint(%d) accepted an unsafe value", v)
			}
			return
		}
		if e.Err() != nil {
			t.Fatalf("WriteVarUint(%d): %v", v, e.Err())
		}
		got, err := lib0.NewDecoder(e.Bytes()).ReadVarUint()
		if err != nil || got != v {
			t.Fatalf("round trip %d -> %x -> %d, %v", v, e.Bytes(), got, err)
		}
	})
}

func FuzzVarIntRoundTrip(f *testing.F) {
	for _, seed := range []int64{0, 1, -1, 63, -64, 1 << 40, -(1 << 40), lib0.MaxSafeInteger} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, v int64) {
		e := lib0.NewEncoder()
		e.WriteVarInt(v)
		if v > lib0.MaxSafeInteger || v < -lib0.MaxSafeInteger {
			if !errors.Is(e.Err(), lib0.ErrIntegerOutOfRange) {
				t.Fatalf("WriteVarInt(%d) accepted an unsafe value", v)
			}
			return
		}
		if e.Err() != nil {
			t.Fatalf("WriteVarInt(%d): %v", v, e.Err())
		}
		got, err := lib0.NewDecoder(e.Bytes()).ReadVarInt()
		if err != nil || got != v {
			t.Fatalf("round trip %d -> %x -> %d, %v", v, e.Bytes(), got, err)
		}
	})
}

func FuzzDecoderNeverPanics(f *testing.F) {
	f.Add([]byte{0x00})
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0x7F})
	f.Add([]byte{0x05, 'h', 'e'})
	f.Fuzz(func(t *testing.T, data []byte) {
		d := lib0.NewDecoder(data)
		for range 4 {
			if _, err := d.ReadVarUint(); err != nil {
				break
			}
		}
		d = lib0.NewDecoder(data)
		for range 4 {
			if _, err := d.ReadVarInt(); err != nil {
				break
			}
		}
		d = lib0.NewDecoder(data)
		for range 4 {
			if _, err := d.ReadVarString(); err != nil {
				break
			}
		}
	})
}
