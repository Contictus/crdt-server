package room

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mesutokul/ycollab/internal/crdt"
	"github.com/mesutokul/ycollab/internal/crdt/lib0"
	"github.com/mesutokul/ycollab/internal/protocol"
)

const fixturesDir = "../../testdata/fixtures"

func readFixture(t *testing.T, parts ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{fixturesDir}, parts...)...))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

// fakeConn records what the room sent it and can be told to stop accepting.
type fakeConn struct {
	id     uint64
	sent   [][]byte
	full   bool
	closed bool
	code   int
	reason string
}

func (c *fakeConn) ID() uint64 { return c.id }

func (c *fakeConn) Send(frame []byte) bool {
	if c.full {
		return false
	}
	c.sent = append(c.sent, frame)
	return true
}

func (c *fakeConn) Close(code int, reason string) {
	if c.closed {
		return
	}
	c.closed, c.code, c.reason = true, code, reason
}

// take removes and returns the frames received so far.
func (c *fakeConn) take() [][]byte {
	out := c.sent
	c.sent = nil
	return out
}

func (c *fakeConn) decodeAll(t *testing.T) []protocol.Message {
	t.Helper()
	var out []protocol.Message
	for _, f := range c.take() {
		msg, err := protocol.Decode(f)
		if err != nil {
			t.Fatalf("conn %d: decode %x: %v", c.id, f, err)
		}
		out = append(out, msg)
	}
	return out
}

// testRoom builds a room with an injectable clock, not started: tests drive
// handle directly so there is no goroutine and no sleeping.
func testRoom(t *testing.T, now *time.Time) *Room {
	t.Helper()
	return New(Config{
		Name:        "test",
		IdleTimeout: time.Minute,
		Now:         func() time.Time { return *now },
	})
}

func emptyStateVector(t *testing.T) []byte {
	t.Helper()
	sv, err := crdt.NewDoc(1).EncodeStateVector()
	if err != nil {
		t.Fatal(err)
	}
	return sv
}

// The handshake is the one part of the flow the server drives, and sync.js:23-28
// specifies it exactly: SyncStep2 answering the client's state vector, then our
// own SyncStep1.
func TestSyncHandshake(t *testing.T) {
	now := time.Unix(1700000000, 0)
	r := testRoom(t, &now)
	c := &fakeConn{id: 1}
	r.handle(joinCmd{c})
	if got := c.take(); got != nil {
		t.Fatalf("room spoke before the client did: %x", got)
	}

	r.handle(frameCmd{c, protocol.WriteSyncStep1(emptyStateVector(t))})
	msgs := c.decodeAll(t)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2: %#v", len(msgs), msgs)
	}
	if _, ok := msgs[0].(protocol.SyncStep2Message); !ok {
		t.Fatalf("first reply is %T, want SyncStep2", msgs[0])
	}
	if _, ok := msgs[1].(protocol.SyncStep1Message); !ok {
		t.Fatalf("second reply is %T, want SyncStep1", msgs[1])
	}
}

// A second client asking to sync must be told about the document the first one
// wrote, and about the cursors already in the room.
func TestJoinerReceivesDocumentAndAwareness(t *testing.T) {
	now := time.Unix(1700000000, 0)
	r := testRoom(t, &now)
	a := &fakeConn{id: 1}
	r.handle(joinCmd{a})
	r.handle(frameCmd{a, protocol.WriteSyncStep1(emptyStateVector(t))})
	a.take()

	state := readFixture(t, "text-insert-single", "state.bin")
	r.handle(frameCmd{a, protocol.WriteUpdate(state)})
	r.handle(frameCmd{a, protocol.WriteAwareness(singleEntry(1001, 1, `{"user":"ada"}`))})
	a.take()

	b := &fakeConn{id: 2}
	r.handle(joinCmd{b})
	r.handle(frameCmd{b, protocol.WriteSyncStep1(emptyStateVector(t))})
	msgs := b.decodeAll(t)
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want SyncStep2 + SyncStep1 + awareness: %#v", len(msgs), msgs)
	}

	// The document the joiner receives must be the document that was written.
	doc := crdt.NewDoc(9)
	if err := doc.ApplyUpdate(msgs[0].(protocol.SyncStep2Message).Update); err != nil {
		t.Fatalf("apply step2: %v", err)
	}
	want := crdt.NewDoc(9)
	if err := want.ApplyUpdate(state); err != nil {
		t.Fatal(err)
	}
	gotSV, err := doc.EncodeStateVector()
	if err != nil {
		t.Fatal(err)
	}
	wantSV, err := want.EncodeStateVector()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotSV, wantSV) {
		t.Fatalf("joiner got a different document\n got %x\nwant %x", gotSV, wantSV)
	}

	aw := protocol.NewAwareness()
	if _, err := aw.ApplyUpdate(msgs[2].(protocol.AwarenessMessage).Payload, now); err != nil {
		t.Fatalf("apply awareness: %v", err)
	}
	if got, ok := aw.State(1001); !ok || got != `{"user":"ada"}` {
		t.Fatalf("joiner awareness state %q %v", got, ok)
	}
}

