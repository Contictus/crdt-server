// Package gateway is the WebSocket edge: it accepts connections, hands each one
// to the room that owns the requested document, and runs the two pumps per
// connection that the brief calls for.
//
// It knows nothing about CRDTs. Frames go to the room untouched and come back
// ready to write.
package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/mesutokul/ycollab/internal/metrics"
	"github.com/mesutokul/ycollab/internal/protocol"
	"github.com/mesutokul/ycollab/internal/room"
)

const (
	// DefaultReadLimit caps one inbound frame. A SyncStep2 for a large
	// document is legitimately megabytes, so this is generous; it exists to
	// stop a single frame from being a memory attack, not to police documents.
	DefaultReadLimit = 16 << 20
	// DefaultWriteTimeout bounds one write. A peer that cannot absorb a frame
	// within this is not going to catch up.
	DefaultWriteTimeout = 10 * time.Second
	// DefaultPingInterval is how often an idle connection is pinged, so dead
	// peers are noticed rather than accumulating.
	DefaultPingInterval = 20 * time.Second
	// maxCloseReason is the WebSocket limit on a close reason (RFC 6455 says
	// the whole control frame payload is at most 125 bytes, two of which are
	// the status code).
	maxCloseReason = 123
)

// Config configures the handler.
type Config struct {
	Rooms *room.Manager

	// Origins are the allowed Origin header patterns. Empty means same-origin
	// only, which is what coder/websocket does by default.
	Origins []string

	ReadLimit    int64
	WriteTimeout time.Duration
	PingInterval time.Duration
	OutBuffer    int

	// Rate bounds how fast one connection may send. The zero value applies the
	// defaults, which are generous; a negative rate switches a dimension off.
	Rate Rate

	// MaxConns caps how many connections this node serves at once. Zero means
	// no cap, which is the right default for a laptop and the wrong one for
	// anything reachable: a connection costs two goroutines, a 256-frame queue
	// and a room, and nothing else in the server bounds how many there are.
	// Beyond the cap the upgrade is refused with 1013, which tells a
	// y-websocket client to come back rather than to give up.
	MaxConns int

	// MaxConnsPerIP caps how many connections one client address may hold.
	// Zero means no cap. Without it, MaxConns is a limit one address can reach
	// on its own, which turns "the node is full" into something a single client
	// decides.
	//
	// It stays off by default, unlike the rate limits, and the reason is NAT: an
	// office, a school or a mobile carrier is one address to this server, so a
	// live default would be a live default on how many colleagues may edit at
	// once. The deployment knows its clients; the Kubernetes manifests set it.
	MaxConnsPerIP int

	// Proxies are the addresses whose X-Forwarded-For header is believed. The
	// zero value trusts none, so the socket's peer address is used. See
	// clientip.go for why this must be opt-in.
	Proxies Proxies

	// Metrics is where connection counts and traffic are reported. Nil becomes
	// a set registered nowhere, so this package never checks for one.
	Metrics *metrics.Metrics

	Logger *slog.Logger

	// Authorize decides whether this request may open this document, and with
	// what permission. A non-nil error rejects the connection with a
	// y-protocols/auth permission-denied message and a 1008 close. The error's
	// text is sent to the client, so it must not leak anything.
	//
	// Nil means every connection is allowed to read and write, which is what a
	// server started without a signing secret does.
	//
	// ip is the client address as this handler resolved it, honouring Proxies.
	// It is passed rather than left to be re-derived from the request, so that
	// what an external authorization endpoint is told about the client is the
	// same address this server rate-limits and logs. Two answers to "who is
	// this" is one too many.
	Authorize func(r *http.Request, doc, ip string) (Grant, error)
}

// A Grant is what Authorize decided.
type Grant struct {
	// Subject identifies the client for logging. It may be empty.
	Subject string
	// Write says whether the connection may send document updates.
	Write bool
	// Owner is the tenant this connection belongs to. Empty means it claims
	// none, which is what a server without multi-tenancy grants and what a
	// token with no owner claim gets.
	//
	// A document is stamped with the owner of whoever opened it first, and
	// every later connection must match. That is the whole tenancy boundary:
	// without it, knowing a document name is enough to open it.
	Owner string
}

