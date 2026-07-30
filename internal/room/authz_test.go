package room

import (
	"testing"
	"time"

	"github.com/mesutokul/ycollab/internal/protocol"
)

// A read-only connection receives the document like any other.
func TestAReadOnlyConnectionStillReads(t *testing.T) {
	now := time.Unix(1700000000, 0)
	r := testRoom(t, &now)
	writer := &fakeConn{id: 1}
	r.handle(joinCmd{writer})
	r.handle(frameCmd{writer, protocol.WriteUpdate(readFixture(t, "text-insert-single", "update-000.bin"))})

	reader := &fakeConn{id: 2, readOnly: true}
	r.handle(joinCmd{reader})
	r.handle(frameCmd{reader, protocol.WriteSyncStep1(emptyStateVector(t))})

	msgs := reader.decodeAll(t)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want SyncStep2 then SyncStep1: %#v", len(msgs), msgs)
	}
	step2, ok := msgs[0].(protocol.SyncStep2Message)
	if !ok {
		t.Fatalf("got %T, want SyncStep2", msgs[0])
	}
	if len(step2.Update) == 0 {
		t.Fatal("a read-only client was sent an empty document")
	}
	if closed, _ := reader.status(); closed {
		t.Fatal("reading closed the connection")
	}
}

// And it is refused, out loud, when it writes.
func TestAReadOnlyConnectionCannotWrite(t *testing.T) {
	now := time.Unix(1700000000, 0)
	r := testRoom(t, &now)
	reader := &fakeConn{id: 1, readOnly: true}
	peer := &fakeConn{id: 2}
	r.handle(joinCmd{reader})
	r.handle(joinCmd{peer})
	reader.take()
	peer.take()

	before := r.doc.StateVector()
	r.handle(frameCmd{reader, protocol.WriteUpdate(readFixture(t, "text-insert-single", "update-000.bin"))})

	if got := len(r.doc.StateVector()); got != len(before) {
		t.Fatal("a read-only connection changed the document")
	}
	msgs := reader.decodeAll(t)
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want one refusal: %#v", len(msgs), msgs)
	}
	denied, ok := msgs[0].(protocol.PermissionDeniedMessage)
	if !ok {
		t.Fatalf("got %T, want PermissionDeniedMessage", msgs[0])
	}
	if denied.Reason == "" {
		t.Fatal("the refusal carried no reason")
	}
	closed, code := reader.status()
	if !closed || code != ClosePolicyViolation {
		t.Fatalf("closed=%v code=%d, want a 1008 close", closed, code)
	}
	// Nobody else hears about an update that was refused.
	if got := peer.frames(); len(got) != 0 {
		t.Fatalf("the refused update was relayed to %d peers: %x", len(got), got)
	}
}

// A well-behaved read-only client answers our SyncStep1 with an update that
// carries nothing. Treating that as an attempt to write would disconnect every
// read-only client on its second message.
func TestAnEmptyUpdateFromAReadOnlyConnectionIsFine(t *testing.T) {
	now := time.Unix(1700000000, 0)
	r := testRoom(t, &now)
	reader := &fakeConn{id: 1, readOnly: true}
	r.handle(joinCmd{reader})
	reader.take()

	r.handle(frameCmd{reader, protocol.WriteSyncStep2(emptyUpdate)})
	if closed, code := reader.status(); closed {
		t.Fatalf("an empty answer closed the connection with %d", code)
	}
	if got := reader.frames(); len(got) != 0 {
		t.Fatalf("an empty answer was refused: %x", got)
	}
}

// Cursors are not writes. A read-only viewer showing up in the document is the
// normal case, not a violation.
func TestAReadOnlyConnectionMayPublishACursor(t *testing.T) {
	now := time.Unix(1700000000, 0)
	r := testRoom(t, &now)
	reader := &fakeConn{id: 1, readOnly: true}
	peer := &fakeConn{id: 2}
	r.handle(joinCmd{reader})
	r.handle(joinCmd{peer})
	reader.take()
	peer.take()

	r.handle(frameCmd{reader, protocol.WriteAwareness(singleEntry(1001, 1, `{"user":"viewer"}`))})
	if closed, _ := reader.status(); closed {
		t.Fatal("publishing a cursor closed a read-only connection")
	}
	msgs := peer.decodeAll(t)
	if len(msgs) != 1 {
		t.Fatalf("the peer got %d messages, want the cursor", len(msgs))
	}
	if _, ok := msgs[0].(protocol.AwarenessMessage); !ok {
		t.Fatalf("got %T, want Awareness", msgs[0])
	}
}

// A refused update must not reach the other replicas either: authorisation is
// enforced where the update enters the system, so there is nothing for the
// cluster to filter later.
func TestARefusedUpdateNeverReachesTheCluster(t *testing.T) {
	now := time.Unix(1700000000, 0)
	reps, _ := newCluster(t, &now, 2, nil)
	a, b := reps[0], reps[1]

	reader := &fakeConn{id: 1, readOnly: true}
	a.room.handle(joinCmd{reader})
	reader.take()
	b.join(2)

	a.room.handle(frameCmd{reader, protocol.WriteUpdate(readFixture(t, "text-insert-single", "update-000.bin"))})
	if published := pump(t, reps); published != 0 {
		t.Fatalf("a refused update produced %d envelopes", published)
	}
	if got, want := b.print(), docPrint(t, a.room.doc); got != want {
		t.Fatalf("replica B\n got %s\nwant %s", got, want)
	}
	if got := a.room.stats.PublishedUpdate.Load(); got != 0 {
		t.Fatalf("published_update is %d after a refusal", got)
	}
}
