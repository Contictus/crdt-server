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
