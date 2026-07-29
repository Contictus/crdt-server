package crdt

import (
	"errors"
	"sort"
)

var (
	// ErrMissingStruct means a struct referred to an ID the store does not
	// have. Callers integrating an update should treat it as "not yet".
	ErrMissingStruct = errors.New("crdt: struct not in store")
	// ErrClockGap means a struct would leave a hole in its client's clock
	// space, which the format forbids.
	ErrClockGap = errors.New("crdt: struct leaves a clock gap")
)

// StructStore holds every struct this document has integrated, grouped by
// client and sorted by clock.
//
// Per client the structs are contiguous and gapless: struct i ends exactly
// where struct i+1 begins. That invariant is what makes the binary search in
// find valid, and it is why an update with a gap must be held back rather than
// integrated.
type StructStore struct {
	clients map[ClientID][]Struct
}

// NewStructStore returns an empty store.
func NewStructStore() *StructStore {
	return &StructStore{clients: make(map[ClientID][]Struct)}
}

// State returns the next expected clock for client: one past the last struct.
func (s *StructStore) State(client ClientID) Clock {
	structs := s.clients[client]
	if len(structs) == 0 {
		return 0
	}
	last := structs[len(structs)-1]
	return last.StructID().Clock + Clock(last.StructLen())
}

// StateVector returns the store's state vector.
func (s *StructStore) StateVector() StateVector {
	sv := make(StateVector, len(s.clients))
	for client := range s.clients {
		sv[client] = s.State(client)
	}
	return sv
}

// Clients returns the client ids present, sorted descending - the order every
// client-keyed section of the wire format uses.
func (s *StructStore) Clients() []ClientID {
	clients := make([]ClientID, 0, len(s.clients))
	for client := range s.clients {
		clients = append(clients, client)
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i] > clients[j] })
	return clients
}

// Structs returns the structs of one client in clock order. The slice is the
// store's own; callers must not modify it.
func (s *StructStore) Structs(client ClientID) []Struct { return s.clients[client] }

// Append adds a struct at the end of its client's list. The struct must start
// exactly where the client's clock space currently ends.
func (s *StructStore) Append(st Struct) error {
	id := st.StructID()
	if state := s.State(id.Client); state != id.Clock {
		return ErrClockGap
	}
	s.clients[id.Client] = append(s.clients[id.Client], st)
	return nil
}

// find returns the index of the struct covering clock.
//
// Mirrors findIndexSS (yjs/src/utils/StructStore.js:129): a binary search that
// exploits the gapless invariant, not a linear scan.
func (s *StructStore) find(id ID) (int, error) {
	structs := s.clients[id.Client]
	if len(structs) == 0 {
		return 0, ErrMissingStruct
	}
	i := sort.Search(len(structs), func(i int) bool {
		st := structs[i]
		return st.StructID().Clock+Clock(st.StructLen()) > id.Clock
	})
	if i >= len(structs) || structs[i].StructID().Clock > id.Clock {
		return 0, ErrMissingStruct
	}
	return i, nil
}

// Get returns the struct covering id.
func (s *StructStore) Get(id ID) (Struct, error) {
	i, err := s.find(id)
	if err != nil {
		return nil, err
	}
	return s.clients[id.Client][i], nil
}

// GetItem returns the item covering id, or an error if the range is covered by
// a GC or Skip struct instead.
func (s *StructStore) GetItem(id ID) (*Item, error) {
	st, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	it, ok := st.(*Item)
	if !ok {
		return nil, ErrMissingStruct
	}
	return it, nil
}

// GetItemCleanStart returns the item that starts exactly at id, splitting the
// covering item if it starts earlier.
//
// Splitting is what makes YATA's neighbour references exact: an item may only
// point at a boundary, never into the middle of another item.
func (s *StructStore) GetItemCleanStart(id ID) (*Item, error) {
	i, err := s.find(id)
	if err != nil {
		return nil, err
	}
	structs := s.clients[id.Client]
	it, ok := structs[i].(*Item)
	if !ok {
		return nil, ErrMissingStruct
	}
	offset := int(id.Clock - it.ID.Clock)
	if offset == 0 {
		return it, nil
	}
	right, err := s.splitItem(it, offset, i)
	if err != nil {
		return nil, err
	}
	return right, nil
}

// GetItemCleanEnd returns the item that ends exactly after id, splitting the
// covering item if it extends further.
func (s *StructStore) GetItemCleanEnd(id ID) (*Item, error) {
	i, err := s.find(id)
	if err != nil {
		return nil, err
	}
	structs := s.clients[id.Client]
	it, ok := structs[i].(*Item)
	if !ok {
		return nil, ErrMissingStruct
	}
	offset := int(id.Clock - it.ID.Clock)
	if offset == it.StructLen()-1 {
		return it, nil
	}
	if _, err := s.splitItem(it, offset+1, i); err != nil {
		return nil, err
	}
	return it, nil
}

// splitItem cuts it in two after offset clock units and inserts the tail into
// the store at index+1. It returns the new right-hand item.
//
// Mirrors splitItem (yjs/src/structs/Item.js:53).
func (s *StructStore) splitItem(it *Item, offset int, index int) (*Item, error) {
	tail, err := it.Content.Splice(offset)
	if err != nil {
		return nil, err
	}
	right := &Item{
		ID:           ID{Client: it.ID.Client, Clock: it.ID.Clock + Clock(offset)},
		Origin:       &ID{Client: it.ID.Client, Clock: it.ID.Clock + Clock(offset) - 1},
		RightOrigin:  it.RightOrigin,
		HasParentSub: it.HasParentSub,
		ParentSub:    it.ParentSub,
		Content:      tail,
		left:         it,
		right:        it.right,
		parent:       it.parent,
		deleted:      it.deleted,
		keep:         it.keep,
	}
	if it.Parent != nil {
		// Keep the decoded parent so an item that was never integrated still
		// re-encodes correctly.
		p := *it.Parent
		right.Parent = &p
	}
	if right.right != nil {
		right.right.left = right
	}
	it.right = right

	// A split item that was the newest entry for its map key hands that role to
	// its right half (Item.js:76).
	if right.HasParentSub && right.right == nil && right.parent != nil {
		right.parent.mapItems[right.ParentSub] = right
	}

	structs := s.clients[it.ID.Client]
	structs = append(structs, nil)
	copy(structs[index+2:], structs[index+1:])
	structs[index+1] = right
	s.clients[it.ID.Client] = structs
	return right, nil
}

// followRedone is not implemented: the undo manager is out of scope, so no
// struct in this store is ever a redone version of another.
