package room

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mesutokul/ycollab/internal/crdt"
	"github.com/mesutokul/ycollab/internal/hook"
	"github.com/mesutokul/ycollab/internal/protocol"
)

// recorder is a Sink that keeps what it was given.
type recorder struct {
	mu     sync.Mutex
	events []hook.Event
}

func (r *recorder) Emit(e hook.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) all() []hook.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]hook.Event(nil), r.events...)
}

func (r *recorder) of(kind hook.Kind) []hook.Event {
	var out []hook.Event
	for _, e := range r.all() {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// A person typing produces tens of updates a second. One webhook per keystroke
// is not a feature, so the room reports what happened since the last tick, once.
func TestChangesAreCoalescedIntoOneEvent(t *testing.T) {
	sink := &recorder{}
	now := time.Unix(1750000000, 0)
	r := New(Config{
		Name:        "notes",
		NodeID:      9,
		IdleTimeout: time.Hour,
		Hooks:       sink,
		Now:         func() time.Time { return now },
		Logger:      quietLogger(),
	})

	c := &fakeConn{id: 1}
	r.handle(joinCmd{c})
	updates := scenarioUpdates(t, "text-three-client-interleaved")
	for _, u := range updates {
		r.handle(frameCmd{c, protocol.WriteUpdate(u)})
	}
	if got := sink.all(); len(got) != 0 {
		t.Fatalf("an event was emitted before the tick: %v", got)
	}

	r.handle(tickCmd{now})
	changes := sink.of(hook.KindChange)
	if len(changes) != 1 {
		t.Fatalf("%d change events, want 1", len(changes))
	}
	e := changes[0]
	if e.Doc != "notes" || e.Node != 9 || e.Clients != 1 {
		t.Errorf("event says doc=%q node=%d clients=%d", e.Doc, e.Node, e.Clients)
	}
	if e.Updates != uint64(len(updates)) {
		t.Errorf("event covers %d updates, want %d", e.Updates, len(updates))
	}
	if !e.At.Equal(now) {
		t.Errorf("event is stamped %v, want %v", e.At, now)
	}
	// The state vector lets a receiver tell whether it is behind, so it is on
	// every event whether or not the document itself is.
	if len(e.StateVector) == 0 {
		t.Error("no state vector")
	}
	if e.State != nil {
		t.Error("the document was included without being asked for")
	}

	// A tick with nothing new must not produce an event: a receiver watching an
	// idle document should hear nothing at all.
	r.handle(tickCmd{now})
	if got := sink.of(hook.KindChange); len(got) != 1 {
		t.Fatalf("%d change events after an idle tick, want 1", len(got))
	}
}

// -webhook-state is what turns a notification into a copy of the document.
func TestTheDocumentIsIncludedWhenAskedFor(t *testing.T) {
	sink := &recorder{}
	now := time.Unix(1750000000, 0)
	r := New(Config{
		Name: "notes", IdleTimeout: time.Hour, Hooks: sink, HookState: true,
		Now: func() time.Time { return now }, Logger: quietLogger(),
	})
	c := &fakeConn{id: 1}
	r.handle(joinCmd{c})
	for _, u := range scenarioUpdates(t, "text-three-client-interleaved") {
		r.handle(frameCmd{c, protocol.WriteUpdate(u)})
	}
	r.handle(tickCmd{now})

	changes := sink.of(hook.KindChange)
	if len(changes) != 1 {
		t.Fatalf("%d change events, want 1", len(changes))
	}
	// It has to be a Yjs update a client could apply, not an opaque blob, so it
	// is applied to a fresh document and compared with the one the room holds.
	got := crdt.NewDoc(1)
	if err := got.ApplyUpdate(changes[0].State); err != nil {
		t.Fatalf("the state in the event would not apply: %v", err)
	}
	if docPrint(t, got) != docPrint(t, r.doc) {
		t.Errorf("the event carried a different document:\n got %s\nwant %s",
			docPrint(t, got), docPrint(t, r.doc))
	}
}

// One edit must not become one webhook per replica holding the document.
func TestARemoteUpdateDoesNotRaiseAChangeEvent(t *testing.T) {
	now := time.Unix(1750000000, 0)
	author := &recorder{}
	peer := &recorder{}
	sinks := []*recorder{author, peer}
	i := 0
	replicas, _ := newCluster(t, &now, 2, func(cfg *Config) {
		cfg.Hooks = sinks[i]
		i++
	})

	c := replicas[0].join(1)
	updates := scenarioUpdates(t, "text-three-client-interleaved")
	for _, u := range updates {
		replicas[0].room.handle(frameCmd{c, protocol.WriteUpdate(u)})
	}
	pump(t, replicas)
	for _, rep := range replicas {
		rep.room.handle(tickCmd{now})
	}

	if got := len(author.of(hook.KindChange)); got != 1 {
		t.Errorf("the replica the client is on emitted %d change events, want 1", got)
	}
	if got := peer.of(hook.KindChange); len(got) != 0 {
		t.Errorf("the replica that only received the update on the bus emitted %d change events", len(got))
	}
	// The peer did apply it, so this is about who reports, not about who syncs.
	if replicas[0].print() != replicas[1].print() {
		t.Error("the replicas did not converge, so this test proved nothing")
	}
}

// The store event has to mean the rows are actually in the database, which only
// the writer knows, so it is raised there and drained by the room.
func TestStoreEventsFollowSuccessfulWrites(t *testing.T) {
	sink := &recorder{}
	fake := &fakeStore{}
	r := runRoom(t, Config{
		Store: fake, FlushInterval: 5 * time.Millisecond, Hooks: sink,
		Tick: 20 * time.Millisecond,
	})

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
	eventually(t, "a store event", func() bool { return len(sink.of(hook.KindStore)) > 0 })

	var rows uint64
	for _, e := range sink.of(hook.KindStore) {
		rows += e.Updates
	}
	if rows != uint64(len(updates)) {
		t.Errorf("store events cover %d rows, want %d", rows, len(updates))
	}
}

// A failed write must not be reported as a store: a receiver mirroring the
// document would record that it was saved when it was not.
func TestAFailedWriteRaisesNoStoreEvent(t *testing.T) {
	sink := &recorder{}
	fake := &fakeStore{appendErr: errors.New("the database is down")}
	r := runRoom(t, Config{
		Store: fake, FlushInterval: 5 * time.Millisecond, Hooks: sink,
		Tick: 10 * time.Millisecond,
	})

	c := &fakeConn{id: 1}
	if err := r.Join(c); err != nil {
		t.Fatal(err)
	}
	for _, u := range scenarioUpdates(t, "text-three-client-interleaved") {
		if err := r.Deliver(c, protocol.WriteUpdate(u)); err != nil {
			t.Fatal(err)
		}
	}
	// Long enough for several flush intervals and several ticks.
	eventually(t, "a change event", func() bool { return len(sink.of(hook.KindChange)) > 0 })
	if got := sink.of(hook.KindStore); len(got) != 0 {
		t.Errorf("%d store events were raised for writes that failed", len(got))
	}
}

// The last edits before a room evicts are the ones a receiver most wants, and
// they are the ones a tick will never come for.
func TestTheLastEventsGoOutAsTheRoomStops(t *testing.T) {
	sink := &recorder{}
	now := time.Unix(1750000000, 0)
	fake := &fakeStore{}
	r := New(Config{
		Name: "notes", Store: fake, IdleTimeout: time.Hour, Hooks: sink,
		Now: func() time.Time { return now }, Logger: quietLogger(),
	})
	// Start the writer without running the whole room, the way the other
	// persist tests do, so the shutdown path can be driven directly.
	go r.persist(r.jobs)

	c := &fakeConn{id: 1}
	r.handle(joinCmd{c})
	for _, u := range scenarioUpdates(t, "text-three-client-interleaved") {
		r.handle(frameCmd{c, protocol.WriteUpdate(u)})
	}
	r.stop(CloseGoingAway, "test")

	changes := sink.of(hook.KindChange)
	if len(changes) != 1 {
		t.Fatalf("%d change events, want the pending one", len(changes))
	}
	// It is emitted before the connections are closed, so the count is what the
	// room actually had.
	if changes[0].Clients != 1 {
		t.Errorf("the final change event says %d clients, want 1", changes[0].Clients)
	}
	if got := sink.of(hook.KindStore); len(got) == 0 {
		t.Error("the final flush was never reported")
	}
}

// Nothing configured must cost nothing: no events, and no encoding done for
// them either.
func TestNoHooksMeansNoEvents(t *testing.T) {
	now := time.Unix(1750000000, 0)
	r := New(Config{
		Name: "notes", IdleTimeout: time.Hour,
		Now: func() time.Time { return now }, Logger: quietLogger(),
	})
	c := &fakeConn{id: 1}
	r.handle(joinCmd{c})
	for _, u := range scenarioUpdates(t, "text-three-client-interleaved") {
		r.handle(frameCmd{c, protocol.WriteUpdate(u)})
	}
	r.handle(tickCmd{now})
	r.stop(CloseGoingAway, "test")
	if r.changes != 0 {
		t.Errorf("the room counted %d changes with nothing listening", r.changes)
	}
}
