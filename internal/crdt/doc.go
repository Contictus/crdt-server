package crdt

// typeRefUnknown marks a root type whose kind the wire never states. Root types
// are addressed by name; only the client knows whether "text" is a YText or a
// YMap (yjs/src/utils/Doc.js get).
const typeRefUnknown = 0xFF

// Doc is one collaborative document: a struct store, the root types, and
// whatever could not be integrated yet.
//
// A Doc is not safe for concurrent use. Phase 2 gives each document a single
// owning goroutine, so no locking is needed here.
type Doc struct {
	// ClientID is the id this server writes under when it edits the document
	// itself. Relayed updates keep their own client ids.
	ClientID ClientID

	store *StructStore
	share map[string]*AbstractType

	// pending holds updates that referred to structs we do not have yet. They
	// are retried after every successful integration, because the update that
	// fills the gap may arrive at any time.
	pending []*Update
	// pendingDeletes holds deletions of clock ranges we have not received.
	pendingDeletes *DeleteSet
}

// NewDoc returns an empty document.
func NewDoc(clientID ClientID) *Doc {
	return &Doc{
		ClientID:       clientID,
		store:          NewStructStore(),
		share:          make(map[string]*AbstractType),
		pendingDeletes: NewDeleteSet(),
	}
}

// Store exposes the struct store. Reading it is safe; mutating it is not.
func (d *Doc) Store() *StructStore { return d.store }

// StateVector returns the clocks this document has integrated.
func (d *Doc) StateVector() StateVector { return d.store.StateVector() }

// EncodeStateVector returns the wire form of the document's state vector.
func (d *Doc) EncodeStateVector() ([]byte, error) {
	return EncodeStateVector(d.store.StateVector())
}

// PendingCount returns how many updates are held back waiting for structs the
// document has not seen. It is non-zero only while an update is out of order.
func (d *Doc) PendingCount() int { return len(d.pending) }

// Get returns the root type called name, creating it if needed.
//
// The wire format never states a root type's kind, so the returned type starts
// out untyped and takes its shape from the first nested type or accessor that
// uses it.
func (d *Doc) Get(name string) *AbstractType {
	if t, ok := d.share[name]; ok {
		return t
	}
	t := newAbstractType(typeRefUnknown)
	t.rootName = name
	d.share[name] = t
	return t
}

// Roots returns the names of the document's root types.
func (d *Doc) Roots() []string {
	names := make([]string, 0, len(d.share))
	for name := range d.share {
		names = append(names, name)
	}
	return names
}

// ApplyUpdate integrates a v1 update.
//
// Updates whose dependencies are missing are kept and retried later rather than
// rejected: Yjs clients may legitimately send an update before the one it
// builds on, and dropping it would lose the edit permanently.
func (d *Doc) ApplyUpdate(b []byte) error {
	u, err := DecodeUpdate(b)
	if err != nil {
		return err
	}
	return d.ApplyDecodedUpdate(u)
}

// ApplyDecodedUpdate integrates an already-parsed update.
func (d *Doc) ApplyDecodedUpdate(u *Update) error {
	d.integrate(u)
	d.applyDeleteSet(u.Deletes)
	d.retryPending()
	return nil
}

// integrate applies as many of the update's structs as it can, keeping the rest
// for later.
func (d *Doc) integrate(u *Update) {
	// Per-client queues, consumed in clock order. A client's structs can only
	// be integrated in order, so a blocked queue stays blocked until whatever
	// it waits for arrives.
	queues := make(map[ClientID][]Struct, len(u.Clients))
	order := make([]ClientID, 0, len(u.Clients))
	for _, block := range u.Clients {
		if _, seen := queues[block.Client]; !seen {
			order = append(order, block.Client)
		}
		queues[block.Client] = append(queues[block.Client], block.Structs...)
	}

	for {
		progress := false
		for _, client := range order {
			queue := queues[client]
			for len(queue) > 0 {
				head := queue[0]
				if _, isSkip := head.(*Skip); isSkip {
					// Skip marks a range the sender knew we already had.
					queue = queue[1:]
					progress = true
					continue
				}
				id := head.StructID()
				state := d.store.State(id.Client)
				if state < id.Clock {
					break // a gap: wait for the structs in between
				}
				offset := int(state - id.Clock)
				if offset >= head.StructLen() {
					queue = queue[1:] // already integrated
					progress = true
					continue
				}
				if !d.resolve(head) {
					break // depends on a struct from another client
				}
				d.integrateStruct(head, offset)
				queue = queue[1:]
				progress = true
			}
			queues[client] = queue
		}
		if !progress {
			break
		}
	}

	// Whatever is left waits for an update that has not arrived.
	var rest []ClientBlock
	for _, client := range order {
		if queue := queues[client]; len(queue) > 0 {
			rest = append(rest, ClientBlock{
				Client:     client,
				StartClock: queue[0].StructID().Clock,
				Structs:    queue,
			})
		}
	}
	if len(rest) > 0 {
		d.pending = append(d.pending, &Update{Clients: rest, Deletes: NewDeleteSet()})
	}
}

