package room

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestManagerReusesRoomsPerName(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(ctx, ManagerConfig{Room: Config{IdleTimeout: time.Hour}})

	a, err := m.Join("doc-1", &fakeConn{id: 1})
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Join("doc-1", &fakeConn{id: 2})
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("two connections to the same document got different rooms")
	}
	if _, err := m.Join("doc-2", &fakeConn{id: 3}); err != nil {
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

	if _, err := m.Join("doc-1", &fakeConn{id: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Join("doc-2", &fakeConn{id: 2}); !errors.Is(err, ErrTooManyRooms) {
		t.Fatalf("got %v, want ErrTooManyRooms", err)
	}
	// The cap is on distinct documents, not connections.
	if _, err := m.Join("doc-1", &fakeConn{id: 3}); err != nil {
		t.Fatal(err)
	}
}

func TestManagerCancelClosesRooms(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := NewManager(ctx, ManagerConfig{Room: Config{IdleTimeout: time.Hour}})
	c := &fakeConn{id: 1}
	r, err := m.Join("doc-1", c)
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
			r, err := m.Join("doc", c)
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
	r, err := m.Join("doc", c)
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
	next, err := m.Join("doc", &fakeConn{id: 2})
	if err != nil {
		t.Fatal(err)
	}
	if next == r {
		t.Fatal("got the evicted room back")
	}
}
