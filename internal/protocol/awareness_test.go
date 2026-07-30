package protocol_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/mesutokul/ycollab/internal/crdt/lib0"
	"github.com/mesutokul/ycollab/internal/protocol"
)

// singleEntry builds a one-client awareness payload the way a client would
// (awareness.js:194), so tests can express states the Awareness type would not
// produce on its own.
func singleEntry(client, clock uint64, state string) []byte {
	e := lib0.NewEncoder()
	e.WriteVarUint(1)
	e.WriteVarUint(client)
	e.WriteVarUint(clock)
	e.WriteVarString(state)
	return e.Bytes()
}

type awarenessFixture struct {
	Updates []struct {
		File   string `json:"file"`
		States []struct {
			ClientID uint64          `json:"clientID"`
			Clock    uint64          `json:"clock"`
			State    json.RawMessage `json:"state"`
		} `json:"states"`
	} `json:"updates"`
}

func loadAwarenessFixture(t *testing.T) awarenessFixture {
	t.Helper()
	var f awarenessFixture
	raw := readFixture(t, filepath.Join(fixturesDir, "awareness", "expected.json"))
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("expected.json: %v", err)
	}
	if len(f.Updates) == 0 {
		t.Fatal("no awareness updates in expected.json")
	}
	return f
}

// jsonEqual compares two JSON documents by value, not by text: key order is
// whatever JSON.stringify produced and carries no meaning.
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("unmarshal %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	return reflect.DeepEqual(x, y)
}

// Every awareness payload y-protocols produced must decode to the states
// expected.json describes, and re-encode to the same bytes. The state JSON is
// relayed verbatim, so byte identity here is the property that matters: a
// re-serialised cursor with reordered keys would still be correct, but it would
// no longer be the client's bytes.
func TestAwarenessFixtures(t *testing.T) {
	now := time.Unix(1700000000, 0)
	for _, u := range loadAwarenessFixture(t).Updates {
		raw := readFixture(t, filepath.Join(fixturesDir, "awareness", u.File))
		aw := protocol.NewAwareness()
		changed, err := aw.ApplyUpdate(raw, now)
		if err != nil {
			t.Fatalf("%s: apply: %v", u.File, err)
		}
		if len(changed) != len(u.States) {
			t.Fatalf("%s: %d clients changed, want %d", u.File, len(changed), len(u.States))
		}
		for i, want := range u.States {
			if changed[i] != want.ClientID {
				t.Fatalf("%s: changed[%d] = %d, want %d", u.File, i, changed[i], want.ClientID)
			}
			got, present := aw.State(want.ClientID)
			isNull := string(want.State) == "null"
			if present == isNull {
				t.Fatalf("%s: client %d present=%v, state was %s", u.File, want.ClientID, present, want.State)
			}
			if !isNull && !jsonEqual(t, []byte(got), want.State) {
				t.Fatalf("%s: client %d state\n got %s\nwant %s", u.File, want.ClientID, got, want.State)
			}
		}
		again, err := aw.Encode(changed)
		if err != nil {
			t.Fatalf("%s: encode: %v", u.File, err)
		}
		if !bytes.Equal(again, raw) {
			t.Fatalf("%s: re-encode differs\n got %x\nwant %x", u.File, again, raw)
		}
	}
}

// mustApply applies a payload and asserts exactly wantChanged clients changed,
// in that order.
func mustApply(t *testing.T, aw *protocol.Awareness, payload []byte, now time.Time, wantChanged ...uint64) {
	t.Helper()
	changed, err := aw.ApplyUpdate(payload, now)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(changed) != len(wantChanged) {
		t.Fatalf("changed %v, want %v", changed, wantChanged)
	}
	for i := range changed {
		if changed[i] != wantChanged[i] {
			t.Fatalf("changed %v, want %v", changed, wantChanged)
		}
	}
}

