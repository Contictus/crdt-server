package main_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/mesutokul/ycollab/internal/crdt"
	"github.com/mesutokul/ycollab/internal/protocol"
)

// Subdocuments needed checking rather than building.
//
// y-websocket does not mention them: Yjs emits a `subdocs` event and the
// application opens a second provider, naming the room after the subdocument's
// guid. So a subdocument is an ordinary document to this server, and the only
// honest thing to do was to prove that end to end rather than assert it.
//
// This is that proof: a parent carrying a real ContentDoc syncs, a second
// connection syncs the subdocument under its guid, and both converge.
func TestASubdocumentSyncsAsADocumentOfItsOwn(t *testing.T) {
	srv := startServer(t, buildServer(t), freePort(t), "")
	parentName := fmt.Sprintf("book-%d", time.Now().UnixNano())

	// The parent, with its subdocument references, straight from the Yjs
	// fixture - so what the server integrates is bytes Yjs produced.
	author := dial(t, srv.addr, parentName)
	author.sync()
	author.send(protocol.WriteUpdate(readFixture(t, "subdocument", "state.bin")))
	author.send(protocol.WriteSyncStep1(emptyStateVector(t)))
	author.recv()

	// A second client opens the parent and gets the references back intact. A
	// subdocument reference that did not survive the server would leave the
	// client pointing at nothing.
	reader := dial(t, srv.addr, parentName)
	got := reader.sync()
	guids := got.Subdocs()
	if !slices.Equal(guids, []string{"chapter-one"}) {
		t.Fatalf("the parent came back referencing %v, want [chapter-one]", guids)
	}

	// Now the part a client actually does: open the subdocument as its own
	// room, named by the guid.
	sub := dial(t, srv.addr, guids[0])
	sub.sync()
	sub.send(protocol.WriteUpdate(readFixture(t, "text-insert-single", "update-000.bin")))

	// And a second client on the subdocument sees it, which is the whole
	// question: is a subdocument a document here?
	peer := dial(t, srv.addr, guids[0])
	inSub := peer.sync()
	want := crdt.NewDoc(1)
	if err := want.ApplyUpdate(readFixture(t, "text-insert-single", "update-000.bin")); err != nil {
		t.Fatal(err)
	}
	if textOf(t, inSub) != textOf(t, want) {
		t.Errorf("the subdocument reads %q, want %q", textOf(t, inSub), textOf(t, want))
	}

	// The two are separate documents, so an edit in the subdocument does not
	// appear in the parent.
	parentAfter := dial(t, srv.addr, parentName).sync()
	if textOf(t, parentAfter) == textOf(t, want) {
		t.Error("the subdocument's content leaked into the parent")
	}
}

// A parent document is the only thing that names its subdocuments, so without
// this an operator deleting or backing up a document cannot know what else
// belongs to it.
func TestTheReadAPINamesTheSubdocuments(t *testing.T) {
	srv := startServer(t, buildServer(t), freePort(t), "")
	doc := fmt.Sprintf("book-json-%d", time.Now().UnixNano())

	c := dial(t, srv.addr, doc)
	c.sync()
	c.send(protocol.WriteUpdate(readFixture(t, "subdocument", "state.bin")))
	c.send(protocol.WriteSyncStep1(emptyStateVector(t)))
	c.recv()

	resp, body := get(t, srv, "/documents/"+doc+"?format=json", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET returned %d: %s", resp.StatusCode, body)
	}
	var view struct {
		Subdocs []string `json:"subdocs"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, body)
	}
	// "appendix" was referenced and then removed, so it is not part of this
	// document any more and must not appear in a list somebody deletes from.
	if !slices.Equal(view.Subdocs, []string{"chapter-one"}) {
		t.Errorf("subdocs = %v, want [chapter-one]", view.Subdocs)
	}

	// A document with no subdocuments reports an empty list rather than null,
	// so a caller can iterate without a nil check.
	plain := fmt.Sprintf("plain-%d", time.Now().UnixNano())
	p := dial(t, srv.addr, plain)
	p.sync()
	p.send(protocol.WriteUpdate(readFixture(t, "text-insert-single", "update-000.bin")))
	p.send(protocol.WriteSyncStep1(emptyStateVector(t)))
	p.recv()

	_, body = get(t, srv, "/documents/"+plain+"?format=json", nil)
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	if view.Subdocs == nil || len(view.Subdocs) != 0 {
		t.Errorf("subdocs = %v, want an empty list", view.Subdocs)
	}
}
