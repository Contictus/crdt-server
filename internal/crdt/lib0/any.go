package lib0

import (
	"encoding/binary"
	"math"
	"sort"
)

// The lib0 "any" format: a one byte type tag followed by a payload. Tags count
// down from 127 so that low numbers stay free for application use; readAny
// indexes its table with 127-tag (lib0/decoding.js readAny).
const (
	TagUndefined  = 127
	TagNull       = 126
	TagInteger    = 125
	TagFloat32    = 124
	TagFloat64    = 123
	TagBigInt     = 122
	TagFalse      = 121
	TagTrue       = 120
	TagString     = 119
	TagObject     = 118
	TagArray      = 117
	TagUint8Array = 116
)

// maxInt31 is lib0's binary.BITS31. A JS number is written as a varInt only
// when it is integral and within this range (lib0/encoding.js writeAny).
const maxInt31 = 0x7FFFFFFF

// maxAnyDepth bounds recursion so that hostile input cannot exhaust the stack.
// Yjs itself puts no limit here, but nothing a real client sends comes close.
const maxAnyDepth = 128

// canonicalNaN64 is the NaN bit pattern JavaScript engines produce.
const canonicalNaN64 = 0x7FF8000000000000

// Undefined is the Go stand-in for JavaScript's undefined, which is a distinct
// any value from null (tag 127 vs 126). ContentAny round-trips it.
type Undefined struct{}

// BigInt is a JavaScript BigInt, written as a signed big-endian 64 bit value
// (tag 122). It is a distinct Go type because a plain int64 is encoded the way
// JS would encode the same *number*, which is not the same thing.
type BigInt int64

// Field is one key/value pair of an Object.
type Field struct {
	Key   string
	Value any
}

// Object is a JS object with its key order preserved.
//
// Order matters: lib0 writes keys in Object.keys order and Yjs stores the
// resulting bytes verbatim, so re-encoding a decoded value must reproduce them.
// A Go map cannot do that, which is why decoding produces *Object rather than
// map[string]any. Encoding accepts either; a map is written with sorted keys so
// that our own output is at least deterministic.
type Object struct {
	Fields []Field
}

// Get returns the value stored under key.
func (o *Object) Get(key string) (any, bool) {
	for _, f := range o.Fields {
		if f.Key == key {
			return f.Value, true
		}
	}
	return nil, false
}

// Set replaces the value under key, keeping its position, or appends it.
func (o *Object) Set(key string, v any) {
	for i := range o.Fields {
		if o.Fields[i].Key == key {
			o.Fields[i].Value = v
			return
		}
	}
	o.Fields = append(o.Fields, Field{Key: key, Value: v})
}

// WriteAny writes v in lib0's any format.
//
// Mirrors lib0/encoding.js writeAny. Supported: nil (null), Undefined, bool,
// string, all int/uint kinds, float32, float64, BigInt, []byte, []any, *Object,
// Object and map[string]any. Anything else records ErrUnsupportedType.
func (e *Encoder) WriteAny(v any) { e.writeAny(v, 0) }

func (e *Encoder) writeAny(v any, depth int) {
	if e.err != nil {
		return
	}
	if depth > maxAnyDepth {
		e.fail(ErrDepthExceeded)
		return
	}
	switch t := v.(type) {
	case nil:
		e.WriteUint8(TagNull)
	case Undefined:
		e.WriteUint8(TagUndefined)
	case bool:
		if t {
			e.WriteUint8(TagTrue)
		} else {
			e.WriteUint8(TagFalse)
		}
	case string:
		e.WriteUint8(TagString)
		e.WriteVarString(t)
	case BigInt:
		e.WriteUint8(TagBigInt)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(t))
		e.WriteBytes(b[:])
	case []byte:
		e.WriteUint8(TagUint8Array)
		e.WriteVarUint8Array(t)
	case []any:
		e.WriteUint8(TagArray)
		e.WriteVarUint(uint64(len(t)))
		for _, item := range t {
			e.writeAny(item, depth+1)
		}
	case *Object:
		e.writeFields(t.Fields, depth)
	case Object:
		e.writeFields(t.Fields, depth)
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fields := make([]Field, len(keys))
		for i, k := range keys {
			fields[i] = Field{Key: k, Value: t[k]}
		}
		e.writeFields(fields, depth)
	case float32:
		e.WriteUint8(TagFloat32)
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], math.Float32bits(t))
		e.WriteBytes(b[:])
	case float64:
		e.writeNumber(t)
	case int:
		e.writeNumber(float64(t))
	case int8:
		e.writeNumber(float64(t))
	case int16:
		e.writeNumber(float64(t))
	case int32:
		e.writeNumber(float64(t))
	case int64:
		e.writeNumber(float64(t))
	case uint:
		e.writeNumber(float64(t))
	case uint8:
		e.writeNumber(float64(t))
	case uint16:
		e.writeNumber(float64(t))
	case uint32:
		e.writeNumber(float64(t))
	case uint64:
		e.writeNumber(float64(t))
	default:
		e.fail(ErrUnsupportedType)
	}
}

