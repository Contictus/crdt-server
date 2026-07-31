package room

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mesutokul/ycollab/internal/protocol"
)

func TestManagerReusesRoomsPerName(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(ctx, ManagerConfig{Room: Config{IdleTimeout: time.Hour}})

	a, err := m.Join("doc-1", &fakeConn{id: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Join("doc-1", &fakeConn{id: 2}, "")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("two connections to the same document got different rooms")
	}
	if _, err := m.Join("doc-2", &fakeConn{id: 3}, ""); err != nil {
		t.Fatal(err)
	}
	if got := m.Len(); got != 2 {
		t.Fatalf("%d rooms resident, want 2", got)
	}
}

func TestManagerCapsResidentRooms(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(ctx, ManagerConfig{MaxRooms: 1, Room: Config{IdleTimeout: time.Hour}})

	if _, err := m.Join("doc-1", &fakeConn{id: 1}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Join("doc-2", &fakeConn{id: 2}, ""); !errors.Is(err, ErrTooManyRooms) {
		t.Fatalf("got %v, want ErrTooManyRooms", err)
	}
	// The cap is on distinct documents, not connections.
	if _, err := m.Join("doc-1", &fakeConn{id: 3}, ""); err != nil {
		t.Fatal(err)
	}
}

// The cap is an LRU, not a wall: an idle room is written out and dropped to
// make space, because refusing to open a document is worse than paying for a
// snapshot.
func TestManagerEvictsLeastRecentlyUsed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &fakeStore{}
	m := NewManager(ctx, ManagerConfig{MaxRooms: 2, Room: Config{
		IdleTimeout:   time.Hour,
		Store:         fake,
		FlushInterval: 2 * time.Millisecond,
		Logger:        quietLogger(),
	}})

	oldest := &fakeConn{id: 1}
	if _, err := m.Join("doc-1", oldest, ""); err != nil {
		t.Fatal(err)
	}
	newer := &fakeConn{id: 2}
	if _, err := m.Join("doc-2", newer, ""); err != nil {
		t.Fatal(err)
	}
	// doc-1 is now idle and least recently used; doc-2 still has a connection.
	if _, err := m.Join("doc-1", oldest, ""); err != nil {
		t.Fatal(err)
	}
	// Give it something worth writing: an eviction with nothing to fold is a
	// no-op, which is what makes the snapshot assertion below meaningful.
	if err := m.rooms["doc-1"].Deliver(oldest, protocol.WriteUpdate(readFixture(t, "text-insert-single", "state.bin"))); err != nil {
		t.Fatal(err)
	}
	if err := m.rooms["doc-1"].Leave(oldest); err != nil {
		t.Fatal(err)
	}

	// doc-3 does not fit, so the idle room goes and its document is written.
	if _, err := m.Join("doc-3", &fakeConn{id: 3}, ""); err != nil {
		t.Fatalf("join: %v", err)
	}
	if m.Len() != 2 {
		t.Fatalf("%d rooms resident, want 2", m.Len())
	}
	m.mu.Lock()
	_, stillThere := m.rooms["doc-1"]
	m.mu.Unlock()
	if stillThere {
		t.Fatal("the least recently used room was not evicted")
	}
	if _, snapshots := fake.counts(); snapshots == 0 {
		t.Fatal("the evicted room did not write itself out")
	}

	// A room with somebody in it is never evicted for space.
	if _, err := m.Join("doc-4", &fakeConn{id: 4}, ""); !errors.Is(err, ErrTooManyRooms) {
		t.Fatalf("got %v, want ErrTooManyRooms", err)
	}
	if closed, _ := newer.status(); closed {
		t.Fatal("a connected client was disconnected to free memory")
	}
}

func TestManagerCancelClosesRooms(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := NewManager(ctx, ManagerConfig{Room: Config{IdleTimeout: time.Hour}})
	c := &fakeConn{id: 1}
	r, err := m.Join("doc-1", c, "")
	if err != nil {
		t.Fatal(err)
	}

	cancel()
	m.Wait()

	select {
	case <-r.Done():
	default:
		t.Fatal("room did not stop")
	}
	if !c.closed || c.code != CloseGoingAway {
		t.Fatalf("conn closed=%v code=%d, want 1001", c.closed, c.code)
	}
}

// A room evicts itself from its own goroutine while connections are arriving on
// others. The handover must not lose a connection: every joiner ends up in a
// live room or is closed so it reconnects. This is the one genuinely concurrent
// path in the package, so it is written to be run under -race.
func TestJoinRacesEviction(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A room that is idle from the moment it is created and ticks constantly:
	// every join is racing an eviction.
	m := NewManager(ctx, ManagerConfig{Room: Config{
		Tick:        time.Microsecond,
		IdleTimeout: time.Nanosecond,
	}})

	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := &fakeConn{id: uint64(i)}
			r, err := m.Join("doc", c, "")
			if err != nil {
				t.Errorf("join: %v", err)
				return
			}
			// Either the room took us, or it was already going away and closed
			// us. Both are fine; silently dropping the connection is not.
			if err := r.Deliver(c, nil); err != nil && !errors.Is(err, ErrClosed) {
				t.Errorf("deliver: %v", err)
			}
			_ = r.Leave(c)
		}()
	}
	wg.Wait()
}

func TestManagerForgetsEvictedRooms(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(ctx, ManagerConfig{Room: Config{
		Tick:        time.Millisecond,
		IdleTimeout: time.Nanosecond,
	}})

	c := &fakeConn{id: 1}
	r, err := m.Join("doc", c, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Leave(c)
	<-r.Done()

	// The registry must have let go of the evicted room before it started
	// refusing joins, or the next Join would hand out a dead room forever.
	deadline := time.Now().Add(time.Second)
	for m.Len() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("evicted room is still registered")
		}
	}
	next, err := m.Join("doc", &fakeConn{id: 2}, "")
	if err != nil {
		t.Fatal(err)
	}
	if next == r {
		t.Fatal("got the evicted room back")
	}
}
