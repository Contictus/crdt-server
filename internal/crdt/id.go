package crdt

import "strconv"

// ClientID identifies one writer. Yjs generates it randomly per Doc
// (yjs/src/utils/Doc.js: `random.uint32()`), so it is not dense and must never
// be used as an index.
type ClientID uint64

// Clock counts operations of one client. Every struct occupies a half-open
// range [Clock, Clock+Len) of its client's clock space, and those ranges are
// contiguous and gapless per client.
//
// The unit is deliberately not "characters": for string content it is UTF-16
// code units, matching JavaScript's String.length (DECISIONS.md §2.11 item 9).
type Clock uint64

// ID addresses one clock tick of one client - i.e. one element of content.
type ID struct {
	Client ClientID
	Clock  Clock
}

// NewID returns the ID at clock of client.
func NewID(client ClientID, clock Clock) ID { return ID{Client: client, Clock: clock} }

// String renders an ID the way the Yjs debug output does.
func (id ID) String() string {
	return strconv.FormatUint(uint64(id.Client), 10) + ":" + strconv.FormatUint(uint64(id.Clock), 10)
}

// Compare orders IDs by client, then clock. It defines no semantic ordering of
// operations - concurrent edits from different clients are unordered by
// definition. It exists so that maps of IDs can be iterated deterministically.
func (id ID) Compare(other ID) int {
	switch {
	case id.Client < other.Client:
		return -1
	case id.Client > other.Client:
		return 1
	case id.Clock < other.Clock:
		return -1
	case id.Clock > other.Clock:
		return 1
	}
	return 0
}