// The clock rules are the whole protocol: get them wrong and cursors either
// flicker back to stale positions or never disappear. Transcribed from
// awareness.js:250.
func TestAwarenessClockRules(t *testing.T) {
	now := time.Unix(1700000000, 0)

	t.Run("advancing clock is accepted", func(t *testing.T) {
		aw := protocol.NewAwareness()
		mustApply(t, aw, singleEntry(7, 1, `{"a":1}`), now, 7)
		mustApply(t, aw, singleEntry(7, 2, `{"a":2}`), now, 7)
		if got, _ := aw.State(7); got != `{"a":2}` {
			t.Fatalf("state %q", got)
		}
	})

	t.Run("stale clock is ignored", func(t *testing.T) {
		aw := protocol.NewAwareness()
		mustApply(t, aw, singleEntry(7, 5, `{"a":1}`), now, 7)
		mustApply(t, aw, singleEntry(7, 4, `{"a":9}`), now)
		if got, _ := aw.State(7); got != `{"a":1}` {
			t.Fatalf("state %q", got)
		}
	})

	t.Run("equal clock with a state is ignored", func(t *testing.T) {
		aw := protocol.NewAwareness()
		mustApply(t, aw, singleEntry(7, 5, `{"a":1}`), now, 7)
		mustApply(t, aw, singleEntry(7, 5, `{"a":9}`), now)
		if got, _ := aw.State(7); got != `{"a":1}` {
			t.Fatalf("state %q", got)
		}
	})

	t.Run("equal clock removal is accepted once", func(t *testing.T) {
		aw := protocol.NewAwareness()
		mustApply(t, aw, singleEntry(7, 5, `{"a":1}`), now, 7)
		mustApply(t, aw, singleEntry(7, 5, protocol.NullState), now, 7)
		if _, present := aw.State(7); present {
			t.Fatal("client still present after removal")
		}
		// Idempotent: the same removal replayed changes nothing, which is what
		// makes duplicate delivery over fanout safe.
		mustApply(t, aw, singleEntry(7, 5, protocol.NullState), now)
	})

	t.Run("removal does not reset the clock", func(t *testing.T) {
		aw := protocol.NewAwareness()
		mustApply(t, aw, singleEntry(7, 5, `{"a":1}`), now, 7)
		mustApply(t, aw, singleEntry(7, 5, protocol.NullState), now, 7)
		// A replay of the state that was removed must not resurrect it.
		mustApply(t, aw, singleEntry(7, 5, `{"a":1}`), now)
		if _, present := aw.State(7); present {
			t.Fatal("removed client resurrected by a stale state")
		}
	})
}

func TestAwarenessSweepRemovesSilentClients(t *testing.T) {
	start := time.Unix(1700000000, 0)
	aw := protocol.NewAwareness()
	mustApply(t, aw, singleEntry(7, 3, `{"a":1}`), start, 7)
	mustApply(t, aw, singleEntry(8, 1, `{"b":1}`), start.Add(20*time.Second), 8)

	removed, _, err := aw.Sweep(start.Add(29*time.Second), protocol.DefaultTimeout)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != nil {
		t.Fatalf("swept too early: %v", removed)
	}

	removed, payload, err := aw.Sweep(start.Add(31*time.Second), protocol.DefaultTimeout)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(removed) != 1 || removed[0] != 7 {
		t.Fatalf("removed %v, want [7]", removed)
	}
	if _, present := aw.State(7); present {
		t.Fatal("swept client still present")
	}
	if _, present := aw.State(8); !present {
		t.Fatal("swept a client that was still talking")
	}

	// Peers accept the removal at the client's own clock, because they hold a
	// state for it and the equal-clock null rule applies (awareness.js:250).
	peer := protocol.NewAwareness()
	mustApply(t, peer, singleEntry(7, 3, `{"a":1}`), start, 7)
	mustApply(t, peer, payload, start, 7)
	if _, present := peer.State(7); present {
		t.Fatal("peer did not accept the sweep removal")
	}
}

func TestAwarenessRemoveClients(t *testing.T) {
	now := time.Unix(1700000000, 0)
	aw := protocol.NewAwareness()
	mustApply(t, aw, singleEntry(7, 3, `{"a":1}`), now, 7)

	removed, payload, err := aw.RemoveClients([]uint64{7, 99}, now)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(removed) != 1 || removed[0] != 7 {
		t.Fatalf("removed %v, want [7]", removed)
	}
	if _, present := aw.State(7); present {
		t.Fatal("client still present")
	}

	peer := protocol.NewAwareness()
	mustApply(t, peer, singleEntry(7, 3, `{"a":1}`), now, 7)
	mustApply(t, peer, payload, now, 7)
	if _, present := peer.State(7); present {
		t.Fatal("peer did not accept the removal")
	}

	// A reconnecting client announces itself one clock past what it last sent.
	// If the removal had bumped our clock, that announcement would land on an
	// equal clock and be rejected, and the client would stay a ghost until its
	// own 15 s renewal pushed it past us.
	mustApply(t, aw, singleEntry(7, 4, `{"a":2}`), now, 7)
	if state, present := aw.State(7); !present || state != `{"a":2}` {
		t.Fatalf("reconnecting client was not accepted back: %q %v", state, present)
	}

	// Removing a client we never knew is a no-op, not an empty broadcast.
	removed, payload, err = aw.RemoveClients([]uint64{99}, now)
	if err != nil || removed != nil || payload != nil {
		t.Fatalf("removing an unknown client: %v %v %v", removed, payload, err)
	}
}

