// Package room owns documents. One goroutine per document, and that goroutine
// is the only thing that touches the document's CRDT state, so there is no
// locking anywhere in this package's hot path - which is exactly the promise
// internal/crdt's Doc was written against.
//
// The room speaks the y-websocket message flow described in
// y-protocols/sync.js:23-28: the client opens with SyncStep1, the server
// answers with SyncStep2 and then its own SyncStep1, and everything after that
// is Update messages and awareness.
package room

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/mesutokul/ycollab/internal/crdt"
	"github.com/mesutokul/ycollab/internal/protocol"
	"github.com/mesutokul/ycollab/internal/store"
)

// ErrClosed means the room stopped between the moment a caller looked it up and
// the moment it tried to hand over a connection. The manager retries with a
// fresh room; see registry.go.
var ErrClosed = errors.New("room: closed")

// Defaults chosen in the brief.
const (
	// DefaultMailbox bounds how many commands can be waiting for the room
	// goroutine. Producers block when it is full, which pushes back on the
	// connections that are shouting, not on the document.
	DefaultMailbox = 256
	// DefaultTick is how often the room checks awareness timeouts and its own
	// idleness. Well below the 30 s awareness timeout so a ghost cursor is
	// never more than a tick late.
	DefaultTick = 5 * time.Second
	// DefaultIdleTimeout is how long an empty room stays resident.
	DefaultIdleTimeout = 5 * time.Minute
)

// Config configures one room.
type Config struct {
	Name string

	// ServerClientID is the client id the room's Doc is created with. The
	// server never authors content, so this only matters if a later phase makes
	// it edit documents itself.
	ServerClientID crdt.ClientID

	Mailbox      int
	Tick         time.Duration
	IdleTimeout  time.Duration
	AwarenessTTL time.Duration

	// Store persists the document. Nil keeps everything in memory, which is
	// what the room's own unit tests want and what the server does when it is
	// started without a database.
	Store Persistence
	// CompactAfter is how many updates accumulate before the log is folded into
	// a new snapshot.
	CompactAfter int
	// FlushInterval bounds how long an update sits in memory before it is
	// written.
	FlushInterval time.Duration

	Logger *slog.Logger

	// Now is the clock, injectable so tests do not have to sleep.
	Now func() time.Time

	// OnEvict runs when an idle room is about to disappear, before it stops and
	// before its final snapshot is written. Persistence does not go through it -
	// the room writes its own snapshot - so this is a hook for observers.
	OnEvict func(name string, doc *crdt.Doc)

	// OnExit removes the room from whatever registry owns it. It must have
	// completed before the room accepts no further joins; the manager relies on
	// that ordering to avoid losing a connection to a room that is going away.
	OnExit func(name string)
}