// An update goes to everybody except the client that sent it. The sender
// already has it, and a CRDT server that echoes doubles its own fanout.
func TestUpdateFanoutSkipsSender(t *testing.T) {
	now := time.Unix(1700000000, 0)
	r := testRoom(t, &now)
	a, b, c := &fakeConn{id: 1}, &fakeConn{id: 2}, &fakeConn{id: 3}
	for _, conn := range []*fakeConn{a, b, c} {
		r.handle(joinCmd{conn})
		conn.take()
	}

	update := readFixture(t, "text-insert-single", "update-000.bin")
	r.handle(frameCmd{a, protocol.WriteUpdate(update)})

	if got := a.take(); got != nil {
		t.Fatalf("sender got its own update back: %x", got)
	}
	for _, conn := range []*fakeConn{b, c} {
		msgs := conn.decodeAll(t)
		if len(msgs) != 1 {
			t.Fatalf("conn %d got %d messages, want 1", conn.id, len(msgs))
		}
		// Relayed verbatim: the peer sees exactly the author's bytes.
		if got := msgs[0].(protocol.UpdateMessage).Update; !bytes.Equal(got, update) {
			t.Fatalf("conn %d update\n got %x\nwant %x", conn.id, got, update)
		}
	}
}

func TestAwarenessFanoutSkipsSenderAndRetractsOnLeave(t *testing.T) {
	now := time.Unix(1700000000, 0)
	r := testRoom(t, &now)
	a, b := &fakeConn{id: 1}, &fakeConn{id: 2}
	r.handle(joinCmd{a})
	r.handle(joinCmd{b})

	r.handle(frameCmd{a, protocol.WriteAwareness(singleEntry(1001, 1, `{"user":"ada"}`))})
	if got := a.take(); got != nil {
		t.Fatalf("sender got its own awareness back: %x", got)
	}
	if msgs := b.decodeAll(t); len(msgs) != 1 {
		t.Fatalf("peer got %d messages, want 1", len(msgs))
	}

	// When the connection goes, its cursor must go with it - immediately, not
	// after the 30 s timeout.
	r.handle(leaveCmd{a})
	msgs := b.decodeAll(t)
	if len(msgs) != 1 {
		t.Fatalf("peer got %d messages on leave, want 1", len(msgs))
	}
	peer := protocol.NewAwareness()
	if _, err := peer.ApplyUpdate(singleEntry(1001, 1, `{"user":"ada"}`), now); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.ApplyUpdate(msgs[0].(protocol.AwarenessMessage).Payload, now); err != nil {
		t.Fatal(err)
	}
	if _, present := peer.State(1001); present {
		t.Fatal("cursor survived the disconnect")
	}
}

// The brief's backpressure policy: a connection that cannot keep up is closed
// with 1008. The room must not block, must not buffer more, and must keep
// serving everybody else.
func TestSlowConnectionIsClosedAndRoomSurvives(t *testing.T) {
	now := time.Unix(1700000000, 0)
	r := testRoom(t, &now)
	slow, fast, sender := &fakeConn{id: 1, full: true}, &fakeConn{id: 2}, &fakeConn{id: 3}
	for _, conn := range []*fakeConn{slow, fast, sender} {
		r.handle(joinCmd{conn})
	}

	r.handle(frameCmd{sender, protocol.WriteUpdate(readFixture(t, "text-insert-single", "update-000.bin"))})

	if !slow.closed || slow.code != ClosePolicyViolation {
		t.Fatalf("slow conn closed=%v code=%d, want 1008", slow.closed, slow.code)
	}
	if _, ok := r.conns[slow]; ok {
		t.Fatal("slow conn still in the room")
	}
	if len(fast.sent) != 1 {
		t.Fatalf("fast conn got %d frames, want 1", len(fast.sent))
	}
	// The room is still usable.
	r.handle(frameCmd{sender, protocol.WriteSyncStep1(emptyStateVector(t))})
	if len(sender.sent) == 0 {
		t.Fatal("room stopped answering after dropping a slow client")
	}
}

