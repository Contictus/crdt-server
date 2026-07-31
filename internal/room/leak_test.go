package room

import (
	"context"
	"runtime"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"github.com/mesutokul/ycollab/internal/cluster"
	"github.com/mesutokul/ycollab/internal/protocol"
)

// A room starts up to three goroutines of its own - the actor, the writer and
// the publisher - and the shutdown has to end all three. Nothing else in the
// suite checks that: every other test either drives the actor by hand or lets
// the process exit, and a leaked goroutine is invisible either way until a
// server that has served a thousand documents is holding a thousand writers.
//
// This is the check. It exercises the full shape - store, bus, hooks - because
// the shutdown ordering between them is where a leak would live.
func TestRoomsLeaveNoGoroutinesBehind(t *testing.T) {
	before := settle(t, 0)

	bus := cluster.NewMemory()
	sink := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	m := NewManager(ctx, ManagerConfig{Room: Config{
		IdleTimeout: time.Hour,
		Tick:        5 * time.Millisecond,
		Store:       &fakeStore{},
		Versions:    &fakeVersions{},
		// Short enough that the version path runs during the test rather than
		// being a code path nothing exercised.
		VersionInterval: 5 * time.Millisecond,
		FlushInterval:   5 * time.Millisecond,
		Bus:             bus,
		NodeID:          7,
		AntiEntropy:     5 * time.Millisecond,
		Hooks:           sink,
		Logger:          quietLogger(),
	}})

	// Twenty documents, each with a connection that actually sends something,
	// so every goroutine has had work to do before it is asked to stop.
	updates := scenarioUpdates(t, "text-three-client-interleaved")
	for i := range 20 {
		name := "doc-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		c := &fakeConn{id: uint64(i + 1)}
		if _, err := m.Join(name, c, ""); err != nil {
			t.Fatalf("join %s: %v", name, err)
		}
		r := m.Resident(name)
		if r == nil {
			t.Fatalf("%s is not resident after a join", name)
		}
		for _, u := range updates[:3] {
			if err := r.Deliver(c, protocol.WriteUpdate(u)); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Let the tick, the flush, the publisher and the version path all run.
	time.Sleep(100 * time.Millisecond)

	cancel()
	m.Wait()
	if err := bus.Close(); err != nil {
		t.Fatalf("closing the bus: %v", err)
	}

	after := settle(t, before)
	if after > before {
		t.Errorf("%d goroutines before, %d after: %d leaked\n%s",
			before, after, after-before, dumpGoroutines())
	}
}

// A room that fails to start - the document will not load - must clean up as
// thoroughly as one that ran, or a database outage leaks a goroutine per
// attempt and the leak outlives the outage.
func TestARoomThatCannotLoadLeavesNothingBehind(t *testing.T) {
	before := settle(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(ctx, ManagerConfig{Room: Config{
		IdleTimeout: time.Hour,
		Store:       &fakeStore{loadErr: errLoad},
		Logger:      quietLogger(),
	}})

	for i := range 20 {
		c := &fakeConn{id: uint64(i + 1)}
		// The join itself succeeds or fails depending on how far the room got;
		// either is fine. What matters is what is left running.
		_, _ = m.Join("doc-"+string(rune('a'+i)), c, "")
	}
	m.Wait()

	if after := settle(t, before); after > before {
		t.Errorf("%d goroutines before, %d after: %d leaked\n%s",
			before, after, after-before, dumpGoroutines())
	}
}

var errLoad = errTest("the database is down")

type errTest string

func (e errTest) Error() string { return string(e) }

// settle waits for the goroutine count to stop moving, so the check is not a
// race against goroutines that are already on their way out. want is the count
// it is hoping for; reaching it ends the wait early.
func settle(t *testing.T, want int) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	last := runtime.NumGoroutine()
	stable := 0
	for time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(20 * time.Millisecond)
		n := runtime.NumGoroutine()
		if want > 0 && n <= want {
			return n
		}
		if n == last {
			if stable++; stable >= 5 {
				return n
			}
		} else {
			stable, last = 0, n
		}
	}
	return runtime.NumGoroutine()
}

// dumpGoroutines is what makes a failure actionable: the count says something
// leaked, the stacks say what.
func dumpGoroutines() string {
	var b strings.Builder
	_ = pprof.Lookup("goroutine").WriteTo(&b, 1)
	return b.String()
}
