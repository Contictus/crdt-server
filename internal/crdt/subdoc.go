package crdt

import "sort"

// A subdocument is a Y.Doc embedded in another document as ContentDoc (ref 9).
// This engine decodes and re-encodes them faithfully, and does not otherwise
// interpret them - which is the whole of what the wire protocol asks of a
// server, because Yjs syncs a subdocument as a separate document under its own
// guid. y-websocket does not mention subdocuments at all: the application
// listens for the `subdocs` event and opens a second provider, naming the room
// after the guid.
//
// What a server can usefully add is the connection between the two. A parent
// document names its subdocuments and nothing else does, so without reading it
// there is no way to know that deleting "book" orphans "chapter-one", or that a
// backup of "book" is not a backup of the book.

// Subdocs returns the guids of the subdocuments this document currently
// references, sorted.
//
// Deleted references are left out: a subdocument whose reference was removed is
// no longer part of this document, which is exactly the distinction an operator
// deciding what to delete needs. The guid itself is not deleted - the item
// remains in the history - so this reads live items rather than the update log.
func (d *Doc) Subdocs() []string {
	seen := make(map[string]struct{})
	for _, client := range d.store.Clients() {
		for _, st := range d.store.Structs(client) {
			it, ok := st.(*Item)
			if !ok || it.deleted {
				continue
			}
			if doc, ok := it.Content.(*ContentDoc); ok {
				seen[doc.GUID] = struct{}{}
			}
		}
	}
	guids := make([]string, 0, len(seen))
	for guid := range seen {
		guids = append(guids, guid)
	}
	sort.Strings(guids)
	return guids
}
