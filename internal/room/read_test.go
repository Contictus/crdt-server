package room

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mesutokul/ycollab/internal/crdt"
	"github.com/mesutokul/ycollab/internal/protocol"
	"github.com/mesutokul/ycollab/internal/store"
)

// A read goes through the mailbox like every other command, so what it returns
// is what the connected clients are looking at rather than what the database
// last heard about.
func TestReadingAResidentRoom(t *testing.T) {
	r := runRoom(t, Config{Name: "notes"})
	c := &fakeConn{id: 1}
	if err := r.Join(c); err != nil {
		t.Fatal(err)
	}
	updates := scenarioUpdates(t, "text-three-client-interleaved")
	for _, u := range updates {
		if err := r.Deliver(c, protocol.WriteUpdate(u)); err != nil {
			t.Fatal(err)
		}
	}

	snapshot, err := r.Read(nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !snapshot.Resident || snapshot.Clients != 1 {
		t.Errorf("resident=%v clients=%d", snapshot.Resident, snapshot.Clients)
	}
	got := crdt.NewDoc(1)
	if err := got.ApplyUpdate(snapshot.Update); err != nil {
		t.Fatalf("the snapshot would not apply: %v", err)
	}
	if docPrint(t, got) != canonical(t, updates) {
		t.Errorf("read a different document:\n got %s\nwant %s", docPrint(t, got), canonical(t, updates))
	}

	// The same read with the state vector it just returned is the caller
	// saying "I have this", and has to come back carrying nothing new.
	diff, err := r.Read(snapshot.StateVector)
	if err != nil {
		t.Fatalf("Read with a state vector: %v", err)
	}
	if len(diff.Update) >= len(snapshot.Update) {
		t.Errorf("the diff is %d bytes and the document is %d", len(diff.Update), len(snapshot.Update))
	}
	before := docPrint(t, got)
	if err := got.ApplyUpdate(diff.Update); err != nil {
		t.Fatalf("the diff would not apply: %v", err)
	}
	if docPrint(t, got) != before {
		t.Error("a diff against the current state vector changed the document")
	}
}

// A room that stopped between the lookup and the read reports it, so the caller
// can fall back to the database instead of returning an error to somebody who
// only asked for a document.
func TestReadingARoomThatHasStopped(t *testing.T) {
	r := New(Config{Name: "notes", IdleTimeout: time.Hour, Logger: quietLogger()})
	r.stop(CloseGoingAway, "test")
	if _, err := r.Read(nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Read returned %v, want ErrClosed", err)
	}
}

// Reading a document nobody is editing must not start a room: waking one would
// hold the document in memory and join it to the cluster as a side effect of
// somebody looking at it.
func TestFetchReadsTheStoreWithoutARoom(t *testing.T) {
	updates := scenarioUpdates(t, "text-three-client-interleaved")
	fake := &fakeStore{doc: store.Document{Updates: updates}}

	snapshot, err := Fetch(context.Background(), fake, "notes", nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snapshot.Resident {
		t.Error("a document read from the store was reported as resident")
	}
	if snapshot.Clients != -1 {
		t.Errorf("clients is %d, want -1 for a document with no room", snapshot.Clients)
	}
	got := crdt.NewDoc(1)
	if err := got.ApplyUpdate(snapshot.Update); err != nil {
		t.Fatalf("the snapshot would not apply: %v", err)
	}
	if docPrint(t, got) != canonical(t, updates) {
		t.Errorf("read a different document:\n got %s\nwant %s", docPrint(t, got), canonical(t, updates))
	}
}

// A stored row that will not apply is skipped rather than making the document
// unreadable - but skipping it silently would hand back a document that is
// quietly missing edits, and whoever asked is very possibly taking a backup.
func TestFetchReportsRowsItCouldNotApply(t *testing.T) {
	updates := scenarioUpdates(t, "text-three-client-interleaved")
	// Two rows of rubbish among the real ones.
	corrupt := append([][]byte{}, updates...)
	corrupt = append(corrupt, []byte("this is not an update"), []byte{0xff, 0xff, 0xff})
	fake := &fakeStore{doc: store.Document{Updates: corrupt}}

	snapshot, err := Fetch(context.Background(), fake, "notes", nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snapshot.Skipped != 2 {
		t.Errorf("Skipped = %d, want 2", snapshot.Skipped)
	}
	// And the readable rows still made it, because one bad row must not make
	// the document unopenable.
	got := crdt.NewDoc(1)
	if err := got.ApplyUpdate(snapshot.Update); err != nil {
		t.Fatal(err)
	}
	if docPrint(t, got) != canonical(t, updates) {
		t.Error("the readable rows did not survive the corrupt ones")
	}

	// A clean document reports zero, so the field is a signal rather than noise.
	clean := &fakeStore{doc: store.Document{Updates: updates}}
	if snapshot, err := Fetch(context.Background(), clean, "notes", nil); err != nil || snapshot.Skipped != 0 {
		t.Errorf("a clean document reported Skipped=%d (err %v)", snapshot.Skipped, err)
	}
}

// A name that was never written is not an empty document. A caller asking for
// something that does not exist wants to hear so.
func TestFetchingAMissingDocument(t *testing.T) {
	if _, err := Fetch(context.Background(), &fakeStore{}, "never-written", nil); !errors.Is(err, ErrNoDocument) {
		t.Fatalf("Fetch returned %v, want ErrNoDocument", err)
	}
	// And with no store at all, which is what a server started without
	// -database-url has for every document it is not currently holding.
	if _, err := Fetch(context.Background(), nil, "anything", nil); !errors.Is(err, ErrNoDocument) {
		t.Fatalf("Fetch without a store returned %v, want ErrNoDocument", err)
	}
}

// Resident is a lookup, not a get-or-create: it must not be the thing that
// starts a room.
func TestResidentDoesNotCreate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(ctx, ManagerConfig{Room: Config{IdleTimeout: time.Hour, Logger: quietLogger()}})
	t.Cleanup(m.Wait)

	if r := m.Resident("notes"); r != nil {
		t.Fatal("Resident returned a room for a name nothing has opened")
	}
	if m.Len() != 0 {
		t.Fatalf("%d rooms are resident after a lookup", m.Len())
	}
	if _, err := m.Join("notes", &fakeConn{id: 1}, ""); err != nil {
		t.Fatal(err)
	}
	if r := m.Resident("notes"); r == nil {
		t.Fatal("Resident did not find a room that exists")
	}
}
