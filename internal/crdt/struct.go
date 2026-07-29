package crdt

import "github.com/mesutokul/ycollab/internal/crdt/lib0"

// Info byte bits (yjs/src/structs/Item.js:655). The low five bits carry the
// content ref; the top three flag which optional fields follow.
const (
	bitsContentRef = 0x1F
	bitParentSub   = 0x20 // BIT6: parentSub is non-null
	bitRightOrigin = 0x40 // BIT7: rightOrigin is present
	bitOrigin      = 0x80 // BIT8: origin is present
)

// Struct is one entry of a client's clock space: an Item, a GC placeholder or a
// Skip marker.
type Struct interface {
	StructID() ID
	// StructLen is the number of clock units the struct occupies.
	StructLen() int
	// write appends the struct to e, skipping the first offset clock units.
	write(e *lib0.Encoder, offset int)
}

// GC replaces an item whose content has been garbage collected (ref 0). It
// keeps the clock range occupied so that later structs still resolve, but has
// no content and no neighbours.
type GC struct {
	ID     ID
	Length int
}

func (g *GC) StructID() ID   { return g.ID }
func (g *GC) StructLen() int { return g.Length }

func (g *GC) write(e *lib0.Encoder, offset int) {
	e.WriteUint8(RefGC)
	e.WriteVarUint(uint64(g.Length - offset))
}

// Skip marks a clock range the sender deliberately did not send (ref 10). It
// appears in diffs where the receiver is known to have the structs already.
type Skip struct {
	ID     ID
	Length int
}

func (s *Skip) StructID() ID   { return s.ID }
func (s *Skip) StructLen() int { return s.Length }

func (s *Skip) write(e *lib0.Encoder, offset int) {
	e.WriteUint8(RefSkip)
	// Written with a plain varUint rather than writeLen. Identical in v1;
	// deliberately different in v2 (yjs/src/structs/Skip.js:47).
	e.WriteVarUint(uint64(s.Length - offset))
}

// ParentInfo is the parent as it appears on the wire: either a root type name
// or the ID of the item holding the parent type.
type ParentInfo struct {
	IsRoot bool
	Name   string // root type name, when IsRoot
	Item   ID     // owning item, when !IsRoot
}

// Item is one YATA operation: a run of content inserted between two known
// neighbours.
//
// Origin and RightOrigin are the neighbours *at insertion time*, not the
// current ones. That is what makes concurrent edits converge: every replica
// replays the same intent against the same reference points.
type Item struct {
	ID          ID
	Origin      *ID
	RightOrigin *ID

	// Parent is the parent as decoded. It is only present on the wire when the
	// item has neither origin nor rightOrigin; otherwise it is inherited from a
	// neighbour during integration (DECISIONS.md §2.3).
	Parent *ParentInfo

	// HasParentSub mirrors the info bit, which is set whenever parentSub is
	// non-null even when the string itself is not written. ParentSub holds the
	// key once it is known - from the wire, or from the left neighbour.
	HasParentSub bool
	ParentSub    string

	Content Content

	// Integration state, all nil/false until the item is integrated.
	left    *Item
	right   *Item
	parent  *AbstractType
	deleted bool
	// keep marks an item that must survive garbage collection because
	// something still refers to it.
	keep bool
}

func (it *Item) StructID() ID { return it.ID }

func (it *Item) StructLen() int { return it.Content.Len() }

// Deleted reports whether the item has been deleted.
func (it *Item) Deleted() bool { return it.deleted }

// Countable reports whether the item occupies a position in its parent's
// sequence: it must be alive and hold countable content.
func (it *Item) Countable() bool { return !it.deleted && it.Content.Countable() }

// LastID is the ID of the item's final clock unit - the value an item to its
// right would carry as its origin.
func (it *Item) LastID() ID {
	return ID{Client: it.ID.Client, Clock: it.ID.Clock + Clock(it.StructLen()) - 1}
}

func (it *Item) write(e *lib0.Encoder, offset int) {
	origin := it.Origin
	if offset > 0 {
		// A block written from the middle of an item synthesises a
		// self-referencing origin so the receiver can still place it
		// (yjs/src/structs/Item.js:656).
		id := ID{Client: it.ID.Client, Clock: it.ID.Clock + Clock(offset) - 1}
		origin = &id
	}

	info := it.Content.Ref() & bitsContentRef
	if origin != nil {
		info |= bitOrigin
	}
	if it.RightOrigin != nil {
		info |= bitRightOrigin
	}
	if it.HasParentSub {
		info |= bitParentSub
	}
	e.WriteUint8(info)

	if origin != nil {
		writeID(e, *origin)
	}
	if it.RightOrigin != nil {
		writeID(e, *it.RightOrigin)
	}
	if origin == nil && it.RightOrigin == nil {
		it.writeParentInfo(e)
		if it.HasParentSub {
			e.WriteVarString(it.ParentSub)
		}
	}
	it.Content.Write(e, offset)
}

