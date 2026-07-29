package crdt

import (
	"errors"

	"github.com/mesutokul/ycollab/internal/crdt/lib0"
)

// Content refs, from readItemContent's dispatch table (yjs/src/structs/Item.js
// :712). Refs 0 and 10 share the same five bits but are struct kinds, not
// content - see GC and Skip in struct.go.
const (
	RefGC      = 0
	RefDeleted = 1
	RefJSON    = 2
	RefBinary  = 3
	RefString  = 4
	RefEmbed   = 5
	RefFormat  = 6
	RefType    = 7
	RefAny     = 8
	RefDoc     = 9
	RefSkip    = 10
)

// Type refs for RefType content (yjs/src/structs/ContentType.js:18-33).
const (
	TypeRefArray       = 0
	TypeRefMap         = 1
	TypeRefText        = 2
	TypeRefXMLElement  = 3
	TypeRefXMLFragment = 4
	TypeRefXMLHook     = 5
	TypeRefXMLText     = 6
)

var (
	// ErrUnknownContentRef means the five content bits named a ref Yjs does not
	// define.
	ErrUnknownContentRef = errors.New("crdt: unknown content ref")
	// ErrUnknownTypeRef means a ContentType named a type Yjs does not define.
	ErrUnknownTypeRef = errors.New("crdt: unknown type ref")
	// ErrCorruptUpdate means the update did not parse as a v1 update.
	ErrCorruptUpdate = errors.New("crdt: corrupt update")
)

// Content is the payload of an Item.
//
// Len is in clock units, which is what the containing struct occupies in its
// client's clock space. Countable content also occupies a position in the
// parent type's sequence; ContentFormat is the counter-example (it is a
// zero-width marker), as is ContentDeleted once applied.
type Content interface {
	Ref() uint8
	Len() int
	Countable() bool
	// Write appends the content to e, skipping the first offset clock units.
	// The encoder, not the decoder, performs this slicing (DECISIONS.md §2.3).
	Write(e *lib0.Encoder, offset int)
	// Splice splits the content at offset, returning the tail. The receiver is
	// truncated to offset units in place.
	Splice(offset int) (Content, error)
	// MergeWith appends right to the receiver if the two can be represented as
	// one struct, reporting whether it did.
	MergeWith(right Content) bool
}

// ContentDeleted is a run of deleted clock units that carries no value (ref 1).
// It appears when an update describes deletions the receiver has not seen the
// content of.
type ContentDeleted struct{ Length int }

func (c *ContentDeleted) Ref() uint8      { return RefDeleted }
func (c *ContentDeleted) Len() int        { return c.Length }
func (c *ContentDeleted) Countable() bool { return false }

func (c *ContentDeleted) Write(e *lib0.Encoder, offset int) {
	e.WriteVarUint(uint64(c.Length - offset))
}

func (c *ContentDeleted) Splice(offset int) (Content, error) {
	right := &ContentDeleted{Length: c.Length - offset}
	c.Length = offset
	return right, nil
}

func (c *ContentDeleted) MergeWith(right Content) bool {
	r, ok := right.(*ContentDeleted)
	if !ok {
		return false
	}
	c.Length += r.Length
	return true
}

// ContentJSON is the legacy JSON content (ref 2), superseded by ContentAny but
// still readable. Values are kept as their raw JSON text so that re-encoding
// reproduces the original bytes; the literal "undefined" is not valid JSON and
// marks an absent value (yjs/src/structs/ContentJSON.js:83).
type ContentJSON struct{ Values []string }

func (c *ContentJSON) Ref() uint8      { return RefJSON }
func (c *ContentJSON) Len() int        { return len(c.Values) }
func (c *ContentJSON) Countable() bool { return true }

func (c *ContentJSON) Write(e *lib0.Encoder, offset int) {
	e.WriteVarUint(uint64(len(c.Values) - offset))
	for _, v := range c.Values[offset:] {
		e.WriteVarString(v)
	}
}

func (c *ContentJSON) Splice(offset int) (Content, error) {
	right := &ContentJSON{Values: append([]string(nil), c.Values[offset:]...)}
	c.Values = c.Values[:offset:offset]
	return right, nil
}

func (c *ContentJSON) MergeWith(right Content) bool {
	r, ok := right.(*ContentJSON)
	if !ok {
		return false
	}
	c.Values = append(c.Values, r.Values...)
	return true
}

// ContentBinary is an opaque byte string (ref 3). It is one clock unit however
// long it is.
type ContentBinary struct{ Data []byte }

func (c *ContentBinary) Ref() uint8      { return RefBinary }
func (c *ContentBinary) Len() int        { return 1 }
func (c *ContentBinary) Countable() bool { return true }

func (c *ContentBinary) Write(e *lib0.Encoder, offset int) { e.WriteVarUint8Array(c.Data) }