// Handler serves the WebSocket endpoint. The URL path is the document name,
// matching y-websocket's serverUrl + '/' + roomname (y-websocket.js:403-406).
type Handler struct {
	cfg     Config
	log     *slog.Logger
	metrics *metrics.Metrics
	nextID  atomic.Uint64
	// open counts connections currently being served, for MaxConns.
	open atomic.Int64

	// perIP counts connections by client address, for MaxConnsPerIP. It is a
	// map rather than a counter because the question is per address, and it is
	// pruned to zero so an address that goes away leaves nothing behind - the
	// alternative is a map that grows with every address that ever connected.
	ipMu  sync.Mutex
	perIP map[string]int
}

// New returns a handler. Rooms is required.
func New(cfg Config) *Handler {
	if cfg.ReadLimit <= 0 {
		cfg.ReadLimit = DefaultReadLimit
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = DefaultWriteTimeout
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = DefaultPingInterval
	}
	if cfg.OutBuffer <= 0 {
		cfg.OutBuffer = DefaultOutBuffer
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Metrics == nil {
		cfg.Metrics = metrics.Nop()
	}
	return &Handler{
		cfg:     cfg,
		log:     cfg.Logger,
		metrics: cfg.Metrics,
		perIP:   make(map[string]int),
	}
}

// claimIP reserves a slot for one address, reporting whether there was one.
func (h *Handler) claimIP(ip string) bool {
	if h.cfg.MaxConnsPerIP <= 0 {
		return true
	}
	h.ipMu.Lock()
	defer h.ipMu.Unlock()
	if h.perIP[ip] >= h.cfg.MaxConnsPerIP {
		return false
	}
	h.perIP[ip]++
	return true
}

func (h *Handler) releaseIP(ip string) {
	if h.cfg.MaxConnsPerIP <= 0 {
		return
	}
	h.ipMu.Lock()
	defer h.ipMu.Unlock()
	if n := h.perIP[ip] - 1; n > 0 {
		h.perIP[ip] = n
	} else {
		delete(h.perIP, ip)
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.Trim(r.URL.Path, "/")
	if name == "" || strings.Contains(name, "/") {
		http.Error(w, "document name must be the whole path", http.StatusNotFound)
		return
	}

	ip := h.cfg.Proxies.ClientIP(r)

	// The caps are claimed before the upgrade and released when the handler
	// returns, so a connection that is refused never counts against them.
	if h.cfg.MaxConns > 0 {
		if n := h.open.Add(1); n > int64(h.cfg.MaxConns) {
			h.open.Add(-1)
			h.metrics.Denied.WithLabelValues("too_many_connections").Inc()
			h.log.Warn("refusing a connection: at the limit", "limit", h.cfg.MaxConns, "ip", ip)
			http.Error(w, "too many connections", http.StatusServiceUnavailable)
			return
		}
		defer h.open.Add(-1)
	}
	// The per-address cap comes second, so an address that is over its own
	// limit is reported as that rather than as the node being full.
	if !h.claimIP(ip) {
		h.metrics.Denied.WithLabelValues("too_many_connections_per_ip").Inc()
		h.log.Warn("refusing a connection: address at its limit", "limit", h.cfg.MaxConnsPerIP, "ip", ip)
		http.Error(w, "too many connections from this address", http.StatusServiceUnavailable)
		return
	}
	defer h.releaseIP(ip)

	// Authorising before the upgrade means a rejected request never reaches a
	// room, so a bad token cannot even create one.
	grant := Grant{Write: true}
	var authErr error
	if h.cfg.Authorize != nil {
		grant, authErr = h.cfg.Authorize(r, name, ip)
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: h.cfg.Origins,
	})
	if err != nil {
		h.log.Warn("websocket upgrade failed", "err", err)
		return
	}
	ws.SetReadLimit(h.cfg.ReadLimit)

	// The rejection has to travel over the upgraded connection: that is where
	// y-websocket's client reads it (y-websocket.js:84-92).
	if authErr != nil {
		// The reason is the label, and it is a label rather than the error text
		// because the text can name a document: a metric with a document name in
		// it is a metric with unbounded cardinality.
		h.metrics.Denied.WithLabelValues("unauthorized").Inc()
		// The address is on this line and not in the metric: it is what an
		// operator needs to answer "who is doing this", and a label would make
		// the metric's cardinality the number of addresses on the internet.
		h.log.Warn("refusing a connection: unauthorized", "room", name, "ip", ip)
		h.reject(r.Context(), ws, authErr)
		return
	}

	h.metrics.ConnectionsTotal.Inc()
	h.metrics.ConnectionsOpen.Inc()
	defer h.metrics.ConnectionsOpen.Dec()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	c := newConn(h.nextID.Add(1), ws, h.cfg.OutBuffer, grant.Write, cancel)
	rm, err := h.cfg.Rooms.Join(name, c, grant.Owner)
	if err != nil {
		// A document that belongs to another tenant is refused the same way a
		// bad token is, and with the same words. Telling the two apart would
		// make the boundary a way to ask "does this name exist somewhere else
		// on this server", one guess at a time.
		if errors.Is(err, room.ErrWrongOwner) {
			h.metrics.Denied.WithLabelValues("unauthorized").Inc()
			h.log.Warn("refusing a connection: the document belongs to another owner",
				"room", name, "ip", ip, "owner", grant.Owner)
			h.reject(r.Context(), ws, errNotYours)
			return
		}
		// The process is going down and this connection arrived a moment too
		// late. It is a routine part of a deploy, not a fault, so it closes the
		// same way but without a warning line per in-flight client.
		if errors.Is(err, room.ErrManagerClosed) {
			h.log.Debug("refusing a connection: shutting down", "room", name)
			_ = ws.Close(websocket.StatusTryAgainLater, "server shutting down")
			return
		}
		h.log.Warn("join failed", "room", name, "err", err)
		_ = ws.Close(websocket.StatusTryAgainLater, truncateReason(err.Error()))
		return
	}

	log := h.log.With("room", name, "conn", c.id, "ip", ip, "sub", grant.Subject,
		"write", grant.Write, "owner", grant.Owner)
	go h.writePump(c, log)
	h.readPump(ctx, c, rm, newLimiter(h.cfg.Rate, nil), log)

	// One Leave per connection, whatever ended it.
	_ = rm.Leave(c)
	c.Close(int(websocket.StatusNormalClosure), "")
	// Wait for the close frame to go out before the handler returns and the
	// hijacked socket is torn down under the write pump.
	<-c.finished
}

// errNotYours is what a client is told when the document belongs to somebody
// else. Deliberately the same sentence a missing or wrong token produces.
var errNotYours = errors.New("not authorized for this document")

func (h *Handler) reject(ctx context.Context, ws *websocket.Conn, cause error) {
	ctx, cancel := context.WithTimeout(ctx, h.cfg.WriteTimeout)
	defer cancel()
	frame := protocol.WritePermissionDenied(cause.Error())
	if err := ws.Write(ctx, websocket.MessageBinary, frame); err != nil {
		h.log.Debug("could not deliver permission denied", "err", err)
	}
	_ = ws.Close(websocket.StatusPolicyViolation, truncateReason(cause.Error()))
}

// readPump owns reading. It returns when the peer goes away, the room closes
// the connection, or the server shuts down.
func (h *Handler) readPump(ctx context.Context, c *conn, rm *room.Room, limit *limiter, log *slog.Logger) {
	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			if !isExpectedClose(err) {
				log.Debug("read ended", "err", err)
			}
			return
		}
		if typ != websocket.MessageBinary {
			// y-websocket only ever sends binary. A text frame means something
			// else is talking to us, and guessing at its meaning is worse than
			// saying so.
			log.Warn("non-binary frame")
			c.Close(room.CloseProtocolError, "binary frames only")
			return
		}
		h.metrics.BytesReceived.Add(float64(len(data)))

		// Over the limit this waits rather than closing, which stops us reading
		// and lets TCP slow the sender down. Only this connection is affected:
		// the room is never blocked on it.
		if waited, err := limit.wait(ctx, len(data)); err != nil {
			log.Debug("throttling ended with the connection", "err", err)
			return
		} else if waited > 0 {
			h.metrics.Throttled.Inc()
			h.metrics.ThrottledSeconds.Add(waited.Seconds())
			log.Debug("throttled", "waited", waited)
		}

		// data is freshly allocated per read, so the room may keep slices of it
		// and broadcast them without a copy.
		if err := rm.Deliver(c, data); err != nil {
			log.Debug("room stopped accepting", "err", err)
			return
		}
	}
}