// retryPending re-runs held-back updates until none of them makes progress.
func (d *Doc) retryPending() {
	for len(d.pending) > 0 {
		pending := d.pending
		d.pending = nil
		before := 0
		for _, u := range pending {
			for _, block := range u.Clients {
				before += len(block.Structs)
			}
		}
		for _, u := range pending {
			d.integrate(u)
		}
		after := 0
		for _, u := range d.pending {
			for _, block := range u.Clients {
				after += len(block.Structs)
			}
		}
		if after >= before {
			break // nothing moved; stop retrying until new input arrives
		}
	}
	if !d.pendingDeletes.IsEmpty() {
		pending := d.pendingDeletes
		d.pendingDeletes = NewDeleteSet()
		d.applyDeleteSet(pending)
	}
}

// resolve fills in an item's left, right and parent from the store, reporting
// whether every dependency was found.
//
// Mirrors Item.getMissing (yjs/src/structs/Item.js:372).
func (d *Doc) resolve(s Struct) bool {
	it, ok := s.(*Item)
	if !ok {
		return true // GC needs nothing resolved
	}
	// A same-client neighbour is always already present, so only other clients
	// can be missing.
	if it.Origin != nil && it.Origin.Client != it.ID.Client && it.Origin.Clock >= d.store.State(it.Origin.Client) {
		return false
	}
	if it.RightOrigin != nil && it.RightOrigin.Client != it.ID.Client && it.RightOrigin.Clock >= d.store.State(it.RightOrigin.Client) {
		return false
	}
	if it.Parent != nil && !it.Parent.IsRoot &&
		it.Parent.Item.Client != it.ID.Client && it.Parent.Item.Clock >= d.store.State(it.Parent.Item.Client) {
		return false
	}

	if it.Origin != nil {
		left, err := d.store.GetItemCleanEnd(*it.Origin)
		if err != nil {
			// The range is covered by a GC struct: the content is gone, so this
			// item has nowhere to attach and becomes a tombstone below.
			it.left = nil
			it.parent = nil
			return true
		}
		it.left = left
		last := left.LastID()
		it.Origin = &last
	}
	if it.RightOrigin != nil {
		right, err := d.store.GetItemCleanStart(*it.RightOrigin)
		if err != nil {
			it.right = nil
			it.parent = nil
			return true
		}
		it.right = right
	}

	switch {
	case it.Parent == nil:
		// Parent was not on the wire; inherit it from a neighbour, along with
		// parentSub (DECISIONS.md §2.3).
		if it.left != nil {
			it.parent = it.left.parent
			it.HasParentSub = it.left.HasParentSub
			it.ParentSub = it.left.ParentSub
		} else if it.right != nil {
			it.parent = it.right.parent
			it.HasParentSub = it.right.HasParentSub
			it.ParentSub = it.right.ParentSub
		}
	case it.Parent.IsRoot:
		it.parent = d.Get(it.Parent.Name)
	default:
		owner, err := d.store.GetItem(it.Parent.Item)
		if err != nil {
			it.parent = nil
			return true
		}
		ct, ok := owner.Content.(*ContentType)
		if !ok {
			it.parent = nil
			return true
		}
		it.parent = ct.Type
	}
	return true
}

func (d *Doc) integrateStruct(s Struct, offset int) {
	switch st := s.(type) {
	case *GC:
		if offset > 0 {
			st = &GC{ID: ID{Client: st.ID.Client, Clock: st.ID.Clock + Clock(offset)}, Length: st.Length - offset}
		}
		_ = d.store.Append(st)
	case *Item:
		d.integrateItem(st, offset)
	}
}