func (c *ContentBinary) Splice(int) (Content, error) { return nil, errNotSplittable }

func (c *ContentBinary) MergeWith(Content) bool { return false }

// ContentString is text (ref 4). Its length is in UTF-16 code units, which is
// why the struct length and the varString byte length differ for anything
// outside the BMP.
type ContentString struct{ Str string }

func (c *ContentString) Ref() uint8      { return RefString }
func (c *ContentString) Len() int        { return lib0.UTF16Length(c.Str) }
func (c *ContentString) Countable() bool { return true }

func (c *ContentString) Write(e *lib0.Encoder, offset int) {
	if offset == 0 {
		e.WriteVarString(c.Str)
		return
	}
	_, tail, ok := splitUTF16(c.Str, offset)
	if !ok {
		e.WriteVarString(c.Str)
		return
	}
	e.WriteVarString(tail)
}

func (c *ContentString) Splice(offset int) (Content, error) {
	head, tail, ok := splitUTF16(c.Str, offset)
	if !ok {
		return nil, errSplitSurrogate
	}
	c.Str = head
	return &ContentString{Str: tail}, nil
}

func (c *ContentString) MergeWith(right Content) bool {
	r, ok := right.(*ContentString)
	if !ok {
		return false
	}
	c.Str += r.Str
	return true
}

// ContentEmbed is a non-text node inside text (ref 5), e.g. an image in a
// TipTap document. The value is kept as its raw JSON text so re-encoding is
// byte-stable; JSON.stringify key order is not something Go can reproduce from
// a decoded map.
type ContentEmbed struct{ JSON string }

func (c *ContentEmbed) Ref() uint8      { return RefEmbed }
func (c *ContentEmbed) Len() int        { return 1 }
func (c *ContentEmbed) Countable() bool { return true }

func (c *ContentEmbed) Write(e *lib0.Encoder, offset int) { e.WriteVarString(c.JSON) }

func (c *ContentEmbed) Splice(int) (Content, error) { return nil, errNotSplittable }

func (c *ContentEmbed) MergeWith(Content) bool { return false }

// ContentFormat is a zero-width formatting marker inside text (ref 6): bold on,
// bold off. It is not countable, so it occupies a clock unit but no position in
// the visible sequence. Value is raw JSON text, as for ContentEmbed.
type ContentFormat struct {
	Key   string
	Value string
}

func (c *ContentFormat) Ref() uint8      { return RefFormat }
func (c *ContentFormat) Len() int        { return 1 }
func (c *ContentFormat) Countable() bool { return false }

func (c *ContentFormat) Write(e *lib0.Encoder, offset int) {
	e.WriteVarString(c.Key)
	e.WriteVarString(c.Value)
}

func (c *ContentFormat) Splice(int) (Content, error) { return nil, errNotSplittable }

// Format markers never merge: two adjacent markers are two distinct events
// (yjs/src/structs/ContentFormat.js mergeWith returns false).
func (c *ContentFormat) MergeWith(Content) bool { return false }

// ContentType is a nested shared type (ref 7): a YMap inside a YMap, the root
// of a YText, an XML element. The type's children are not part of this content
// - they arrive as separate structs whose parent is this item.
type ContentType struct{ Type *AbstractType }

func (c *ContentType) Ref() uint8      { return RefType }
func (c *ContentType) Len() int        { return 1 }
func (c *ContentType) Countable() bool { return true }

func (c *ContentType) Write(e *lib0.Encoder, offset int) { c.Type.write(e) }

func (c *ContentType) Splice(int) (Content, error) { return nil, errNotSplittable }

func (c *ContentType) MergeWith(Content) bool { return false }

// ContentAny holds arbitrary lib0-any values (ref 8) - the normal content of
// YMap entries and YArray elements. One value is one clock unit.
type ContentAny struct{ Values []any }

func (c *ContentAny) Ref() uint8      { return RefAny }
func (c *ContentAny) Len() int        { return len(c.Values) }
func (c *ContentAny) Countable() bool { return true }

func (c *ContentAny) Write(e *lib0.Encoder, offset int) {
	e.WriteVarUint(uint64(len(c.Values) - offset))
	for _, v := range c.Values[offset:] {
		e.WriteAny(v)
	}
}

func (c *ContentAny) Splice(offset int) (Content, error) {
	right := &ContentAny{Values: append([]any(nil), c.Values[offset:]...)}
	c.Values = c.Values[:offset:offset]
	return right, nil
}

func (c *ContentAny) MergeWith(right Content) bool {
	r, ok := right.(*ContentAny)
	if !ok {
		return false
	}
	c.Values = append(c.Values, r.Values...)
	return true
}

// ContentDoc is a subdocument reference (ref 9). Subdocuments are out of scope
// per the brief, but the content must still round-trip: a client that uses them
// would otherwise have its update rejected or corrupted.
type ContentDoc struct {
	GUID string
	Opts any
}

