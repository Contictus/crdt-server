package cluster

import (
	"context"
	"errors"
	"sync"
)

// Memory is a Bus that stays inside one process.
//
// It exists so the room's fanout logic can be tested without Redis: those tests
// are about which envelopes a room publishes and what it does with the ones it
// receives, and none of that involves the network. The Redis bus has its own
// integration test against a real server.
//
// Delivery is synchronous and includes the publisher, which is what Redis
// Pub/Sub does. Synchronous is deliberate: a test that publishes and then
// asserts should not have to sleep, and the room's deliver callback is
// non-blocking by contract, so there is nothing to deadlock on.
type Memory struct {
	mu     sync.RWMutex
	subs   map[string][]*memorySub
	closed bool
}

// NewMemory returns an empty in-process bus.
func NewMemory() *Memory {
	return &Memory{subs: make(map[string][]*memorySub)}
}

type memorySub struct {
	bus     *Memory
	room    string
	deliver func(Envelope)
	once    sync.Once
}

// Publish implements Bus.
func (m *Memory) Publish(_ context.Context, room string, env Envelope) error {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return errors.New("cluster: bus is closed")
	}
	subs := append([]*memorySub(nil), m.subs[room]...)
	m.mu.RUnlock()

	// The payload is round-tripped through the wire form rather than handed over
	// as a Go value. Anything the encoding cannot carry then fails in these tests
	// instead of only under Redis.
	raw := env.Encode()
	decoded, err := Decode(raw)
	if err != nil {
		return err
	}
	for _, sub := range subs {
		sub.deliver(decoded)
	}
	return nil
}

// Subscribe implements Bus.
func (m *Memory) Subscribe(_ context.Context, room string, deliver func(Envelope)) (Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("cluster: bus is closed")
	}
	sub := &memorySub{bus: m, room: room, deliver: deliver}
	m.subs[room] = append(m.subs[room], sub)
	return sub, nil
}

func (s *memorySub) Close() error {
	s.once.Do(func() {
		m := s.bus
		m.mu.Lock()
		defer m.mu.Unlock()
		subs := m.subs[s.room]
		for i, other := range subs {
			if other == s {
				m.subs[s.room] = append(subs[:i:i], subs[i+1:]...)
				break
			}
		}
		if len(m.subs[s.room]) == 0 {
			delete(m.subs, s.room)
		}
	})
	return nil
}

// Close makes every later Publish and Subscribe fail.
func (m *Memory) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}
