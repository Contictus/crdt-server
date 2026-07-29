package room

import (
	"context"
	"errors"
	"sync"
)

// ErrTooManyRooms means the resident-room cap is reached and every room is in
// use. It bounds worst-case memory: without a cap, one client per document name
// is enough to hold an unbounded number of documents in memory.
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
	wg    sync.WaitGroup
}

// NewManager returns a manager whose rooms stop when ctx is cancelled.
func NewManager(ctx context.Context, cfg ManagerConfig) *Manager {
	return &Manager{
		cfg:   cfg,
		ctx:   ctx,
		rooms: make(map[string]*Room),
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rooms[name]; ok {
		return r, nil
	}
	if m.cfg.MaxRooms > 0 && len(m.rooms) >= m.cfg.MaxRooms {
		return nil, ErrTooManyRooms
	}
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
	return r, nil
}

// forget is called by a room's own goroutine as it stops.
func (m *Manager) forget(name string, r *Room) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rooms[name] == r {
		delete(m.rooms, name)
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
