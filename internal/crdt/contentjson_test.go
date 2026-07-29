package crdt_test

import (
	"bytes"
	"testing"

	"github.com/mesutokul/ycollab/internal/crdt"
	"github.com/mesutokul/ycollab/internal/crdt/lib0"
)

// ContentJSON (ref 2) is the one content type with no fixture: yjs writes
// ContentAny for everything a current client can produce, and there is no way
// to make the public API emit ref 2. Documents written by older versions still
// contain it, so the decoder has to handle it - and untested decoder branches
// are where corruption hides.
//
// The bytes here are therefore built by hand, from ContentJSON.js:
//
//	write: writeVarUint(len(content)); for each, writeVarString(JSON or
//	"undefined") - ContentJSON.js:76-84
//	read:  readVarUint, then readVarString each, mapping "undefined" to
//	undefined - ContentJSON.js:112-121
func jsonUpdate(t *testing.T, values []string) []byte {
	t.Helper()
	e := lib0.NewEncoder()
	e.WriteVarUint(1)   // one client block
	e.WriteVarUint(1)   // one struct
	e.WriteVarUint(101) // client
	e.WriteVarUint(0)   // start clock

	// info: content ref 2, no origin, no rightOrigin, no parentSub - so the
	// struct carries parent info instead.
	e.WriteUint8(2)
	e.WriteVarUint(1)          // parent is a root type
	e.WriteVarString("legacy") // root name

	e.WriteVarUint(uint64(len(values)))
	for _, v := range values {
		e.WriteVarString(v)
	}

	e.WriteVarUint(0) // empty delete set
	if err := e.Err(); err != nil {
		t.Fatalf("build update: %v", err)
	}
	return e.Bytes()
}

func TestContentJSONRoundTrips(t *testing.T) {
	values := []string{`{"a":1}`, `"text"`, `null`, `undefined`, `[1,2,3]`}
	raw := jsonUpdate(t, values)

	u, err := crdt.DecodeUpdate(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(u.Clients) != 1 || len(u.Clients[0].Structs) != 1 {
		t.Fatalf("expected one struct, got %+v", u.Clients)
	}
	item, ok := u.Clients[0].Structs[0].(*crdt.Item)
	if !ok {
		t.Fatalf("got %T, want an Item", u.Clients[0].Structs[0])
	}
	content, ok := item.Content.(*crdt.ContentJSON)
	if !ok {
		t.Fatalf("got %T, want ContentJSON", item.Content)
	}
	if len(content.Values) != len(values) {
		t.Fatalf("got %d values, want %d", len(content.Values), len(values))
	}
	for i, want := range values {
		if content.Values[i] != want {
			t.Fatalf("value %d is %q, want %q", i, content.Values[i], want)
		}
	}
	// The values are kept as text, so "undefined" - which is not JSON - passes
	// through instead of being coerced into null.
	if content.Values[3] != "undefined" {
		t.Fatalf("undefined became %q", content.Values[3])
	}

	again, err := u.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !bytes.Equal(again, raw) {
		t.Fatalf("re-encode differs\n got %x\nwant %x", again, raw)
	}
}

// The struct length is the number of values, so an item holding five of them
// occupies five clock units - the same rule ContentAny follows.
func TestContentJSONIntegrates(t *testing.T) {
	values := []string{`1`, `2`, `3`}
	doc := crdt.NewDoc(1)
	if err := doc.ApplyUpdate(jsonUpdate(t, values)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if n := doc.PendingCount(); n != 0 {
		t.Fatalf("%d updates left pending", n)
	}
	if got := doc.StateVector()[101]; got != crdt.Clock(len(values)) {
		t.Fatalf("clock advanced to %d, want %d", got, len(values))
	}

	// And it survives the round trip through the document, which is what the
	// server does to everything it stores.
	encoded, err := doc.EncodeStateAsUpdate(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	back := crdt.NewDoc(2)
	if err := back.ApplyUpdate(encoded); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if got, want := back.StateVector()[101], crdt.Clock(len(values)); got != want {
		t.Fatalf("clock %d after a round trip, want %d", got, want)
	}
}

// Splitting is how an item gets cut when a neighbour lands inside it, so the
// values have to split with it rather than being duplicated or dropped.
func TestContentJSONSplits(t *testing.T) {
	c := &crdt.ContentJSON{Values: []string{`1`, `2`, `3`, `4`}}
	right, err := c.Splice(2)
	if err != nil {
		t.Fatalf("splice: %v", err)
	}
	if c.Len() != 2 || right.Len() != 2 {
		t.Fatalf("split into %d and %d, want 2 and 2", c.Len(), right.Len())
	}
	if c.Values[1] != `2` || right.(*crdt.ContentJSON).Values[0] != `3` {
		t.Fatalf("split at the wrong place: %v then %v", c.Values, right.(*crdt.ContentJSON).Values)
	}
	if !c.MergeWith(right) {
		t.Fatal("the two halves did not merge back")
	}
	if c.Len() != 4 || c.Values[2] != `3` {
		t.Fatalf("merged into %v", c.Values)
	}
}
