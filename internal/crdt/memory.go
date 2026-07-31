package crdt

// What a document costs to hold.
//
// Until this file the only bound on the server's memory was a count of resident
// rooms, which is a bound on the wrong thing: two thousand documents is either
// forty megabytes or forty gigabytes depending on what is in them, and an
// operator sizing a pod has no way to tell which. A byte figure is what a memory
// limit is written in.
//
// The accounting is arithmetic over the structures rather than a guess with a
// constant in it. Item headers are measured with unsafe.Sizeof, which is exact;
// what those headers point at - origins, parent keys, content - is added
// separately. What it deliberately does not model is the allocator: Go rounds
// every allocation up to a size class, maps carry buckets and overflow, and the
// garbage collector holds freed spans for a while. So this is a floor on the real
// cost, and TestTheEstimateIsWithinReachOfRealMemory measures how far under.

import (
	"unsafe"
)

// Usage is what one document holds.
type Usage struct {
	// Structs is the number of items and GC markers in the store. Exact.
	Structs int
	// Clients is how many client ids have written to the document.
	Clients int
	// Bytes is the estimated heap cost. See the package comment for what it
	// leaves out; it is a floor, not a bound.
	Bytes int64
	// Pending is the number of updates waiting for a gap to be filled. They are
	// held as raw decoded updates and are included in Bytes.
	Pending int
}

// Sizes of the fixed parts, taken from the compiler rather than written down.
var (
	itemSize    = int64(unsafe.Sizeof(Item{}))
	gcSize      = int64(unsafe.Sizeof(GC{}))
	idSize      = int64(unsafe.Sizeof(ID{}))
	parentSize  = int64(unsafe.Sizeof(ParentInfo{}))
	ifaceSize   = int64(unsafe.Sizeof(Content(nil)))
	sliceHeader = int64(unsafe.Sizeof([]Struct(nil)))
	mapEntry    = int64(unsafe.Sizeof(ClientID(0)) + unsafe.Sizeof([]Struct(nil)))
)

// Usage walks the document and reports what it costs.
//
// O(structs), which is why the room caches the answer and only recomputes it
// when the document has changed. That is a real cost and it is measured:
// BenchmarkUsage reports it against documents of a realistic size.
func (d *Doc) Usage() Usage {
	u := Usage{Pending: len(d.pending)}
	if d.store == nil {
		return u
	}
	u.Clients = len(d.store.clients)
	// The map itself, roughly: one entry per client plus the slice headers.
	u.Bytes += int64(u.Clients) * (mapEntry + sliceHeader)

	for _, structs := range d.store.clients {
		u.Structs += len(structs)
		// The backing array, including the capacity append has reserved.
		u.Bytes += int64(cap(structs)) * ifaceSize
		for _, s := range structs {
			u.Bytes += structBytes(s)
		}
	}
	for _, up := range d.pending {
		u.Bytes += updateBytes(up)
	}
	return u
}

// structBytes is one entry in the store, including everything it points at.
func structBytes(s Struct) int64 {
	switch v := s.(type) {
	case *Item:
		n := itemSize
		if v.Origin != nil {
			n += idSize
		}
		if v.RightOrigin != nil {
			n += idSize
		}
		if v.Parent != nil {
			n += parentSize + int64(len(v.Parent.Name))
		}
		n += int64(len(v.ParentSub))
		return n + contentBytes(v.Content)
	case *GC:
		return gcSize
	default:
		// A struct type this file has not been taught about. Counting the
		// interface word is better than counting nothing, and better than
		// panicking in an accounting routine.
		return ifaceSize
	}
}

// contentBytes is the payload an item carries.
//
// A type switch rather than a method on Content, because Content is the wire
// contract - every type there exists because the format has a ref for it - and
// widening it with a question about heap layout would put two unrelated jobs in
// one interface.
func contentBytes(c Content) int64 {
	switch v := c.(type) {
	case nil:
		return 0
	case *ContentDeleted:
		// A tombstone is a length and nothing else, which is why a document that
		// has been edited heavily is cheaper than the text that passed through
		// it.
		return int64(unsafe.Sizeof(*v))
	case *ContentString:
		return int64(unsafe.Sizeof(*v)) + int64(len(v.Str))
	case *ContentBinary:
		return int64(unsafe.Sizeof(*v)) + int64(cap(v.Data))
	case *ContentEmbed:
		return int64(unsafe.Sizeof(*v)) + int64(len(v.JSON))
	case *ContentFormat:
		return int64(unsafe.Sizeof(*v)) + int64(len(v.Key)+len(v.Value))
	case *ContentJSON:
		n := int64(unsafe.Sizeof(*v))
		for _, s := range v.Values {
			n += int64(unsafe.Sizeof(s)) + int64(len(s))
		}
		return n
	case *ContentAny:
		n := int64(unsafe.Sizeof(*v))
		for _, a := range v.Values {
			n += anyBytes(a)
		}
		return n
	case *ContentType:
		// The type's own items are in the struct store and counted there; this
		// is the header only, or every nested type would be counted twice.
		return int64(unsafe.Sizeof(*v)) + typeHeaderBytes(v.Type)
	case *ContentDoc:
		return int64(unsafe.Sizeof(*v)) + int64(len(v.GUID))
	default:
		return ifaceSize
	}
}

// typeHeaderBytes is an AbstractType's own fields, not its contents.
func typeHeaderBytes(t *AbstractType) int64 {
	if t == nil {
		return 0
	}
	n := int64(unsafe.Sizeof(*t)) + int64(len(t.Name))
	for key := range t.mapItems {
		// The items themselves are in the struct store and counted there; this
		// is the index into them.
		n += int64(len(key)) + mapEntry
	}
	return n
}

// anyBytes is one decoded lib0 "any" value. Approximate for the container cases,
// exact for the rest.
func anyBytes(a any) int64 {
	switch v := a.(type) {
	case nil:
		return int64(ifaceSize)
	case string:
		return int64(ifaceSize) + int64(len(v))
	case []byte:
		return int64(ifaceSize) + int64(cap(v))
	case []any:
		n := int64(ifaceSize)
		for _, e := range v {
			n += anyBytes(e)
		}
		return n
	case map[string]any:
		n := int64(ifaceSize)
		for key, e := range v {
			n += int64(len(key)) + mapEntry + anyBytes(e)
		}
		return n
	default:
		// Numbers and booleans, which fit in the interface word.
		return int64(ifaceSize)
	}
}

// updateBytes is a pending update, which is held decoded until the gap it
// depends on is filled.
func updateBytes(u *Update) int64 {
	if u == nil {
		return 0
	}
	var n int64
	for _, block := range u.Clients {
		n += int64(cap(block.Structs)) * ifaceSize
		for _, s := range block.Structs {
			n += structBytes(s)
		}
	}
	return n
}