func (c *Config) setDefaults() {
	if c.Mailbox <= 0 {
		c.Mailbox = DefaultMailbox
	}
	if c.Tick <= 0 {
		c.Tick = DefaultTick
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = DefaultIdleTimeout
	}
	if c.AwarenessTTL <= 0 {
		c.AwarenessTTL = protocol.DefaultTimeout
	}
	if c.CompactAfter <= 0 {
		c.CompactAfter = DefaultCompactAfter
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = DefaultFlushInterval
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(discard{}, &slog.HandlerOptions{Level: slog.LevelError}))
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// A Room is one document and the connections currently editing it.
type Room struct {
	cfg Config
	log *slog.Logger

	doc   *crdt.Doc
	aw    *protocol.Awareness
	docID store.UUID

	// jobs carries work to the persist goroutine; nil when there is no store.
	// Only the room goroutine touches it.
	jobs        chan persistJob
	persistDone chan struct{}
	// sinceSnapshot counts updates queued since the last compaction (room
	// goroutine); watermark is the highest seq written (persist goroutine).
	sinceSnapshot int
	watermark     int64

	conns map[Conn]*connState

	mailbox chan command
	done    chan struct{}

	// sendMu and closed serialise the shutdown handover; see send and stop.
	sendMu sync.Mutex
	closed bool

	// lastEmpty is when the room last had no connections, or the zero time
	// while somebody is connected.
	lastEmpty time.Time
}

// connState is what the room remembers about a connection beyond its identity.
type connState struct {
	// clients are the awareness client ids this connection has published under.
	// A connection is normally one Yjs client, but nothing in the protocol says
	// so, and a departing connection has to retract everything it announced.
	clients map[uint64]struct{}
}

// New creates a room. It does not start it; call Run.
func New(cfg Config) *Room {
	cfg.setDefaults()
	r := &Room{
		cfg:     cfg,
		log:     cfg.Logger.With("room", cfg.Name),
		doc:     crdt.NewDoc(cfg.ServerClientID),
		aw:      protocol.NewAwareness(),
		docID:   store.DocumentID(cfg.Name),
		conns:   make(map[Conn]*connState),
		mailbox: make(chan command, cfg.Mailbox),
		done:    make(chan struct{}),
	}
	if cfg.Store != nil {
		r.jobs = make(chan persistJob, persistQueue)
		r.persistDone = make(chan struct{})
	}
	r.lastEmpty = cfg.Now()
	return r
}

// Name reports the room's document name.
func (r *Room) Name() string { return r.cfg.Name }

// Done is closed once the room has stopped.
func (r *Room) Done() <-chan struct{} { return r.done }

type command interface{ isCommand() }

type joinCmd struct{ conn Conn }

// evictCmd asks the room to shut down if nobody is connected. The reply says
// whether it did, so the registry can move on to the next candidate instead of
// guessing.
type evictCmd struct{ evicted chan bool }

type leaveCmd struct{ conn Conn }
type frameCmd struct {
	conn  Conn
	frame []byte
}
type tickCmd struct{ now time.Time }

func (joinCmd) isCommand()  {}
func (evictCmd) isCommand() {}
func (leaveCmd) isCommand() {}
func (frameCmd) isCommand() {}
func (tickCmd) isCommand()  {}

// Join hands a connection to the room.
func (r *Room) Join(conn Conn) error { return r.send(joinCmd{conn}) }

// Leave tells the room a connection is gone. The gateway calls it exactly once
// per connection, whatever the reason for the disconnect.
func (r *Room) Leave(conn Conn) error { return r.send(leaveCmd{conn}) }

// Deliver hands one client frame to the room. frame must not be reused by the
// caller: the room keeps slices of it and broadcasts them unchanged.
func (r *Room) Deliver(conn Conn, frame []byte) error { return r.send(frameCmd{conn, frame}) }

// send blocks while the mailbox is full. That is deliberate: it stalls the
// connection that is producing faster than the document can integrate, which is
// the only party that should pay for it.
//
// sendMu is not there to protect the channel - channels do not need that - but
// to make the handover at shutdown total. A command either reaches the room, or
// is drained by stop, or is refused with ErrClosed; it is never accepted and
// then dropped on the floor. See stop for the other half of the argument.
func (r *Room) send(c command) error {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	if r.closed {
		return ErrClosed
	}
	select {
	case r.mailbox <- c:
		return nil
	case <-r.done:
		// The room stopped while we were waiting for mailbox space.
		return ErrClosed
	}
}

// Run is the room's goroutine. It returns when the room evicts itself or ctx is
// cancelled.
func (r *Room) Run(ctx context.Context) {
	if r.cfg.Store != nil {
		if err := r.load(ctx); err != nil {
			// Serving an empty document under a name that has content would let
			// clients merge their state into a blank one and write it back. A
			// closed connection is recoverable; that is not.
			r.log.Error("could not load document", "err", err)
			r.jobs = nil
			close(r.persistDone)
			r.stop(CloseInternalError, "could not load document")
			return
		}
		jobs := r.jobs
		go r.persist(jobs)
	}

	ticker := time.NewTicker(r.cfg.Tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.stop(CloseGoingAway, "server shutting down")
			return
		case c := <-r.mailbox:
			if evicted := r.handle(c); evicted {
				return
			}
		case <-ticker.C:
			if evicted := r.handle(tickCmd{r.cfg.Now()}); evicted {
				return
			}
		}
	}
}

// handle applies one command. It reports whether the room evicted itself.
//
// Tests drive this directly, so the actor's behaviour is checked without a
// goroutine, a real clock or a sleep anywhere.
func (r *Room) handle(c command) bool {
	switch v := c.(type) {
	case joinCmd:
		r.join(v.conn)
	case leaveCmd:
		r.leave(v.conn)
	case frameCmd:
		r.frame(v.conn, v.frame)
	case tickCmd:
		return r.tick(v.now)
	case evictCmd:
		if len(r.conns) > 0 {
			v.evicted <- false
			return false
		}
		r.evict()
		v.evicted <- true
		return true
	}
	return false
}

