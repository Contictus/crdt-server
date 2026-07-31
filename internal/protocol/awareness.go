package protocol

import (
	"errors"
	"fmt"
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

// forgetAfter is how long a removed client's clock is remembered before its
// entry is dropped entirely.
//
// The clock has to outlive the state, or a replayed update would resurrect a
// cursor that was deliberately removed. But it cannot be kept forever: a Yjs
// client picks a new random id for every Y.Doc, so every reconnect leaves one
// behind, and a room that stays resident for days would accumulate an entry per
// reconnect - a slow leak in exactly the rooms that matter most.
//
// Ten minutes is far past any in-flight duplicate and far short of a working
// day. The worst case if a genuinely ancient update arrives afterwards is one
// ghost cursor, which the sweep removes on its next pass.
const forgetAfter = 10 * time.Minute

// ErrAwarenessClockOutOfRange means a client id or clock exceeded what lib0 can
// represent. Such a value decodes here but could never be written back out, so
// it is refused on the way in (the reasoning behind D19).
var ErrAwarenessClockOutOfRange = errors.New("protocol: awareness value out of range")

var (
	// ErrStateTooLarge means a client published a state larger than the room
	// allows. An awareness state is a cursor: a name, a colour and a couple of
	// offsets. Anything much bigger is either a client with a bug or a client
	// using the awareness channel as free broadcast storage - and either way it
	// is held in memory here and relayed to every peer and every replica, which
	// makes it the cheapest amplification this server offers.
	ErrStateTooLarge = errors.New("protocol: awareness state is too large")
	// ErrTooManyClients means the room already tracks as many clients as it is
	// willing to. Client ids are chosen by the client, so one connection can
	// invent as many as it likes, and each one costs an entry that is broadcast
	// to everybody.
	ErrTooManyClients = errors.New("protocol: too many awareness clients")
)

// Limits bound what an Awareness will hold.
type Limits struct {
	// MaxState is the largest state a client may publish, in bytes. Zero means
	// DefaultMaxState; negative means no limit.
	MaxState int
	// MaxClients is how many clients one room will track. Zero means
	// DefaultMaxClients; negative means no limit.
	MaxClients int
}

const (
	// DefaultMaxState is generous next to what a cursor actually costs: a
	// y-prosemirror selection with a user name and colour is a few hundred
	// bytes, so this is twenty times the real thing and still small enough that
	// a full room cannot exhaust memory with it.
	DefaultMaxState = 4 << 10
	// DefaultMaxClients bounds the entries one document can accumulate. Together
	// with DefaultMaxState it caps a room's awareness at a few megabytes, which
	// is what makes the cap a memory bound rather than a gesture.
	DefaultMaxClients = 1024
)

func (l Limits) maxState() int {
	if l.MaxState == 0 {
		return DefaultMaxState
	}
	return l.MaxState
}

func (l Limits) maxClients() int {
	if l.MaxClients == 0 {
		return DefaultMaxClients
	}
	return l.MaxClients
}

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
	limits  Limits
	// present counts the entries that currently hold a state. The cap is on
	// these rather than on the map: an entry whose state was removed is a
	// remembered clock, not a cursor, and holding a slot for it would mean a
	// full room refusing newcomers for the ten minutes after somebody left.
	present int
}

type entry struct {
	clock       uint64
	state       string // raw JSON; meaningful only when present
	present     bool
	lastUpdated time.Time
}

// NewAwareness returns an empty tracker with the default limits.
func NewAwareness() *Awareness { return NewAwarenessWithLimits(Limits{}) }

// NewAwarenessWithLimits returns an empty tracker bounded by limits.
func NewAwarenessWithLimits(limits Limits) *Awareness {
	return &Awareness{entries: make(map[uint64]entry), limits: limits}
}

// Entries reports how many clients are remembered at all, including those whose
// state was removed but whose clock is still held. Len counts only the ones with
// a state; the difference is what forgetAfter bounds.
func (a *Awareness) Entries() int { return len(a.entries) }

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
		e, known := a.entries[client]
		isNull := state == NullState

		// A removal is always allowed through, whatever the limits say: refusing
		// one would leave a cursor on screen that its owner has retracted.
		if !isNull {
			if max := a.limits.maxState(); max > 0 && len(state) > max {
				return changed, fmt.Errorf("%w: %d bytes, limit %d", ErrStateTooLarge, len(state), max)
			}
			// The cap is on *new* clients only. A full room must keep working for
			// the people already in it, and client ids are chosen by the client,
			// so this is the only place the count can be held down.
			if max := a.limits.maxClients(); max > 0 && (!known || !e.present) && a.present >= max {
				return changed, fmt.Errorf("%w: %d", ErrTooManyClients, a.present)
			}
		}
		if !(e.clock < clock || (e.clock == clock && isNull && e.present)) {
			continue
		}
		if !known && a.limits.maxClients() > 0 {
			// Remembered clocks are bounded too, or a client cycling through ids -
			// announce, retract, announce another - would grow the map for the
			// whole forgetAfter window. The oldest one goes; it is the one least
			// likely to still have a duplicate in flight.
			a.forgetOldest(2 * a.limits.maxClients())
		}
		e.clock = clock
		e.lastUpdated = now
		if e.present != !isNull {
			if isNull {
				a.present--
			} else {
				a.present++
			}
		}
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
		if e.present {
			if now.Sub(e.lastUpdated) >= ttl {
				stale = append(stale, id)
			}
			continue
		}
		// Already removed, and long enough ago that nothing in flight can still
		// refer to it. Dropping the entry is what keeps a long-lived room from
		// growing an entry per reconnect; see forgetAfter.
		if now.Sub(e.lastUpdated) >= forgetAfter {
			delete(a.entries, id)
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

// forgetOldest drops removed entries until the map is under limit.
func (a *Awareness) forgetOldest(limit int) {
	for len(a.entries) >= limit {
		var oldest uint64
		var at time.Time
		found := false
		for id, e := range a.entries {
			if e.present {
				continue
			}
			if !found || e.lastUpdated.Before(at) {
				oldest, at, found = id, e.lastUpdated, true
			}
		}
		if !found {
			return
		}
		delete(a.entries, oldest)
	}
}

// remove is the shared half of Sweep and RemoveClients; see the note above
// forgetOldest for why the clock is left where it is.
func (a *Awareness) remove(clients []uint64, now time.Time) ([]uint64, []byte, error) {
	for _, id := range clients {
		e := a.entries[id]
		if e.present {
			a.present--
		}
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