func (e *Encoder) writeFields(fields []Field, depth int) {
	e.WriteUint8(TagObject)
	e.WriteVarUint(uint64(len(fields)))
	for _, f := range fields {
		e.WriteVarString(f.Key)
		e.writeAny(f.Value, depth+1)
	}
}

// writeNumber picks the same branch lib0 does for a JS number: varInt when the
// value is integral and fits in 31 bits, float32 when it survives a float32
// round trip, float64 otherwise.
func (e *Encoder) writeNumber(v float64) {
	if v == math.Trunc(v) && math.Abs(v) <= maxInt31 {
		e.WriteUint8(TagInteger)
		if v == 0 && math.Signbit(v) {
			// lib0 writes negative zero with the sign bit set; int64 cannot
			// hold -0, so emit the byte directly to stay byte-identical.
			e.WriteUint8(0x40)
			return
		}
		e.WriteVarInt(int64(v))
		return
	}
	// NaN fails this comparison (NaN != NaN) and so takes the float64 branch,
	// while +-Inf survives it and is written as a float32 - same as lib0's
	// isFloat32 test bed.
	if float64(float32(v)) == v {
		e.WriteUint8(TagFloat32)
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], math.Float32bits(float32(v)))
		e.WriteBytes(b[:])
		return
	}
	e.WriteUint8(TagFloat64)
	bits := math.Float64bits(v)
	if math.IsNaN(v) {
		// Go's math.NaN() is 0x7FF8000000000001 while V8 produces the quiet NaN
		// 0x7FF8000000000000. Both are NaN, but only one matches the bytes a
		// browser writes, and byte identity is the point. Any NaN payload is
		// dropped - JS cannot express one anyway.
		bits = canonicalNaN64
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], bits)
	e.WriteBytes(b[:])
}

// ReadAny reads a lib0 any value.
//
// Mirrors lib0/decoding.js readAny. The Go types produced are: nil, Undefined,
// bool, int64 (tag 125), float32, float64, BigInt, string, *Object, []any and
// []byte. Byte slices alias the decoder's input.
func (d *Decoder) ReadAny() (any, error) { return d.readAny(0) }

func (d *Decoder) readAny(depth int) (any, error) {
	if depth > maxAnyDepth {
		return nil, ErrDepthExceeded
	}
	tag, err := d.ReadUint8()
	if err != nil {
		return nil, err
	}
	switch tag {
	case TagUndefined:
		return Undefined{}, nil
	case TagNull:
		return nil, nil
	case TagInteger:
		return d.ReadVarInt()
	case TagFloat32:
		b, err := d.ReadBytes(4)
		if err != nil {
			return nil, err
		}
		return math.Float32frombits(binary.BigEndian.Uint32(b)), nil
	case TagFloat64:
		b, err := d.ReadBytes(8)
		if err != nil {
			return nil, err
		}
		return math.Float64frombits(binary.BigEndian.Uint64(b)), nil
	case TagBigInt:
		b, err := d.ReadBytes(8)
		if err != nil {
			return nil, err
		}
		return BigInt(binary.BigEndian.Uint64(b)), nil
	case TagFalse:
		return false, nil
	case TagTrue:
		return true, nil
	case TagString:
		return d.ReadVarString()
	case TagObject:
		n, err := d.ReadVarUint()
		if err != nil {
			return nil, err
		}
		// A key/value pair costs at least two bytes, so a length larger than
		// the remaining input is a lie and must not drive an allocation.
		if n > uint64(d.Remaining()) {
			return nil, ErrUnexpectedEOF
		}
		obj := &Object{Fields: make([]Field, 0, n)}
		for range n {
			key, err := d.ReadVarString()
			if err != nil {
				return nil, err
			}
			val, err := d.readAny(depth + 1)
			if err != nil {
				return nil, err
			}
			obj.Fields = append(obj.Fields, Field{Key: key, Value: val})
		}
		return obj, nil
	case TagArray:
		n, err := d.ReadVarUint()
		if err != nil {
			return nil, err
		}
		if n > uint64(d.Remaining()) {
			return nil, ErrUnexpectedEOF
		}
		arr := make([]any, 0, n)
		for range n {
			val, err := d.readAny(depth + 1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, val)
		}
		return arr, nil
	case TagUint8Array:
		return d.ReadVarUint8Array()
	default:
		return nil, ErrUnknownAnyTag
	}
}
