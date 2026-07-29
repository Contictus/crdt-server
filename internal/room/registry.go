package room

import (
	"context"
	"errors"
	"sort"
	"sync"
)

// ErrTooManyRooms means the resident-room cap is reached and every resident
// room has somebody connected to it, so none of them can be evicted to make
// space. The cap bounds worst-case memory: without one, a client per document
// name is enough to hold an unbounded number of documents.
var ErrTooManyRooms = errors.New("room: resident room limit reached")

// ManagerConfig configures the registry and the rooms it creates.
type ManagerConfig struct {
	// Room is the template every room is created from. Name, OnExit and Logger
	// are filled in per room.
	Room Config
	// MaxRooms caps resident rooms. Zero means unlimited.
	MaxRooms int
}

// A Manager owns the set of resident rooms and starts one goroutine per room.
//
// This is the only place in the package with a lock, and it guards nothing but
// the name-to-room map.
type Manager struct {
	cfg ManagerConfig
	ctx context.Context

	mu    sync.Mutex
	rooms map[string]*Room
	// used orders rooms for the LRU cap: the value is a counter, not a clock,
	// so two joins in the same nanosecond still order.
	used  map[string]uint64
	clock uint64
	wg    sync.WaitGroup
}

// NewManager returns a manager whose rooms stop when ctx is cancelled.
func NewManager(ctx context.Context, cfg ManagerConfig) *Manager {
	return &Manager{
		cfg:   cfg,
		ctx:   ctx,
		rooms: make(map[string]*Room),
		used:  make(map[string]uint64),
	}
}

// Join places conn in the named room, creating and starting the room if needed.
//
// A room can decide to evict itself at any moment, including between the lookup
// and the handover. The room's shutdown removes itself from this map before it
// starts refusing joins, so observing ErrClosed proves the map no longer holds
// that room and one retry is enough.
func (m *Manager) Join(name string, conn Conn) (*Room, error) {
	for {
		r, err := m.get(name)
		if err != nil {
			return nil, err
		}
		switch err := r.Join(conn); {
		case err == nil:
			return r, nil
		case errors.Is(err, ErrClosed):
			continue
		default:
			return nil, err
		}
	}
}

func (m *Manager) get(name string) (*Room, error) {
	for {
		r, candidates, err := m.tryGet(name)
		if err != nil {
			return nil, err
		}
		if r != nil {
			return r, nil
		}
		// At the cap. Evict the least recently used idle room and try again;
		// with persistence in place that costs a snapshot write, not the
		// document. Done outside the lock, because evicting waits for the room
		// to write itself out.
		if !m.evictOne(candidates) {
			return nil, ErrTooManyRooms
		}
	}
}

// tryGet returns the room, or the eviction candidates in least-recently-used
// order when the cap is in the way.
func (m *Manager) tryGet(name string) (*Room, []*Room, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rooms[name]; ok {
		m.touch(name)
		return r, nil, nil
	}
	if m.cfg.MaxRooms > 0 && len(m.rooms) >= m.cfg.MaxRooms {
		names := make([]string, 0, len(m.rooms))
		for n := range m.rooms {
			names = append(names, n)
		}
		sort.Slice(names, func(i, j int) bool { return m.used[names[i]] < m.used[names[j]] })
		rooms := make([]*Room, 0, len(names))
		for _, n := range names {
			rooms = append(rooms, m.rooms[n])
		}
		return nil, rooms, nil
	}
	m.touch(name)
	cfg := m.cfg.Room
	cfg.Name = name
	r := New(cfg)
	// The room's exit hook removes this exact room, never whatever happens to
	// be registered under the name later. Set after New and before Run, so no
	// goroutine has looked at the config yet.
	r.cfg.OnExit = func(string) { m.forget(name, r) }
	m.rooms[name] = r
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		r.Run(m.ctx)
	}()
	return r, nil, nil
}

// touch records that a room was just used. Called with the lock held.
func (m *Manager) touch(name string) {
	m.clock++
	m.used[name] = m.clock
}

// evictOne stops the first idle room in the list, reporting whether any went.
func (m *Manager) evictOne(candidates []*Room) bool {
	for _, r := range candidates {
		if r.EvictIfIdle() {
			<-r.Done()
			return true
		}
	}
	return false
}

// forget is called by a room's own goroutine as it stops.
func (m *Manager) forget(name string, r *Room) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rooms[name] == r {
		delete(m.rooms, name)
		delete(m.used, name)
	}
}

// Len reports how many rooms are resident.
func (m *Manager) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rooms)
}

// Wait blocks until every room goroutine has returned. Callers cancel the
// context first; this is the drain half of a graceful shutdown.
func (m *Manager) Wait() { m.wg.Wait() }