func (it *Item) writeParentInfo(e *lib0.Encoder) {
	// An integrated item knows its parent type; a decoded one only knows what
	// the wire said. Prefer the resolved parent: after integration it is the
	// authority, and re-encoding must not depend on how the item arrived.
	if it.parent != nil {
		if owner := it.parent.item; owner != nil {
			e.WriteVarUint(0)
			writeID(e, owner.ID)
		} else {
			e.WriteVarUint(1)
			e.WriteVarString(it.parent.rootName)
		}
		return
	}
	if it.Parent == nil {
		// Cannot happen for a well-formed item: an item with neither origin nor
		// rightOrigin always carries parent info.
		e.WriteVarUint(1)
		e.WriteVarString("")
		return
	}
	if it.Parent.IsRoot {
		e.WriteVarUint(1)
		e.WriteVarString(it.Parent.Name)
		return
	}
	e.WriteVarUint(0)
	writeID(e, it.Parent.Item)
}

func writeID(e *lib0.Encoder, id ID) {
	e.WriteVarUint(uint64(id.Client))
	e.WriteVarUint(uint64(id.Clock))
}

func readID(d *lib0.Decoder) (ID, error) {
	client, err := readSafeVarUint(d)
	if err != nil {
		return ID{}, err
	}
	clock, err := readSafeVarUint(d)
	if err != nil {
		return ID{}, err
	}
	return ID{Client: ClientID(client), Clock: Clock(clock)}, nil
}

// readStruct decodes one struct of client's clock space starting at clock.
func readStruct(d *lib0.Decoder, client ClientID, clock Clock) (Struct, error) {
	info, err := d.ReadUint8()
	if err != nil {
		return nil, err
	}
	id := ID{Client: client, Clock: clock}
	ref := info & bitsContentRef
	if ref == RefGC || ref == RefSkip {
		// GC and Skip have no neighbours and no parent, so the three flag bits
		// are meaningless. Yjs ignores them and writes them back as zero, i.e.
		// it silently rewrites the struct; we reject instead, so that an update
		// this server relays is always the update it received.
		if info&(bitParentSub|bitRightOrigin|bitOrigin) != 0 {
			return nil, ErrCorruptUpdate
		}
	}
	switch ref {
	case RefGC:
		n, err := readLen(d)
		if err != nil {
			return nil, err
		}
		return &GC{ID: id, Length: n}, nil
	case RefSkip:
		n, err := readLen(d)
		if err != nil {
			return nil, err
		}
		return &Skip{ID: id, Length: n}, nil
	}

	it := &Item{ID: id}
	if info&bitOrigin != 0 {
		origin, err := readID(d)
		if err != nil {
			return nil, err
		}
		it.Origin = &origin
	}
	if info&bitRightOrigin != 0 {
		right, err := readID(d)
		if err != nil {
			return nil, err
		}
		it.RightOrigin = &right
	}
	it.HasParentSub = info&bitParentSub != 0

	// Parent info is on the wire only when both neighbours are absent. Reading
	// the parentSub string whenever the info bit is set - the obvious mistake -
	// desynchronises the byte stream on the first map update with a left
	// neighbour.
	if it.Origin == nil && it.RightOrigin == nil {
		kind, err := d.ReadVarUint()
		if err != nil {
			return nil, err
		}
		parent := &ParentInfo{}
		switch kind {
		case 1:
			parent.IsRoot = true
			name, err := d.ReadVarString()
			if err != nil {
				return nil, err
			}
			parent.Name = name
		case 0:
			pid, err := readID(d)
			if err != nil {
				return nil, err
			}
			parent.Item = pid
		default:
			return nil, ErrCorruptUpdate
		}
		it.Parent = parent
		if it.HasParentSub {
			sub, err := d.ReadVarString()
			if err != nil {
				return nil, err
			}
			it.ParentSub = sub
		}
	}

	content, err := readContent(d, info&bitsContentRef)
	if err != nil {
		return nil, err
	}
	it.Content = content
	return it, nil
}