// EvictIfIdle asks the room to stop, and reports whether it did. A room with
// connections refuses: freeing memory by disconnecting people who are editing
// is not a trade the registry is allowed to make.
func (r *Room) EvictIfIdle() bool {
	reply := make(chan bool, 1)
	if err := r.send(evictCmd{reply}); err != nil {
		// Already gone, which is what the caller wanted.
		return true
	}
	select {
	case ok := <-reply:
		return ok
	case <-r.done:
		return true
	}
}

func (r *Room) join(conn Conn) {
	if _, ok := r.conns[conn]; ok {
		return
	}
	r.conns[conn] = &connState{clients: make(map[uint64]struct{})}
	r.lastEmpty = time.Time{}
	// Nothing is sent here. The protocol is client-initiated (sync.js:23-28):
	// the client opens with SyncStep1 and we answer it. Sending first would
	// work with y-websocket but would be us inventing a handshake.
}

func (r *Room) leave(conn Conn) {
	state, ok := r.conns[conn]
	if !ok {
		return
	}
	delete(r.conns, conn)
	if len(r.conns) == 0 {
		r.lastEmpty = r.cfg.Now()
	}
	// Retract whatever this connection announced, so its cursor disappears now
	// rather than after the awareness timeout.
	if len(state.clients) > 0 {
		ids := make([]uint64, 0, len(state.clients))
		for id := range state.clients {
			ids = append(ids, id)
		}
		_, payload, err := r.aw.RemoveClients(ids, r.cfg.Now())
		if err != nil {
			r.log.Error("encode awareness removal", "err", err)
		} else if payload != nil {
			r.broadcast(protocol.WriteAwareness(payload), nil)
		}
	}
}

func (r *Room) frame(conn Conn, frame []byte) {
	if _, ok := r.conns[conn]; !ok {
		return // raced with a disconnect
	}
	msg, err := protocol.Decode(frame)
	if err != nil {
		r.log.Warn("undecodable frame", "conn", conn.ID(), "err", err)
		r.drop(conn, CloseProtocolError, "undecodable message")
		return
	}
	switch v := msg.(type) {
	case protocol.SyncStep1Message:
		r.syncStep1(conn, v.StateVector)
	case protocol.SyncStep2Message:
		r.update(conn, v.Update)
	case protocol.UpdateMessage:
		r.update(conn, v.Update)
	case protocol.AwarenessMessage:
		r.awareness(conn, v.Payload)
	case protocol.QueryAwarenessMessage:
		r.sendAll(conn)
	case protocol.PermissionDeniedMessage:
		// Server to client only. A client sending one is confused, but it is
		// harmless, so log and carry on rather than dropping a live editor.
		r.log.Warn("client sent an auth message", "conn", conn.ID(), "reason", v.Reason)
	}
}

// syncStep1 answers a client's state vector with what it is missing, then asks
// for what we are missing (sync.js:23-28).
func (r *Room) syncStep1(conn Conn, sv []byte) {
	diff, err := r.doc.EncodeDiff(sv)
	if err != nil {
		r.log.Warn("bad state vector", "conn", conn.ID(), "err", err)
		r.drop(conn, CloseProtocolError, "bad state vector")
		return
	}
	if !r.deliver(conn, protocol.WriteSyncStep2(diff)) {
		return
	}
	ours, err := r.doc.EncodeStateVector()
	if err != nil {
		r.log.Error("encode state vector", "err", err)
		return
	}
	if !r.deliver(conn, protocol.WriteSyncStep1(ours)) {
		return
	}
	// Hand over the cursors that are already in the room. Without this a
	// joining client sees nobody until they next move.
	if r.aw.Len() > 0 {
		r.sendAll(conn)
	}
}

func (r *Room) sendAll(conn Conn) {
	payload, err := r.aw.EncodeAll()
	if err != nil {
		r.log.Error("encode awareness", "err", err)
		return
	}
	r.deliver(conn, protocol.WriteAwareness(payload))
}

func (r *Room) update(conn Conn, update []byte) {
	if err := r.doc.ApplyUpdate(update); err != nil {
		r.log.Warn("bad update", "conn", conn.ID(), "err", err)
		r.drop(conn, CloseProtocolError, "bad update")
		return
	}
	r.record(update)
	// Relay the sender's bytes rather than re-encoding the document's delta.
	// Peers then receive exactly what the author produced, and updates for
	// structs we are still missing propagate instead of being stuck behind our
	// own integration state.
	r.broadcast(protocol.WriteUpdate(update), conn)
}

