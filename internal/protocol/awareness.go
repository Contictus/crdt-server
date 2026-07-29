package protocol

import (
	"errors"
	"slices"
	"time"

	"github.com/mesutokul/ycollab/internal/crdt/lib0"
)

// NullState is what y-protocols writes for a client that is gone:
// JSON.stringify(null) (awareness.js:205).
const NullState = "null"

// DefaultTimeout mirrors awareness.js:13 outdatedTimeout. In y-protocols this
// is a client-side interval; the server keeps the same number so that a client
// that vanishes is dropped on roughly the same schedule everywhere.
const DefaultTimeout = 30 * time.Second

// ErrAwarenessClockOutOfRange means a client id or clock exceeded what lib0 can
// represent. Such a value decodes here but could never be written back out, so
// it is refused on the way in (the reasoning behind D19).
var ErrAwarenessClockOutOfRange = errors.New("protocol: awareness value out of range")

// Awareness tracks the last state every client published.
//
// It is the server's copy of the shared map described in awareness.js. Two
// differences from the client implementation, both deliberate:
//
//   - The server has no local state, so the "never let a remote client remove
//     the local state" branch (awareness.js:252-257) has no counterpart here.
//   - awareness.js drops a client's state but keeps its meta clock. This does
//     the same: an entry survives removal with present=false, so a replayed
//     update at the old clock stays rejected.
//
// The published state is kept as the raw JSON string the client sent. The
// server has no business interpreting a cursor position, and parsing it would
// only create a way to mangle it on the way through.
//
// Awareness is not safe for concurrent use; the room goroutine owns it.
type Awareness struct {
	entries map[uint64]entry
}

type entry struct {
	clock       uint64
	state       string // raw JSON; meaningful only when present
	present     bool
	lastUpdated time.Time
}

// NewAwareness returns an empty tracker.
func NewAwareness() *Awareness {
	return &Awareness{entries: make(map[uint64]entry)}
}

// Len reports how many clients currently have a state.
func (a *Awareness) Len() int {
	n := 0
	for _, e := range a.entries {
		if e.present {
			n++
		}
	}
	return n
}

// Clients returns the clients that currently have a state, in ascending order.
func (a *Awareness) Clients() []uint64 {
	out := make([]uint64, 0, len(a.entries))
	for id, e := range a.entries {
		if e.present {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// State returns the raw JSON a client published.
func (a *Awareness) State(client uint64) (string, bool) {
	e, ok := a.entries[client]
	if !ok || !e.present {
		return "", false
	}
	return e.state, true
}

// ApplyUpdate applies an awareness update payload and reports which clients
// changed, in the order they appeared in the payload.
//
// The acceptance rule is awareness.js:250 exactly: take the entry when the
// clock advanced, or when the clock is unchanged, the new state is null, and we
// currently hold a state for that client - which is what makes a removal
// idempotent at equal clocks.
func (a *Awareness) ApplyUpdate(payload []byte, now time.Time) ([]uint64, error) {
	d := lib0.NewDecoder(payload)
	n, err := d.ReadVarUint()
	if err != nil {
		return nil, err
	}
	if n > uint64(d.Remaining()) {
		// Every entry costs at least three bytes, so a count larger than the
		// remaining input cannot be honest. Refusing it up front stops a
		// three-byte frame from asking for a huge allocation.
		return nil, lib0.ErrUnexpectedEOF
	}
	var changed []uint64
	for i := uint64(0); i < n; i++ {
		client, err := d.ReadVarUint()
		if err != nil {
			return changed, err
		}
		clock, err := d.ReadVarUint()
		if err != nil {
			return changed, err
		}
		state, err := d.ReadVarString()
		if err != nil {
			return changed, err
		}
		if client > lib0.MaxSafeInteger || clock > lib0.MaxSafeInteger {
			return changed, ErrAwarenessClockOutOfRange
		}
		e := a.entries[client]
		isNull := state == NullState
		if !(e.clock < clock || (e.clock == clock && isNull && e.present)) {
			continue
		}
		e.clock = clock
		e.lastUpdated = now
		e.present = !isNull
		if isNull {
			e.state = ""
		} else {
			e.state = state
		}
		a.entries[client] = e
		changed = append(changed, client)
	}
	if !d.Done() {
		return changed, ErrTrailingBytes
	}
	return changed, nil
}

// Encode writes an awareness update for the given clients, in the order given.
// A client with no state is written as null, which is how a removal travels
// (awareness.js:199-206).
func (a *Awareness) Encode(clients []uint64) ([]byte, error) {
	e := lib0.NewEncoderSize(len(clients) * 24)
	e.WriteVarUint(uint64(len(clients)))
	for _, id := range clients {
		ent := a.entries[id]
		e.WriteVarUint(id)
		e.WriteVarUint(ent.clock)
		if ent.present {
			e.WriteVarString(ent.state)
		} else {
			e.WriteVarString(NullState)
		}
	}
	if err := e.Err(); err != nil {
		return nil, err
	}
	return e.Bytes(), nil
}

// EncodeAll writes every client that currently has a state. This is the reply
// to a queryAwareness message and the greeting a joining connection gets.
func (a *Awareness) EncodeAll() ([]byte, error) {
	return a.Encode(a.Clients())
}

// Sweep removes clients that have not been heard from within ttl and returns
// the update announcing it.
//
// This is the server-side timeout that y-protocols does not have (DECISIONS
// C3). Without it a client that dies without sending its own null state stays
// in the room forever and is handed to everyone who joins afterwards, so the
// document accumulates ghost cursors.
func (a *Awareness) Sweep(now time.Time, ttl time.Duration) ([]uint64, []byte, error) {
	var stale []uint64
	for id, e := range a.entries {
		if e.present && now.Sub(e.lastUpdated) >= ttl {
			stale = append(stale, id)
		}
	}
	if len(stale) == 0 {
		return nil, nil, nil
	}
	slices.Sort(stale)
	return a.remove(stale, now)
}

// RemoveClients marks clients as gone and returns the update announcing it.
// The room calls this when the connection that owned those client ids goes
// away, so peers see the cursor disappear immediately instead of after ttl.
func (a *Awareness) RemoveClients(clients []uint64, now time.Time) ([]uint64, []byte, error) {
	var present []uint64
	for _, id := range clients {
		if e, ok := a.entries[id]; ok && e.present {
			present = append(present, id)
		}
	}
	if len(present) == 0 {
		return nil, nil, nil
	}
	return a.remove(present, now)
}

// remove drops the given clients and encodes the announcement.
//
// The clock is deliberately left alone. A removal is accepted by peers at an
// equal clock (awareness.js:250), so bumping is unnecessary - and actively
// harmful: the client whose state we just dropped does not know we bumped it,
// so its next announcement, one clock ahead of what it last sent, would land on
// an equal clock here and be rejected. It would then stay invisible until its
// own 15 s renewal pushed it past us. A reconnect must not cost a client half a
// minute of being a ghost. y-protocols does the same thing for the same reason:
// removeAwarenessStates only bumps the clock of the *local* client
// (awareness.js:175-181), never of the peers it is dropping.
func (a *Awareness) remove(clients []uint64, now time.Time) ([]uint64, []byte, error) {
	for _, id := range clients {
		e := a.entries[id]
		e.present = false
		e.state = ""
		e.lastUpdated = now
		a.entries[id] = e
	}
	payload, err := a.Encode(clients)
	if err != nil {
		return nil, nil, err
	}
	return clients, payload, nil
}
