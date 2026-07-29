package crdt

import (
	"sort"

	"github.com/mesutokul/ycollab/internal/crdt/lib0"
)

// DeleteRange is a half-open range [Clock, Clock+Len) of one client's clock
// space that has been deleted.
type DeleteRange struct {
	Clock Clock
	Len   int
}

// End returns the first clock past the range.
func (r DeleteRange) End() Clock { return r.Clock + Clock(r.Len) }

// DeleteSet records deletions per client. Deletions are not operations in YATA:
// they are a separate set that travels alongside the structs, which is why an
// update can delete content the receiver has never seen.
type DeleteSet struct {
	Clients map[ClientID][]DeleteRange
}

// NewDeleteSet returns an empty delete set.
func NewDeleteSet() *DeleteSet {
	return &DeleteSet{Clients: make(map[ClientID][]DeleteRange)}
}

// IsEmpty reports whether the set deletes nothing.
func (ds *DeleteSet) IsEmpty() bool {
	for _, ranges := range ds.Clients {
		if len(ranges) > 0 {
			return false
		}
	}
	return true
}

// Add records that [clock, clock+length) of client is deleted. Ranges are kept
// sorted and adjacent or overlapping ranges are merged, which is the shape
// createDeleteSetFromStructStore produces (yjs/src/utils/DeleteSet.js:188).
func (ds *DeleteSet) Add(client ClientID, clock Clock, length int) {
	if length <= 0 {
		return
	}
	if ds.Clients == nil {
		ds.Clients = make(map[ClientID][]DeleteRange)
	}
	ranges := append(ds.Clients[client], DeleteRange{Clock: clock, Len: length})
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].Clock < ranges[j].Clock })
	merged := ranges[:1]
	for _, r := range ranges[1:] {
		last := &merged[len(merged)-1]
		if r.Clock <= last.End() {
			if end := r.End(); end > last.End() {
				last.Len = int(end - last.Clock)
			}
			continue
		}
		merged = append(merged, r)
	}
	ds.Clients[client] = merged
}

// IsDeleted reports whether id falls in a deleted range.
func (ds *DeleteSet) IsDeleted(id ID) bool {
	ranges := ds.Clients[id.Client]
	// Ranges are sorted, so the first one starting past id cannot contain it.
	i := sort.Search(len(ranges), func(i int) bool { return ranges[i].End() > id.Clock })
	return i < len(ranges) && ranges[i].Clock <= id.Clock
}

// clientsDescending returns the client ids in the order Yjs writes them: higher
// ids first (yjs/src/utils/DeleteSet.js:227).
func (ds *DeleteSet) clientsDescending() []ClientID {
	clients := make([]ClientID, 0, len(ds.Clients))
	for client := range ds.Clients {
		// Clients with no ranges are kept rather than dropped: Yjs never writes
		// one, but an update that contains one must still re-encode byte for
		// byte, since the server relays bytes it did not produce.
		clients = append(clients, client)
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i] > clients[j] })
	return clients
}

func (ds *DeleteSet) write(e *lib0.Encoder) {
	clients := ds.clientsDescending()
	e.WriteVarUint(uint64(len(clients)))
	for _, client := range clients {
		ranges := ds.Clients[client]
		e.WriteVarUint(uint64(client))
		e.WriteVarUint(uint64(len(ranges)))
		for _, r := range ranges {
			// v1 writes both plainly; v2 delta-encodes them
			// (yjs/src/utils/UpdateEncoder.js:24,31).
			e.WriteVarUint(uint64(r.Clock))
			e.WriteVarUint(uint64(r.Len))
		}
	}
}

func readDeleteSet(d *lib0.Decoder) (*DeleteSet, error) {
	ds := NewDeleteSet()
	numClients, err := d.ReadVarUint()
	if err != nil {
		return nil, err
	}
	// Each client costs at least three bytes; a larger count is corrupt input
	// and must not drive an allocation.
	if numClients > uint64(d.Remaining()) {
		return nil, lib0.ErrUnexpectedEOF
	}
	for range numClients {
		client, err := readSafeVarUint(d)
		if err != nil {
			return nil, err
		}
		numRanges, err := d.ReadVarUint()
		if err != nil {
			return nil, err
		}
		if numRanges > uint64(d.Remaining()) {
			return nil, lib0.ErrUnexpectedEOF
		}
		ranges := make([]DeleteRange, 0, numRanges)
		for range numRanges {
			clock, err := readSafeVarUint(d)
			if err != nil {
				return nil, err
			}
			length, err := readLen(d)
			if err != nil {
				return nil, err
			}
			ranges = append(ranges, DeleteRange{Clock: Clock(clock), Len: length})
		}
		ds.Clients[ClientID(client)] = ranges
	}
	return ds, nil
}