// integrateItem places an item among its parent's children.
//
// Mirrors Item.integrate (yjs/src/structs/Item.js:419).
func (d *Doc) integrateItem(it *Item, offset int) {
	if offset > 0 {
		// The sender sliced this item; drop the part we already have.
		it.ID.Clock += Clock(offset)
		left, err := d.store.GetItemCleanEnd(ID{Client: it.ID.Client, Clock: it.ID.Clock - 1})
		if err != nil {
			return
		}
		it.left = left
		last := left.LastID()
		it.Origin = &last
		tail, err := it.Content.Splice(offset)
		if err != nil {
			return
		}
		it.Content = tail
	}

	if it.parent == nil {
		// Nothing to attach to: keep the clock range occupied so later structs
		// still resolve, but drop the content.
		_ = d.store.Append(&GC{ID: it.ID, Length: it.StructLen()})
		return
	}

	// Conflict resolution runs only when the neighbourhood is not already
	// exactly left|this|right.
	if (it.left == nil && (it.right == nil || it.right.left != nil)) ||
		(it.left != nil && it.left.right != it.right) {
		it.left = d.resolveConflict(it)
	}

	// Relink.
	if it.left != nil {
		it.right = it.left.right
		it.left.right = it
	} else {
		var r *Item
		if it.HasParentSub {
			r = leftmost(it.parent.mapItems[it.ParentSub])
		} else {
			r = it.parent.start
			it.parent.start = it
		}
		it.right = r
	}
	if it.right != nil {
		it.right.left = it
	} else if it.HasParentSub {
		// This is now the current value for the key; the previous value is
		// deleted, which is exactly how a YMap overwrite works
		// (yjs/src/structs/Item.js:507).
		it.parent.mapItems[it.ParentSub] = it
		if it.left != nil {
			d.deleteItem(it.left)
		}
	}

	_ = d.store.Append(it)

	if _, ok := it.Content.(*ContentDeleted); ok {
		// Deleted content marks its own item deleted rather than holding a
		// value (yjs/src/structs/ContentDeleted.js integrate).
		it.deleted = true
	}
	if ct, ok := it.Content.(*ContentType); ok {
		ct.Type.item = it
	}

	// An item under a deleted parent, or a map entry that is no longer the
	// newest for its key, is born deleted.
	if (it.parent.item != nil && it.parent.item.deleted) || (it.HasParentSub && it.right != nil) {
		d.deleteItem(it)
	}
}

// resolveConflict returns the item this one must sit to the right of, scanning
// the conflicting range the way YATA prescribes.
//
// The tie-break is the part that must not be inverted: when two items share an
// origin, the one with the *smaller* client id ends up on the left. Getting it
// backwards produces documents that agree until two clients type at the same
// position.
func (d *Doc) resolveConflict(it *Item) *Item {
	left := it.left
	var o *Item
	switch {
	case left != nil:
		o = left.right
	case it.HasParentSub:
		o = leftmost(it.parent.mapItems[it.ParentSub])
	default:
		o = it.parent.start
	}

	conflicting := make(map[*Item]bool)
	before := make(map[*Item]bool)
	for o != nil && o != it.right {
		before[o] = true
		conflicting[o] = true
		switch {
		case sameID(it.Origin, o.Origin):
			if o.ID.Client < it.ID.Client {
				left = o
				conflicting = make(map[*Item]bool)
			} else if sameID(it.RightOrigin, o.RightOrigin) {
				return left
			}
		case o.Origin != nil && before[d.itemAt(*o.Origin)]:
			if !conflicting[d.itemAt(*o.Origin)] {
				left = o
				conflicting = make(map[*Item]bool)
			}
		default:
			return left
		}
		o = o.right
	}
	return left
}

// itemAt returns the item covering id, or nil. It is only used for set
// membership, where a miss and a nil are equivalent.
func (d *Doc) itemAt(id ID) *Item {
	it, err := d.store.GetItem(id)
	if err != nil {
		return nil
	}
	return it
}

func leftmost(it *Item) *Item {
	for it != nil && it.left != nil {
		it = it.left
	}
	return it
}