func (r *Room) awareness(conn Conn, payload []byte) {
	changed, err := r.aw.ApplyUpdate(payload, r.cfg.Now())
	if err != nil {
		r.log.Warn("bad awareness update", "conn", conn.ID(), "err", err)
		r.drop(conn, CloseProtocolError, "bad awareness update")
		return
	}
	if len(changed) == 0 {
		return
	}
	if state, ok := r.conns[conn]; ok {
		for _, id := range changed {
			state.clients[id] = struct{}{}
		}
	}
	out, err := r.aw.Encode(changed)
	if err != nil {
		r.log.Error("encode awareness", "err", err)
		return
	}
	r.broadcast(protocol.WriteAwareness(out), conn)
}

// tick sweeps stale awareness states and evicts the room if it has been empty
// long enough. It reports whether the room evicted itself.
func (r *Room) tick(now time.Time) bool {
	if _, payload, err := r.aw.Sweep(now, r.cfg.AwarenessTTL); err != nil {
		r.log.Error("sweep awareness", "err", err)
	} else if payload != nil {
		r.broadcast(protocol.WriteAwareness(payload), nil)
	}
	if len(r.conns) > 0 || r.lastEmpty.IsZero() {
		return false
	}
	if now.Sub(r.lastEmpty) < r.cfg.IdleTimeout {
		return false
	}
	r.evict()
	return true
}

// evict writes the document out and shuts the room down.
func (r *Room) evict() {
	r.log.Info("evicting room")
	if r.cfg.OnEvict != nil {
		r.cfg.OnEvict(r.cfg.Name, r.doc)
	}
	r.stop(CloseGoingAway, "room evicted")
}

// broadcast sends a frame to every connection except except.
func (r *Room) broadcast(frame []byte, except Conn) {
	var slow []Conn
	for conn := range r.conns {
		if conn == except {
			continue
		}
		if !conn.Send(frame) {
			slow = append(slow, conn)
		}
	}
	// Dropping mutates r.conns, so it happens after the iteration.
	for _, conn := range slow {
		r.dropSlow(conn)
	}
}

// deliver sends one frame to one connection, reporting whether it went out.
func (r *Room) deliver(conn Conn, frame []byte) bool {
	if conn.Send(frame) {
		return true
	}
	r.dropSlow(conn)
	return false
}

func (r *Room) dropSlow(conn Conn) {
	r.log.Warn("dropping slow connection", "conn", conn.ID())
	r.drop(conn, ClosePolicyViolation, "outbound buffer full")
}

// drop removes a connection and closes it. The gateway will still call Leave
// when its pumps notice; leave is idempotent.
func (r *Room) drop(conn Conn, code int, reason string) {
	r.leave(conn)
	conn.Close(code, reason)
}

// stop closes every connection and shuts the room down.
//
// The order is the whole correctness argument:
//
//  1. OnExit removes the room from the registry, so no new caller can find it.
//  2. done is closed, which releases any sender blocked on a full mailbox.
//  3. sendMu is taken and closed is set, which stops senders that had already
//     got past the registry. Taking the lock has to come after step 2, or a
//     sender blocked on the mailbox while holding it would deadlock us.
//  4. Whatever is still queued is drained. Any sender that enqueued before
//     step 3 held the lock, so its command is in the mailbox by now and this
//     drain sees it. A joining connection is closed with 1001 rather than
//     silently forgotten: the client reconnects and lands in a fresh room.
func (r *Room) stop(code int, reason string) {
	for conn := range r.conns {
		conn.Close(code, reason)
	}
	r.conns = make(map[Conn]*connState)
	if r.cfg.OnExit != nil {
		r.cfg.OnExit(r.cfg.Name)
	}
	// Persist before signalling that we are gone, so a caller that waits for
	// the room knows the document is on disk.
	r.finishPersisting()
	close(r.done)
	r.sendMu.Lock()
	r.closed = true
	r.sendMu.Unlock()
	for {
		select {
		case c := <-r.mailbox:
			if j, ok := c.(joinCmd); ok {
				// Same code the connected clients got: a joiner that arrived
				// during the handover deserves the same explanation, not a
				// generic "going away" when the real reason was that the
				// document could not be loaded.
				j.conn.Close(code, reason)
			}
		default:
			return
		}
	}
}
