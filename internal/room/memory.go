package room

// What the resident documents cost, and evicting by that rather than by a count.
//
// The cap that existed before this file was -max-rooms, a count. It bounds
// nothing an operator can act on: two thousand documents is forty megabytes or
// forty gigabytes depending on what is in them, and a pod's memory limit is
// written in bytes. A count also makes eviction pick badly - the least recently
// used tiny document goes while a gigabyte one nobody has touched in an hour
// stays, because they weigh the same to a counter.
//
// # What this does not do
//
// It does not lift the ceiling. A replica serving a document holds all of it:
// sync step 2 needs the whole state, and YATA integration needs the struct
// store, so there is no partial residency to be had. What changes is that the
// ceiling is now stated in the unit the operating system uses, enforced, and
// visible on a dashboard before it is discovered by the out-of-memory killer.
//
// Raising the ceiling itself is a deployment question rather than a code one:
// route each document to one replica consistently and N replicas hold N times
// the documents rather than each holding whatever their clients happened to
// open. See the README.

import (
	"context"
	"sync/atomic"
	"time"
)

// DefaultUsageInterval is how often a room re-measures itself. The walk is
// O(structs) and costs single-digit microseconds for a document of a realistic
// size, so this is not about the cost of measuring; it is about not measuring
// documents that have not changed.
const DefaultUsageInterval = 15 * time.Second

// usage is a room's cached self-measurement, published for the manager to read.
//
// An atomic rather than a mutex because the room goroutine writes it and the
// manager reads it while holding its own lock, and taking a room's lock from
// under the registry's would be the first ordering anybody deadlocks.
type usage struct {
	bytes   atomic.Int64
	structs atomic.Int64
	// dirty is set by anything that changes the document, so an idle room is
	// measured once rather than every interval.
	dirty atomic.Bool
	// measured is when the last walk happened.
	measured time.Time
}

// Bytes is what this room's document is estimated to cost. It is a floor - see
// crdt.Usage - and it is what the memory budget is spent against.
func (r *Room) Bytes() int64 { return r.usage.bytes.Load() }

// Structs is how many CRDT structs the document holds.
func (r *Room) Structs() int64 { return r.usage.structs.Load() }

// touchDocument marks the document as changed, so the next tick re-measures.
// Called from the room goroutine after anything that integrates.
func (r *Room) touchDocument() { r.usage.dirty.Store(true) }

// measure re-walks the document if it has changed since the last look. Called
// from the room goroutine on its tick, and once at startup so a room that is
// loaded and never edited still has a figure.
func (r *Room) measure(now time.Time) {
	if !r.usage.dirty.Load() && !r.usage.measured.IsZero() {
		return
	}
	// The interval throttles re-measuring a document whose weight is already
	// known. It must not throttle learning that weight in the first place: a
	// room is measured when it loads, which for a new document is a measurement
	// of nothing, and waiting a full interval after that would leave the budget
	// blind for exactly the burst where documents are being filled.
	//
	// Found in the field, not by a test: twenty rooms under load reported zero
	// bytes for as long as the interval lasted.
	known := r.usage.bytes.Load() > 0
	if known && now.Sub(r.usage.measured) < r.usageInterval() {
		return
	}
	u := r.doc.Usage()
	r.usage.bytes.Store(u.Bytes)
	r.usage.structs.Store(int64(u.Structs))
	r.usage.measured = now
	r.usage.dirty.Store(false)
}

func (r *Room) usageInterval() time.Duration {
	if r.cfg.UsageInterval > 0 {
		return r.cfg.UsageInterval
	}
	return DefaultUsageInterval
}

// residentBytes is what every open document on this node costs. Called with the
// registry lock held.
func (m *Manager) residentBytes() int64 {
	var total int64
	for _, r := range m.rooms {
		total += r.Bytes()
	}
	return total
}

// ResidentBytes is the same figure for whoever is reporting it.
func (m *Manager) ResidentBytes() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.residentBytes()
}

// enforceBudget evicts idle rooms until the resident documents are inside the
// budget, reporting how many went.
//
// This runs on a timer, and that is not belt and braces - it is most of the
// feature. The check on the join path only fires when a *new* document is
// opened, and documents grow: a server with a stable set of rooms and people
// typing into them would sail past the budget and never be asked about it again.
// Found by a test that filled four rooms under a budget for two and watched the
// total stay over it.
//
// Nothing is evicted while somebody is connected to it. A budget that
// disconnected the person typing in order to make room would be trading a bill
// for an outage.
func (m *Manager) enforceBudget() int {
	evicted := 0
	for {
		m.mu.Lock()
		over, candidates := m.overBudget()
		if over {
			m.pressure = "budget"
		}
		m.mu.Unlock()
		if !over {
			return evicted
		}
		if !m.evictOne(candidates) {
			// Everything resident has somebody in it. Over budget and nothing
			// to do about it, which is what the gauge and the alert are for.
			return evicted
		}
		evicted++
	}
}

// sweepBudget runs enforceBudget on a timer until ctx ends.
func (m *Manager) sweepBudget(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.enforceBudget()
		}
	}
}

// overBudget reports whether the resident documents cost more than the
// configured budget, and returns the eviction candidates in least-recently-used
// order when they do. Called with the lock held.
//
// The newly created room is not yet in the map when this runs on the create
// path, which is deliberate: a document being opened is not a candidate for
// eviction, and a budget that could evict the room it just made would spin.
func (m *Manager) overBudget() (bool, []*Room) {
	if m.cfg.MaxMemory <= 0 {
		return false, nil
	}
	if m.residentBytes() <= m.cfg.MaxMemory {
		return false, nil
	}
	return true, m.byLeastRecentlyUsed()
}