func TestAwarenessEncodeAllCarriesEveryLiveClient(t *testing.T) {
	now := time.Unix(1700000000, 0)
	aw := protocol.NewAwareness()
	mustApply(t, aw, singleEntry(9, 1, `{"a":1}`), now, 9)
	mustApply(t, aw, singleEntry(2, 1, `{"b":1}`), now, 2)
	mustApply(t, aw, singleEntry(5, 1, `{"c":1}`), now, 5)
	mustApply(t, aw, singleEntry(5, 2, protocol.NullState), now, 5)

	if got := aw.Clients(); !reflect.DeepEqual(got, []uint64{2, 9}) {
		t.Fatalf("clients %v", got)
	}
	all, err := aw.EncodeAll()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	peer := protocol.NewAwareness()
	mustApply(t, peer, all, now, 2, 9)
	if aw.Len() != 2 || peer.Len() != 2 {
		t.Fatalf("len %d/%d", aw.Len(), peer.Len())
	}
}

func TestAwarenessRejects(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cases := []struct {
		name string
		in   []byte
		want error
	}{
		{"count larger than input", []byte{0x7f, 0x01}, nil},
		{"truncated entry", []byte{0x01, 0x01, 0x01}, nil},
		{"trailing bytes", append(singleEntry(1, 1, "null"), 0x00), protocol.ErrTrailingBytes},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			aw := protocol.NewAwareness()
			if _, err := aw.ApplyUpdate(c.in, now); err == nil {
				t.Fatalf("accepted %x", c.in)
			} else if c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

func FuzzApplyAwarenessNeverPanics(f *testing.F) {
	f.Add(singleEntry(1, 1, `{"a":1}`))
	f.Add(singleEntry(1, 1, "null"))
	f.Add([]byte{0x00})
	f.Fuzz(func(t *testing.T, data []byte) {
		now := time.Unix(1700000000, 0)
		aw := protocol.NewAwareness()
		changed, err := aw.ApplyUpdate(data, now)
		if err != nil {
			return
		}
		// Whatever was accepted must be re-encodable and must survive the trip
		// to a peer unchanged - that is all the room ever does with it.
		payload, err := aw.Encode(changed)
		if err != nil {
			t.Fatalf("accepted an update it cannot re-encode: %v", err)
		}
		peer := protocol.NewAwareness()
		if _, err := peer.ApplyUpdate(payload, now); err != nil {
			t.Fatalf("peer rejected a relayed update: %v", err)
		}
		for _, id := range changed {
			mine, minePresent := aw.State(id)
			theirs, theirsPresent := peer.State(id)
			if mine != theirs || minePresent != theirsPresent {
				t.Fatalf("client %d diverged: %q/%v vs %q/%v", id, mine, minePresent, theirs, theirsPresent)
			}
		}
	})
}

// A removed entry keeps its clock so a replayed update cannot resurrect the
// cursor - but not forever. A Yjs client picks a new id for every Y.Doc, so
// every reconnect leaves one behind, and a room that stays resident for days
// would grow an entry per reconnect.
func TestRemovedEntriesAreEventuallyForgotten(t *testing.T) {
	now := time.Unix(1700000000, 0)
	a := protocol.NewAwareness()
	if _, err := a.ApplyUpdate(singleEntry(1001, 5, `{"user":"ada"}`), now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.RemoveClients([]uint64{1001}, now); err != nil {
		t.Fatal(err)
	}

	// While the clock is remembered, a replay at that clock is refused.
	changed, err := a.ApplyUpdate(singleEntry(1001, 5, `{"user":"ada"}`), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Fatal("a replayed update resurrected a removed cursor")
	}
	if n := a.Entries(); n != 1 {
		t.Fatalf("holding %d entries, want the removed one to still be remembered", n)
	}

	// A sweep well after the removal drops it.
	if _, _, err := a.Sweep(now.Add(11*time.Minute), protocol.DefaultTimeout); err != nil {
		t.Fatal(err)
	}
	if n := a.Entries(); n != 0 {
		t.Fatalf("holding %d entries after the grace period, want 0", n)
	}
}

// Sweeping must not forget a client that is still there: a live cursor is
// refreshed on every announcement, and only silence should remove it.
func TestSweepKeepsLiveEntries(t *testing.T) {
	now := time.Unix(1700000000, 0)
	a := protocol.NewAwareness()
	if _, err := a.ApplyUpdate(singleEntry(1001, 1, `{"user":"ada"}`), now); err != nil {
		t.Fatal(err)
	}
	stale, _, err := a.Sweep(now.Add(time.Second), protocol.DefaultTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("swept %v while it was still fresh", stale)
	}
	if n := a.Entries(); n != 1 {
		t.Fatalf("holding %d entries, want 1", n)
	}
}
