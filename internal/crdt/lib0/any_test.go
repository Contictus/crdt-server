package lib0_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/mesutokul/ycollab/internal/crdt/lib0"
)

// anyCase says what Go value produces a golden vector's bytes, and what the
// decoder must return for them. The two differ where JS types do not survive
// the trip: a JS number decodes to whichever Go type the tag names.
type anyCase struct {
	enc any
	dec any
}

// Keyed by the vector names in tools/fixturegen/lib0.mjs. Every vector must
// appear here, so adding one there fails this test until it is accounted for.
var anyExpectations = map[string]anyCase{
	"undefined":     {lib0.Undefined{}, lib0.Undefined{}},
	"null":          {nil, nil},
	"true":          {true, true},
	"false":         {false, false},
	"int-0":         {0, int64(0)},
	"int-1":         {1, int64(1)},
	"int-neg-1":     {-1, int64(-1)},
	"int-127":       {127, int64(127)},
	"int-128":       {128, int64(128)},
	"int-max31":     {2147483647, int64(2147483647)},
	"int-neg-max31": {-2147483647, int64(-2147483647)},
	// One past the integer range, but exactly representable as a float32.
	"num-2pow31":      {2147483648, float32(2147483648)},
	"float32-1.5":     {1.5, float32(1.5)},
	"float32-neg-0.5": {-0.5, float32(-0.5)},
	"float64-0.1":     {0.1, 0.1},
	"float64-1e300":   {1e300, 1e300},
	// NaN is not float32-representable by lib0's test (NaN != NaN), Inf is.
	"nan":                  {math.NaN(), math.NaN()},
	"infinity":             {math.Inf(1), float32(math.Inf(1))},
	"neg-infinity":         {math.Inf(-1), float32(math.Inf(-1))},
	"negative-zero":        {math.Copysign(0, -1), int64(0)},
	"bigint-0":             {lib0.BigInt(0), lib0.BigInt(0)},
	"bigint-max-safe-plus": {lib0.BigInt(9007199254740993), lib0.BigInt(9007199254740993)},
	"bigint-neg":           {lib0.BigInt(-9007199254740993), lib0.BigInt(-9007199254740993)},
	"string-empty":         {"", ""},
	"string-emoji":         {"hi 🎉", "hi 🎉"},
	"bytes-empty":          {[]byte{}, []byte{}},
	"bytes":                {[]byte{0, 1, 255, 128}, []byte{0, 1, 255, 128}},
	"array-empty":          {[]any{}, []any{}},
	"array-mixed": {
		[]any{1, "two", nil, true, []any{3.5}},
		[]any{int64(1), "two", nil, true, []any{float32(3.5)}},
	},
	"object-empty": {&lib0.Object{}, &lib0.Object{Fields: []lib0.Field{}}},
	"object-nested": {
		&lib0.Object{Fields: []lib0.Field{
			{Key: "zeta", Value: 1},
			{Key: "alpha", Value: &lib0.Object{Fields: []lib0.Field{{Key: "b", Value: []any{1, 2}}}}},
			{Key: "m", Value: "x"},
		}},
		&lib0.Object{Fields: []lib0.Field{
			{Key: "zeta", Value: int64(1)},
			{Key: "alpha", Value: &lib0.Object{Fields: []lib0.Field{
				{Key: "b", Value: []any{int64(1), int64(2)}},
			}}},
			{Key: "m", Value: "x"},
		}},
	},
}

func hexOf(b []byte) string { return hex.EncodeToString(b) }