func sameID(a, b *ID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (d *Doc) deleteItem(it *Item) {
	if it.deleted {
		return
	}
	it.deleted = true
}

// applyDeleteSet marks the given ranges deleted, splitting items at the range
// boundaries. Ranges naming clocks we have not received are kept for later.
//
// Mirrors readAndApplyDeleteSet (yjs/src/utils/DeleteSet.js:278).
func (d *Doc) applyDeleteSet(ds *DeleteSet) {
	if ds == nil {
		return
	}
	for client, ranges := range ds.Clients {
		state := d.store.State(client)
		for _, r := range ranges {
			if r.Clock >= state {
				d.pendingDeletes.Add(client, r.Clock, r.Len)
				continue
			}
			end := r.End()
			if end > state {
				// Delete what we have; remember the rest.
				d.pendingDeletes.Add(client, state, int(end-state))
				end = state
			}
			d.deleteRange(client, r.Clock, end)
		}
	}
}

func (d *Doc) deleteRange(client ClientID, from, to Clock) {
	if to <= from {
		return
	}
	// Split at the start boundary so the first item is fully inside the range.
	if _, err := d.store.GetItemCleanStart(ID{Client: client, Clock: from}); err != nil {
		// A GC struct covers the start; nothing there to delete.
		if _, err := d.store.Get(ID{Client: client, Clock: from}); err != nil {
			return
		}
	}
	idx, err := d.store.find(ID{Client: client, Clock: from})
	if err != nil {
		return
	}
	for ; idx < len(d.store.clients[client]); idx++ {
		st := d.store.clients[client][idx]
		if st.StructID().Clock >= to {
			break
		}
		it, ok := st.(*Item)
		if !ok {
			continue
		}
		if it.deleted {
			continue
		}
		if end := it.ID.Clock + Clock(it.StructLen()); end > to {
			// Split at the end boundary so we do not delete past the range.
			if _, err := d.store.splitItem(it, int(to-it.ID.Clock), idx); err != nil {
				continue
			}
		}
		d.deleteItem(it)
	}
}

// DeleteSet returns the deletions this document knows about, built from the
// store the way Yjs builds it (createDeleteSetFromStructStore).
func (d *Doc) DeleteSet() *DeleteSet {
	ds := NewDeleteSet()
	for client, structs := range d.store.clients {
		for _, st := range structs {
			switch s := st.(type) {
			case *Item:
				if !s.deleted {
					continue
				}
			case *GC:
				// A GC struct is by definition deleted content
				// (yjs/src/structs/GC.js: `get deleted () { return true }`).
				// Skipping them would split one range in two wherever garbage
				// collection has run.
			default:
				continue
			}
			ds.Add(client, st.StructID().Clock, st.StructLen())
		}
	}
	return ds
}

// deleteSetForUpdate is what peers are told has been deleted: everything the
// store knows about, plus the deletions being held for structs we have not
// received yet.
//
// Yjs does not include its pendingDs here, and that is the behaviour DECISIONS
// C5 describes: a deletion whose structs a replica has not seen stays invisible
// to that replica's peers, so it travels one exchange behind the structs it
// refers to. Including it costs nothing - a peer that cannot resolve the range
// holds it pending exactly as we do, and a peer that can resolve it applies a
// deletion it was going to receive anyway - and it removes a case where two
// replicas that have exchanged everything still disagree. It also means a
// pending deletion survives a snapshot instead of being dropped on restart.
func (d *Doc) deleteSetForUpdate() *DeleteSet {
	ds := d.DeleteSet()
	for client, ranges := range d.pendingDeletes.Clients {
		for _, r := range ranges {
			ds.Add(client, r.Clock, r.Len)
		}
	}
	return ds
}

// EncodeStateAsUpdate returns everything the holder of sv is missing. A nil sv
// means "everything".
//
// Mirrors writeClientsStructs + writeDeleteSet (yjs/src/utils/encoding.js:81).
func (d *Doc) EncodeStateAsUpdate(sv StateVector) ([]byte, error) {
	u := &Update{Deletes: d.deleteSetForUpdate()}
	for _, client := range d.store.Clients() {
		structs := d.store.clients[client]
		if len(structs) == 0 {
			continue
		}
		clock := sv.Get(client)
		if clock >= d.store.State(client) {
			continue // the peer is up to date on this client
		}
		if first := structs[0].StructID().Clock; clock < first {
			clock = first
		}
		idx, err := d.store.find(ID{Client: client, Clock: clock})
		if err != nil {
			continue
		}
		u.Clients = append(u.Clients, ClientBlock{
			Client:     client,
			StartClock: clock,
			Structs:    structs[idx:],
		})
	}
	return u.Encode()
}

// EncodeDiff returns the update a peer with the given encoded state vector is
// missing.
func (d *Doc) EncodeDiff(encodedSV []byte) ([]byte, error) {
	sv, err := DecodeStateVector(encodedSV)
	if err != nil {
		return nil, err
	}
	return d.EncodeStateAsUpdate(sv)
}