func TestUndecodableFrameClosesOnlyThatConnection(t *testing.T) {
	now := time.Unix(1700000000, 0)
	r := testRoom(t, &now)
	bad, good := &fakeConn{id: 1}, &fakeConn{id: 2}
	r.handle(joinCmd{bad})
	r.handle(joinCmd{good})

	r.handle(frameCmd{bad, []byte{0x09, 0x09}})
	if !bad.closed || bad.code != CloseProtocolError {
		t.Fatalf("closed=%v code=%d, want 1002", bad.closed, bad.code)
	}
	if good.closed {
		t.Fatal("an unrelated connection was closed")
	}

	r.handle(frameCmd{good, protocol.WriteUpdate([]byte{0xff, 0xff, 0xff})})
	if !good.closed || good.code != CloseProtocolError {
		t.Fatalf("bad update: closed=%v code=%d, want 1002", good.closed, good.code)
	}
}

func TestAwarenessTimeoutRemovesGhosts(t *testing.T) {
	now := time.Unix(1700000000, 0)
	r := testRoom(t, &now)
	ghost, watcher := &fakeConn{id: 1}, &fakeConn{id: 2}
	r.handle(joinCmd{ghost})
	r.handle(joinCmd{watcher})
	r.handle(frameCmd{ghost, protocol.WriteAwareness(singleEntry(1001, 1, `{"user":"ada"}`))})
	watcher.take()

	// The connection is still open - this is the case y-protocols cannot handle
	// on its own (concern C3): a client that stops talking without saying so.
	now = now.Add(10 * time.Second)
	r.handle(tickCmd{now})
	if got := watcher.take(); got != nil {
		t.Fatalf("swept too early: %x", got)
	}

	now = now.Add(25 * time.Second)
	r.handle(tickCmd{now})
	msgs := watcher.decodeAll(t)
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want a removal", len(msgs))
	}
	peer := protocol.NewAwareness()
	if _, err := peer.ApplyUpdate(singleEntry(1001, 1, `{"user":"ada"}`), now); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.ApplyUpdate(msgs[0].(protocol.AwarenessMessage).Payload, now); err != nil {
		t.Fatal(err)
	}
	if _, present := peer.State(1001); present {
		t.Fatal("ghost cursor survived the sweep")
	}
}

func TestIdleRoomEvicts(t *testing.T) {
	now := time.Unix(1700000000, 0)
	var evicted string
	r := New(Config{
		Name:        "test",
		IdleTimeout: time.Minute,
		Now:         func() time.Time { return now },
		OnEvict:     func(name string, _ *crdt.Doc) { evicted = name },
	})

	c := &fakeConn{id: 1}
	r.handle(joinCmd{c})
	now = now.Add(time.Hour)
	if r.handle(tickCmd{now}) {
		t.Fatal("evicted a room with a connection in it")
	}

	r.handle(leaveCmd{c})
	now = now.Add(30 * time.Second)
	if r.handle(tickCmd{now}) {
		t.Fatal("evicted before the idle timeout")
	}
	now = now.Add(31 * time.Second)
	if !r.handle(tickCmd{now}) {
		t.Fatal("did not evict an idle room")
	}
	if evicted != "test" {
		t.Fatalf("OnEvict got %q", evicted)
	}
	select {
	case <-r.Done():
	default:
		t.Fatal("room did not signal Done")
	}
	if err := r.Join(&fakeConn{id: 2}); err != ErrClosed {
		t.Fatalf("join after eviction: %v", err)
	}
}

// singleEntry builds a one-client awareness payload (awareness.js:194).
func singleEntry(client, clock uint64, state string) []byte {
	e := lib0.NewEncoder()
	e.WriteVarUint(1)
	e.WriteVarUint(client)
	e.WriteVarUint(clock)
	e.WriteVarString(state)
	return e.Bytes()
}