// writePump owns the socket for writing: every frame, every ping and the close
// handshake go through here, so there is never more than one writer.
//
// It deliberately does not watch the request context. That context is cancelled
// as part of closing the connection, and a write pump that raced it would
// abandon the socket before the close frame went out - leaving the peer with an
// abrupt disconnect instead of the reason it was disconnected for. The pump
// ends when c.done says so, and its writes are bounded by their own timeouts.
func (h *Handler) writePump(c *conn, log *slog.Logger) {
	defer close(c.finished)
	defer func() {
		// Whatever is already queued goes out before the close frame. The room
		// closes a connection by queueing an explanation and then closing it -
		// a permission-denied for a read-only client that tried to write, for
		// instance - and without this drain the select below would race, and the
		// client would be disconnected without being told why. The queue is
		// bounded and each write has its own deadline, so this cannot hang.
		h.drain(c, log)
		// The close frame goes out next; only then is the reader unblocked.
		// The other order loses the close code: cancelling a read context makes
		// coder/websocket drop the connection there and then.
		code, reason := c.closeStatus()
		h.metrics.CloseCode(int(code))
		_ = c.ws.Close(code, truncateReason(reason))
		c.cancel()
	}()

	ctx := context.Background()
	ticker := time.NewTicker(h.cfg.PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case frame := <-c.out:
			if err := h.write(ctx, c, frame); err != nil {
				// A failed write is the transport breaking, not this server
				// failing to serve the document, and the close code never
				// reaches the peer anyway - the socket is already gone. What it
				// does reach is the metric, so recording 1011 here put "internal
				// errors" on the dashboard every time somebody closed a tab.
				// 1011 is kept for the case it describes: a document we could not
				// serve. Found by watching the close-code metric during a load
				// run, not by a test.
				log.Debug("write failed", "err", err)
				c.Close(int(websocket.StatusGoingAway), "write failed")
				return
			}
		case <-ticker.C:
			pctx, cancel := context.WithTimeout(ctx, h.cfg.WriteTimeout)
			err := c.ws.Ping(pctx)
			cancel()
			if err != nil {
				log.Debug("ping failed", "err", err)
				c.Close(int(websocket.StatusGoingAway), "ping timeout")
				return
			}
		}
	}
}

// drain writes whatever is still queued, giving up at the first failure: if the
// socket is gone, the frames after it are not going to make it either.
func (h *Handler) drain(c *conn, log *slog.Logger) {
	for {
		select {
		case frame := <-c.out:
			if err := h.write(context.Background(), c, frame); err != nil {
				if !isExpectedClose(err) {
					log.Debug("could not flush a queued frame", "err", err)
				}
				return
			}
		default:
			return
		}
	}
}

func (h *Handler) write(ctx context.Context, c *conn, frame []byte) error {
	ctx, cancel := context.WithTimeout(ctx, h.cfg.WriteTimeout)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageBinary, frame); err != nil {
		return err
	}
	h.metrics.BytesSent.Add(float64(len(frame)))
	return nil
}

// isExpectedClose reports whether an error is just the connection ending.
func isExpectedClose(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		return true
	}
	return false
}

// truncateReason keeps a close reason inside the control-frame budget. A close
// frame that is too long is a protocol error, which would replace a clean
// shutdown with an abrupt one - and so is one that ends mid-rune, since the
// reason must be valid UTF-8.
func truncateReason(reason string) string {
	if len(reason) <= maxCloseReason {
		return reason
	}
	cut := reason[:maxCloseReason]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}