func (c *ContentDoc) Ref() uint8      { return RefDoc }
func (c *ContentDoc) Len() int        { return 1 }
func (c *ContentDoc) Countable() bool { return true }

func (c *ContentDoc) Write(e *lib0.Encoder, offset int) {
	e.WriteVarString(c.GUID)
	e.WriteAny(c.Opts)
}

func (c *ContentDoc) Splice(int) (Content, error) { return nil, errNotSplittable }

func (c *ContentDoc) MergeWith(Content) bool { return false }

var (
	errNotSplittable  = errors.New("crdt: content of length 1 cannot be split")
	errSplitSurrogate = errors.New("crdt: split would cut a surrogate pair")
)

// splitUTF16 splits s after n UTF-16 code units. It reports false if n falls
// inside a surrogate pair, which would produce invalid UTF-8 in Go and a lone
// surrogate in JS. Yjs never splits there because every offset it uses comes
// from a struct boundary.
func splitUTF16(s string, n int) (head, tail string, ok bool) {
	if n <= 0 {
		return "", s, true
	}
	units := 0
	for i, r := range s {
		if units == n {
			return s[:i], s[i:], true
		}
		if r > 0xFFFF {
			units += 2
		} else {
			units++
		}
		if units > n {
			return "", "", false
		}
	}
	if units == n {
		return s, "", true
	}
	return "", "", false
}

// readContent decodes the content named by the low five bits of info.
func readContent(d *lib0.Decoder, ref uint8) (Content, error) {
	switch ref {
	case RefDeleted:
		n, err := readLen(d)
		if err != nil {
			return nil, err
		}
		return &ContentDeleted{Length: n}, nil
	case RefJSON:
		n, err := readLen(d)
		if err != nil {
			return nil, err
		}
		if n > d.Remaining() {
			return nil, lib0.ErrUnexpectedEOF
		}
		values := make([]string, 0, n)
		for range n {
			s, err := d.ReadVarString()
			if err != nil {
				return nil, err
			}
			values = append(values, s)
		}
		return &ContentJSON{Values: values}, nil
	case RefBinary:
		b, err := d.ReadVarUint8Array()
		if err != nil {
			return nil, err
		}
		// The decoder aliases its input; content outlives the update buffer.
		return &ContentBinary{Data: append([]byte(nil), b...)}, nil
	case RefString:
		s, err := d.ReadVarString()
		if err != nil {
			return nil, err
		}
		return &ContentString{Str: s}, nil
	case RefEmbed:
		s, err := d.ReadVarString()
		if err != nil {
			return nil, err
		}
		return &ContentEmbed{JSON: s}, nil
	case RefFormat:
		key, err := d.ReadVarString()
		if err != nil {
			return nil, err
		}
		val, err := d.ReadVarString()
		if err != nil {
			return nil, err
		}
		return &ContentFormat{Key: key, Value: val}, nil
	case RefType:
		t, err := readAbstractType(d)
		if err != nil {
			return nil, err
		}
		return &ContentType{Type: t}, nil
	case RefAny:
		n, err := readLen(d)
		if err != nil {
			return nil, err
		}
		if n > d.Remaining() {
			return nil, lib0.ErrUnexpectedEOF
		}
		values := make([]any, 0, n)
		for range n {
			v, err := d.ReadAny()
			if err != nil {
				return nil, err
			}
			values = append(values, v)
		}
		return &ContentAny{Values: values}, nil
	case RefDoc:
		guid, err := d.ReadVarString()
		if err != nil {
			return nil, err
		}
		opts, err := d.ReadAny()
		if err != nil {
			return nil, err
		}
		return &ContentDoc{GUID: guid, Opts: opts}, nil
	default:
		return nil, ErrUnknownContentRef
	}
}

// readSafeVarUint reads a varUint that will have to be written back out.
//
// lib0's decoder returns values above 2^53-1 (its range check runs only when a
// continuation byte follows), while its encoder refuses them. Anything in that
// gap could be decoded and then not re-encoded, so it is rejected on the way
// in: no JS client can produce such an id or clock in the first place.
func readSafeVarUint(d *lib0.Decoder) (uint64, error) {
	n, err := d.ReadVarUint()
	if err != nil {
		return 0, err
	}
	if n > lib0.MaxSafeInteger {
		return 0, lib0.ErrIntegerOutOfRange
	}
	return n, nil
}

// readLen reads a length prefix and rejects anything that cannot be an int.
func readLen(d *lib0.Decoder) (int, error) {
	n, err := d.ReadVarUint()
	if err != nil {
		return 0, err
	}
	if n > uint64(maxLen) {
		return 0, lib0.ErrNegativeLength
	}
	return int(n), nil
}

// maxLen caps decoded lengths well below the point where int arithmetic could
// wrap on 32 bit builds.
const maxLen = 1 << 31