// sameAny compares decoded values, treating NaN as equal to itself.
func sameAny(a, b any) bool {
	af, aok := a.(float64)
	bf, bok := b.(float64)
	if aok && bok && math.IsNaN(af) && math.IsNaN(bf) {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func TestAnyGolden(t *testing.T) {
	v := loadVectors(t)
	if len(v.Any) == 0 {
		t.Fatal("no any vectors")
	}
	seen := make(map[string]bool, len(v.Any))
	for _, tc := range v.Any {
		want, ok := anyExpectations[tc.Name]
		if !ok {
			t.Errorf("vector %q has no Go expectation; add one to anyExpectations", tc.Name)
			continue
		}
		seen[tc.Name] = true
		wantBytes := mustHex(t, tc.Hex)
		if int(wantBytes[0]) != tc.Tag {
			t.Errorf("%s: fixture tag %d does not match its own bytes %x", tc.Name, tc.Tag, wantBytes)
		}

		e := lib0.NewEncoder()
		e.WriteAny(want.enc)
		if err := e.Err(); err != nil {
			t.Errorf("%s: WriteAny(%#v): %v", tc.Name, want.enc, err)
			continue
		}
		if got := e.Bytes(); !bytes.Equal(got, wantBytes) {
			t.Errorf("%s: WriteAny(%#v) = %x, want %x", tc.Name, want.enc, got, wantBytes)
		}

		d := lib0.NewDecoder(wantBytes)
		got, err := d.ReadAny()
		if err != nil {
			t.Errorf("%s: ReadAny(%x): %v", tc.Name, wantBytes, err)
			continue
		}
		if !sameAny(got, want.dec) {
			t.Errorf("%s: ReadAny(%x) = %#v, want %#v", tc.Name, wantBytes, got, want.dec)
		}
		if !d.Done() {
			t.Errorf("%s: ReadAny left %d bytes unread", tc.Name, d.Remaining())
		}
	}
	for name := range anyExpectations {
		if !seen[name] {
			t.Errorf("expectation %q has no matching golden vector; regenerate fixtures", name)
		}
	}
}

// Go integer kinds are encoded the way JS would encode the same number.
func TestAnyIntegerKinds(t *testing.T) {
	values := []any{int(5), int8(5), int16(5), int32(5), int64(5), uint(5), uint8(5), uint16(5), uint32(5), uint64(5), float32(5), 5.0}
	// float32(5) takes the float32 branch; every other kind is integral and in
	// range, so it takes the varInt branch.
	for _, v := range values {
		e := lib0.NewEncoder()
		e.WriteAny(v)
		if err := e.Err(); err != nil {
			t.Fatalf("WriteAny(%#v): %v", v, err)
		}
		want := "7d05"
		if _, isF32 := v.(float32); isF32 {
			want = "7c40a00000"
		}
		if got := hexOf(e.Bytes()); got != want {
			t.Errorf("WriteAny(%#v) = %s, want %s", v, got, want)
		}
	}
}

// Maps have no wire order, so they are written sorted. Decoding always yields
// an *Object, which does preserve order.
func TestAnyMapIsSorted(t *testing.T) {
	e := lib0.NewEncoder()
	e.WriteAny(map[string]any{"b": 2, "a": 1, "c": 3})
	if err := e.Err(); err != nil {
		t.Fatalf("WriteAny(map): %v", err)
	}
	d := lib0.NewDecoder(e.Bytes())
	got, err := d.ReadAny()
	if err != nil {
		t.Fatalf("ReadAny: %v", err)
	}
	obj, ok := got.(*lib0.Object)
	if !ok {
		t.Fatalf("ReadAny returned %T, want *lib0.Object", got)
	}
	wantKeys := []string{"a", "b", "c"}
	for i, f := range obj.Fields {
		if f.Key != wantKeys[i] {
			t.Fatalf("key order = %v, want %v", obj.Fields, wantKeys)
		}
	}
	if v, ok := obj.Get("b"); !ok || v != int64(2) {
		t.Errorf("Get(\"b\") = %v, %v", v, ok)
	}
	if _, ok := obj.Get("zz"); ok {
		t.Error("Get of a missing key reported ok")
	}
}

func TestObjectSet(t *testing.T) {
	o := &lib0.Object{}
	o.Set("a", 1)
	o.Set("b", 2)
	o.Set("a", 3) // replace in place, do not append
	if len(o.Fields) != 2 || o.Fields[0].Key != "a" || o.Fields[0].Value != 3 {
		t.Fatalf("Set did not replace in place: %#v", o.Fields)
	}
}

func TestAnyUnsupportedType(t *testing.T) {
	e := lib0.NewEncoder()
	e.WriteAny(struct{ X int }{1})
	if !errors.Is(e.Err(), lib0.ErrUnsupportedType) {
		t.Errorf("WriteAny(struct) err = %v, want ErrUnsupportedType", e.Err())
	}
}

func TestAnyUnknownTag(t *testing.T) {
	// Tags below 116 are not defined by lib0's table.
	for _, tag := range []byte{0, 1, 100, 115, 128, 255} {
		d := lib0.NewDecoder([]byte{tag})
		if _, err := d.ReadAny(); !errors.Is(err, lib0.ErrUnknownAnyTag) {
			t.Errorf("ReadAny(tag %d) err = %v, want ErrUnknownAnyTag", tag, err)
		}
	}
}

func TestAnyDepthLimit(t *testing.T) {
	// 200 nested single-element arrays: [[[...]]]
	depth := 200
	buf := make([]byte, 0, depth*2)
	for range depth {
		buf = append(buf, lib0.TagArray, 0x01)
	}
	buf = append(buf, lib0.TagNull)
	d := lib0.NewDecoder(buf)
	if _, err := d.ReadAny(); !errors.Is(err, lib0.ErrDepthExceeded) {
		t.Errorf("ReadAny(deeply nested) err = %v, want ErrDepthExceeded", err)
	}

	var nested any = nil
	for range depth {
		nested = []any{nested}
	}
	e := lib0.NewEncoder()
	e.WriteAny(nested)
	if !errors.Is(e.Err(), lib0.ErrDepthExceeded) {
		t.Errorf("WriteAny(deeply nested) err = %v, want ErrDepthExceeded", e.Err())
	}
}

// A length prefix larger than the remaining input must not drive a huge
// allocation before the read fails.
func TestAnyLyingLengthPrefix(t *testing.T) {
	cases := [][]byte{
		{lib0.TagArray, 0xFF, 0xFF, 0xFF, 0x7F},
		{lib0.TagObject, 0xFF, 0xFF, 0xFF, 0x7F},
	}
	for _, b := range cases {
		d := lib0.NewDecoder(b)
		if _, err := d.ReadAny(); !errors.Is(err, lib0.ErrUnexpectedEOF) {
			t.Errorf("ReadAny(%x) err = %v, want ErrUnexpectedEOF", b, err)
		}
	}
}

func TestAnyRoundTrip(t *testing.T) {
	values := []any{
		nil, lib0.Undefined{}, true, false, "", "ünïcödé 🎉",
		int64(0), int64(-1), int64(2147483647), int64(-2147483647),
		// +Inf must be given as a float32: as a float64 it still encodes with
		// the float32 tag, so it comes back as a float32.
		float32(1.5), 0.1, 1e300, float32(math.Inf(1)),
		lib0.BigInt(-1), []byte{1, 2, 3}, []any{},
		[]any{int64(1), "x", nil, []any{float32(2.5)}},
		&lib0.Object{Fields: []lib0.Field{{Key: "k", Value: int64(7)}, {Key: "n", Value: nil}}},
	}
	e := lib0.NewEncoder()
	for _, v := range values {
		e.WriteAny(v)
	}
	if err := e.Err(); err != nil {
		t.Fatalf("WriteAny: %v", err)
	}
	d := lib0.NewDecoder(e.Bytes())
	for _, want := range values {
		got, err := d.ReadAny()
		if err != nil {
			t.Fatalf("ReadAny(%#v): %v", want, err)
		}
		if !sameAny(got, want) {
			t.Errorf("round trip: got %#v, want %#v", got, want)
		}
	}
	if !d.Done() {
		t.Errorf("%d bytes left unread", d.Remaining())
	}
}

// Decoded values must re-encode to the same bytes, which is what keeps
// ContentAny byte-identical when a document is re-serialised.
func TestAnyReEncodeIsStable(t *testing.T) {
	v := loadVectors(t)
	for _, tc := range v.Any {
		if tc.Name == "negative-zero" {
			continue // -0 decodes to 0; documented asymmetry
		}
		in := mustHex(t, tc.Hex)
		d := lib0.NewDecoder(in)
		val, err := d.ReadAny()
		if err != nil {
			t.Errorf("%s: ReadAny: %v", tc.Name, err)
			continue
		}
		e := lib0.NewEncoder()
		e.WriteAny(val)
		if err := e.Err(); err != nil {
			t.Errorf("%s: re-encode: %v", tc.Name, err)
			continue
		}
		if got := e.Bytes(); !bytes.Equal(got, in) {
			t.Errorf("%s: re-encode = %x, want %x", tc.Name, got, in)
		}
	}
}

func FuzzAnyNeverPanics(f *testing.F) {
	f.Add([]byte{0x7f})
	f.Add([]byte{0x76, 0x01, 0x01, 0x61, 0x7d, 0x05})
	f.Add([]byte{0x75, 0x03, 0x7e, 0x78, 0x79})
	f.Fuzz(func(t *testing.T, b []byte) {
		d := lib0.NewDecoder(b)
		v, err := d.ReadAny()
		if err != nil {
			return
		}
		// Anything that decodes must re-encode without error.
		e := lib0.NewEncoder()
		e.WriteAny(v)
		if e.Err() != nil {
			t.Fatalf("decoded %#v from %x but could not re-encode: %v", v, b, e.Err())
		}
	})
}
