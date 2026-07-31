package room

// The memory budget.
//
// What is worth testing here is not the arithmetic - crdt's own tests measure
// the estimate against real heap - but the policy: that a bound written in bytes
// actually evicts, that it evicts the right room, and that it never evicts the
// room somebody is using.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// budgetManager returns a manager with a byte budget and no room count cap, so
// only the budget can be what evicts.
func budgetManager(t *testing.T, budget int64) *Manager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return NewManager(ctx, ManagerConfig{
		MaxMemory: budget,
		Room: Config{
			IdleTimeout: time.Hour,
			Now:         time.Now,
		},
	})
}

// fill puts a document of a known size into a room, by applying a real update
// and driving the room's own measurement.
func fill(t *testing.T, r *Room, update []byte) int64 {
	t.Helper()
	r.touchDocument()
	if err := r.doc.ApplyUpdate(update); err != nil {
		t.Fatal(err)
	}
	r.usage.measured = time.Time{}
	r.measure(time.Now())
	if r.Bytes() <= 0 {
		t.Fatalf("a room holding %d bytes of update reports %d", len(update), r.Bytes())
	}
	return r.Bytes()
}

// A room reports what it holds, and the manager adds them up.
func TestResidentBytesIsTheSumOfTheRooms(t *testing.T) {
	m := budgetManager(t, 0)
	update := readFixture(t, "text-three-client-interleaved", "update-000.bin")

	if got := m.ResidentBytes(); got != 0 {
		t.Fatalf("an empty manager reports %d bytes", got)
	}

	var want int64
	for i := range 3 {
		r, err := m.Join(fmt.Sprintf("doc-%d", i), &fakeConn{id: uint64(i + 1)}, "")
		if err != nil {
			t.Fatal(err)
		}
		want += fill(t, r, update)
	}
	if got := m.ResidentBytes(); got != want {
		t.Errorf("the manager reports %d, the rooms hold %d", got, want)
	}
}

// The point of the whole thing: a bound written in bytes evicts.
func TestTheBudgetEvicts(t *testing.T) {
	update := readFixture(t, "text-three-client-interleaved", "update-000.bin")

	// Measure one document first, so the budget can be set to hold about two.
	probe := budgetManager(t, 0)
	r, err := probe.Join("probe", &fakeConn{id: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	one := fill(t, r, update)

	m := budgetManager(t, one*5/2)
	// Rooms with nobody connected, so they are all eviction candidates.
	for i := range 4 {
		name := fmt.Sprintf("doc-%d", i)
		conn := &fakeConn{id: uint64(i + 1)}
		room, err := m.Join(name, conn, "")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		fill(t, room, update)
		if err := room.Leave(conn); err != nil {
			t.Fatal(err)
		}
		// The mailbox is FIFO, so a command that answers proves the Leave in
		// front of it has been handled. A barrier rather than a sleep.
		if _, err := room.Read(nil); err != nil {
			t.Fatal(err)
		}
	}

	// The join path evicts as it goes, and the sweep catches what growth adds
	// afterwards. Both are exercised: this calls the sweep directly rather than
	// waiting for its timer.
	m.enforceBudget()
	if got := m.ResidentBytes(); got > one*5/2 {
		t.Errorf("resident bytes %d exceed the budget %d", got, one*5/2)
	}
	if m.Len() >= 4 {
		t.Errorf("%d rooms are still resident; the budget evicted nothing", m.Len())
	}
}

// A room somebody is connected to is never evicted, whatever the budget says.
// The alternative is disconnecting the person who is typing to make room for
// somebody who has just arrived.
func TestTheBudgetWillNotEvictARoomInUse(t *testing.T) {
	update := readFixture(t, "text-three-client-interleaved", "update-000.bin")
	probe := budgetManager(t, 0)
	r, err := probe.Join("probe", &fakeConn{id: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	one := fill(t, r, update)

	// A budget smaller than the one document that is already resident, so the
	// manager is genuinely over and the only candidate is busy.
	m := budgetManager(t, one-1)
	first, err := m.Join("busy", &fakeConn{id: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	fill(t, first, update)

	// The second join is over budget and nothing can be evicted, so it is
	// refused rather than served by exceeding the bound quietly.
	_, err = m.Join("newcomer", &fakeConn{id: 2}, "")
	if err == nil {
		t.Fatal("a join over budget succeeded with nothing evictable")
	}
	if err != ErrTooManyRooms {
		t.Fatalf("err = %v, want ErrTooManyRooms", err)
	}
	// And the busy room is untouched.
	if m.Len() != 1 || m.Resident("busy") == nil {
		t.Error("the room somebody was connected to was evicted")
	}
}

// Zero means unlimited, which is what a laptop running the demo wants.
func TestNoBudgetMeansNoEviction(t *testing.T) {
	m := budgetManager(t, 0)
	update := readFixture(t, "text-three-client-interleaved", "update-000.bin")
	for i := range 6 {
		r, err := m.Join(fmt.Sprintf("doc-%d", i), &fakeConn{id: uint64(i + 1)}, "")
		if err != nil {
			t.Fatal(err)
		}
		fill(t, r, update)
	}
	if m.Len() != 6 {
		t.Errorf("%d rooms resident with no budget, want 6", m.Len())
	}
}

// A room whose weight is not known yet is measured as soon as it changes,
// whatever the interval says. Without this a document that is filled right after
// it loads weighs nothing to the budget until an interval has passed - which is
// exactly when a wave of them is being opened. Found in the field.
func TestADocumentWithNoKnownWeightIsMeasuredAtOnce(t *testing.T) {
	m := budgetManager(t, 0)
	r, err := m.Join("doc", &fakeConn{id: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	r.cfg.UsageInterval = time.Hour

	now := time.Unix(1750000000, 0)
	// What load does for a brand new document: measure nothing, and record that
	// it was measured.
	r.touchDocument()
	r.measure(now)
	if r.Bytes() != 0 {
		t.Fatalf("an empty document weighs %d", r.Bytes())
	}

	// Content arrives a second later, well inside the interval.
	r.touchDocument()
	if err := r.doc.ApplyUpdate(readFixture(t, "text-three-client-interleaved", "update-000.bin")); err != nil {
		t.Fatal(err)
	}
	r.measure(now.Add(time.Second))
	if r.Bytes() == 0 {
		t.Fatal("a document filled inside the interval still weighs nothing to the budget")
	}
}

// The measurement is cached and only redone when the document has changed, so an
// idle room costs nothing to keep measured. Both halves matter: a room that
// never re-measures would report a stale figure forever.
func TestAnUnchangedDocumentIsNotRemeasured(t *testing.T) {
	m := budgetManager(t, 0)
	r, err := m.Join("doc", &fakeConn{id: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	r.cfg.UsageInterval = time.Hour

	now := time.Unix(1750000000, 0)
	fill(t, r, readFixture(t, "text-three-client-interleaved", "update-000.bin"))
	first := r.usage.measured

	// Nothing changed: the walk is skipped even when the interval has passed.
	r.measure(now.Add(2 * time.Hour))
	if !r.usage.measured.Equal(first) {
		t.Error("an unchanged document was measured again")
	}

	// Something changed, but not enough time has passed: still skipped, so a
	// document being typed into does not get walked on every keystroke.
	r.touchDocument()
	r.measure(first.Add(time.Minute))
	if !r.usage.measured.Equal(first) {
		t.Error("a document was re-measured inside its interval")
	}

	// Changed, and the interval has passed.
	r.measure(first.Add(2 * time.Hour))
	if r.usage.measured.Equal(first) {
		t.Error("a changed document was never re-measured")
	}
}
