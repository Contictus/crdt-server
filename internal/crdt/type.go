package crdt

import "github.com/mesutokul/ycollab/internal/crdt/lib0"

// AbstractType is a shared type: the thing items hang off. Every type is either
// a root type (reachable by name from the document) or the content of a
// ContentType item.
//
// One struct covers all seven type refs. The Go API exposes only YText and YMap
// (DECISIONS.md §D8), but every ref must be represented faithfully so that an
// XML document from y-prosemirror round-trips byte for byte.
type AbstractType struct {
	TypeRef uint8
	// Name is the nodeName of a YXmlElement or the hookName of a YXmlHook, and
	// empty for every other type ref.
	Name string

	// start is the head of the sequence (YText, YArray, XML children).
	start *Item
	// mapItems holds the newest item per key (YMap, XML attributes). Older
	// items for the same key remain linked through Item.left.
	mapItems map[string]*Item

	// item is the ContentType item that owns this type; nil for root types.
	item *Item
	// rootName is the document-level name; empty for non-root types.
	rootName string
}

func newAbstractType(typeRef uint8) *AbstractType {
	return &AbstractType{TypeRef: typeRef, mapItems: make(map[string]*Item)}
}

// write emits the type ref plus whatever extra field that ref carries
// (yjs/src/structs/ContentType.js write -> AbstractType._write).
func (t *AbstractType) write(e *lib0.Encoder) {
	e.WriteVarUint(uint64(t.TypeRef))
	switch t.TypeRef {
	case TypeRefXMLElement, TypeRefXMLHook:
		e.WriteVarString(t.Name)
	}
}

// ToJSON renders the type's live content as Go values: string for text types,
// map[string]any for maps, []any for sequences. Values inside come from
// lib0.ReadAny, and nested types recurse.
//
// It is a debugging and test view, not a hot path: it walks the item list.
func (t *AbstractType) ToJSON() any {
	switch t.TypeRef {
	case TypeRefText, TypeRefXMLText:
		return AsText(t).String()
	case TypeRefMap:
		return t.mapJSON()
	case TypeRefArray, TypeRefXMLFragment, TypeRefXMLElement:
		return t.sequenceJSON()
	default:
		// A root type's kind is never stated on the wire. Guess from what it
		// actually holds; an empty type reads as an empty map.
		if t.start != nil {
			return t.sequenceJSON()
		}
		return t.mapJSON()
	}
}

func (t *AbstractType) mapJSON() map[string]any {
	out := make(map[string]any, len(t.mapItems))
	for key, it := range t.mapItems {
		if it == nil || it.deleted {
			continue
		}
		if v, ok := itemValue(it); ok {
			out[key] = v
		}
	}
	return out
}

func (t *AbstractType) sequenceJSON() []any {
	out := []any{}
	for it := t.start; it != nil; it = it.right {
		if !it.Countable() {
			continue
		}
		switch c := it.Content.(type) {
		case *ContentAny:
			out = append(out, c.Values...)
		case *ContentString:
			out = append(out, c.Str)
		case *ContentType:
			out = append(out, c.Type.ToJSON())
		case *ContentDoc:
			out = append(out, c)
		case *ContentBinary:
			out = append(out, c.Data)
		case *ContentJSON:
			for _, v := range c.Values {
				out = append(out, v)
			}
		}
	}
	return out
}

// itemValue returns the value a map entry holds.
func itemValue(it *Item) (any, bool) {
	switch c := it.Content.(type) {
	case *ContentAny:
		if len(c.Values) == 0 {
			return nil, false
		}
		return c.Values[len(c.Values)-1], true
	case *ContentType:
		return c.Type.ToJSON(), true
	case *ContentString:
		return c.Str, true
	case *ContentBinary:
		return c.Data, true
	case *ContentDoc:
		return c, true
	case *ContentJSON:
		if len(c.Values) == 0 {
			return nil, false
		}
		return c.Values[len(c.Values)-1], true
	default:
		return nil, false
	}
}

func readAbstractType(d *lib0.Decoder) (*AbstractType, error) {
	ref, err := d.ReadVarUint()
	if err != nil {
		return nil, err
	}
	if ref > TypeRefXMLText {
		return nil, ErrUnknownTypeRef
	}
	t := newAbstractType(uint8(ref))
	switch t.TypeRef {
	case TypeRefXMLElement, TypeRefXMLHook:
		name, err := d.ReadVarString()
		if err != nil {
			return nil, err
		}
		t.Name = name
	}
	return t, nil
}
