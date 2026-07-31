package protocol

// This file is inside the package on purpose. The client cap is enforced
// against a cached counter, a.present, and every other test can only see it
// through behaviour - so a counter that drifted would show up as "the cap
// refuses somebody when the room is not full", months later and in production.
//
// Drift is exactly the bug this file exists for: an earlier version of the cap
// counted map entries rather than cursors, and a full room refused newcomers
// for ten minutes after every departure. That was caught by a test that
// happened to log the count. This checks the invariant directly, after every
// operation, under a randomised sequence of everything that can touch it.

import (
	"fmt"
	"math/rand/v2"

	"github.com/mesutokul/ycollab/internal/crdt/lib0"
	"testing"
	"time"
)

// counted is the invariant: the cached counter must equal the number of entries
// that actually hold a state.
func (a *Awareness) counted() int {
	n := 0
	for _, e := range a.entries {
		if e.present {
			n++
		}
	}
	return n
}

func TestPresentCounterNeverDrifts(t *testing.T) {
	const (
		rounds  = 4000
		clients = 12
		maxCl   = 5
	)
	a := NewAwarenessWithLimits(Limits{MaxClients: maxCl, MaxState: 1 << 10})
	now := time.Unix(1750000000, 0)
	clocks := make([]uint64, clients)
	rng := rand.New(rand.NewPCG(1, 2))

	check := func(step int, what string) {
		t.Helper()
		if a.present != a.counted() {
			t.Fatalf("step %d, after %s: present=%d but %d entries hold a state",
				step, what, a.present, a.counted())
		}
		if a.present != a.Len() {
			t.Fatalf("step %d, after %s: present=%d but Len()=%d", step, what, a.present, a.Len())
		}
		if a.present > maxCl {
			t.Fatalf("step %d, after %s: %d cursors with a cap of %d", step, what, a.present, maxCl)
		}
	}

	for step := range rounds {
		now = now.Add(time.Duration(rng.IntN(3000)) * time.Millisecond)
		switch rng.IntN(6) {
		case 0, 1, 2:
			// Announce a state. Sometimes with a stale clock, which must be
			// rejected without touching the count.
			id := uint64(rng.IntN(clients)) + 1
			if rng.IntN(4) == 0 && clocks[id-1] > 0 {
				clocks[id-1]--
			} else {
				clocks[id-1]++
			}
			payload := encodeEntry(id, clocks[id-1], fmt.Sprintf(`{"c":%d}`, step))
			_, _ = a.ApplyUpdate(payload, now)
			check(step, "an announcement")
		case 3:
			// Retract one, the way a client does.
			id := uint64(rng.IntN(clients)) + 1
			clocks[id-1]++
			_, _ = a.ApplyUpdate(encodeEntry(id, clocks[id-1], NullState), now)
			check(step, "a retraction")
		case 4:
			// The room dropping a connection's clients.
			id := uint64(rng.IntN(clients)) + 1
			_, _, _ = a.RemoveClients([]uint64{id}, now)
			check(step, "RemoveClients")
		case 5:
			// The timeout sweep, which both retracts and forgets.
			_, _, _ = a.Sweep(now, 5*time.Second)
			check(step, "Sweep")
		}
	}

	// And the map itself stays bounded, which is the other half of what the
	// limits are for: remembered clocks are capped at twice the client cap.
	if got := a.Entries(); got > 2*maxCl {
		t.Errorf("%d entries remembered with a cap of %d", got, maxCl)
	}
}

// A full room must keep working for the people already in it: somebody at the
// cap can still move their cursor, and a departure must free a slot at once
// rather than after the ten-minute forget window.
func TestTheCapCountsCursorsNotMemories(t *testing.T) {
	const maxCl = 3
	a := NewAwarenessWithLimits(Limits{MaxClients: maxCl})
	now := time.Unix(1750000000, 0)

	for id := uint64(1); id <= maxCl; id++ {
		if _, err := a.ApplyUpdate(encodeEntry(id, 1, `{"x":1}`), now); err != nil {
			t.Fatalf("client %d: %v", id, err)
		}
	}
	// One more is refused.
	if _, err := a.ApplyUpdate(encodeEntry(99, 1, `{"x":1}`), now); err == nil {
		t.Fatal("a fourth client was accepted with a cap of three")
	}
	// Somebody already in the room can still move.
	if _, err := a.ApplyUpdate(encodeEntry(1, 2, `{"x":2}`), now); err != nil {
		t.Fatalf("a client already in the room was refused: %v", err)
	}
	// A departure frees a slot immediately. The entry survives as a remembered
	// clock, which is what made the first version of this hold the slot for ten
	// minutes.
	if _, _, err := a.RemoveClients([]uint64{1}, now); err != nil {
		t.Fatal(err)
	}
	if a.Entries() != maxCl {
		t.Errorf("%d entries after a departure, want the clock to be remembered", a.Entries())
	}
	if _, err := a.ApplyUpdate(encodeEntry(99, 1, `{"x":1}`), now); err != nil {
		t.Errorf("a newcomer was refused after a departure freed a slot: %v", err)
	}
}

// encodeEntry builds a one-entry awareness payload, the same shape ApplyUpdate
// reads off the wire.
func encodeEntry(client, clock uint64, state string) []byte {
	e := lib0.NewEncoder()
	e.WriteVarUint(1)
	e.WriteVarUint(client)
	e.WriteVarUint(clock)
	e.WriteVarString(state)
	return e.Bytes()
}
