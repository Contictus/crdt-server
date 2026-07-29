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
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

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

	Logger *slog.Logger

	// Authorize runs before the upgrade. A non-nil error rejects the
	// connection with a y-protocols/auth permission-denied message and a 1008
	// close, which is the shape Phase 5's JWT check will fill in. The error's
	// text is sent to the client, so it must not leak anything.
	Authorize func(r *http.Request) error
}

// Handler serves the WebSocket endpoint. The URL path is the document name,
// matching y-websocket's serverUrl + '/' + roomname (y-websocket.js:403-406).
type Handler struct {
	cfg    Config
	log    *slog.Logger
	nextID atomic.Uint64
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
	return &Handler{cfg: cfg, log: cfg.Logger}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.Trim(r.URL.Path, "/")
	if name == "" || strings.Contains(name, "/") {
		http.Error(w, "document name must be the whole path", http.StatusNotFound)
		return
	}

	var authErr error
	if h.cfg.Authorize != nil {
		authErr = h.cfg.Authorize(r)
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
		h.reject(r.Context(), ws, authErr)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	c := newConn(h.nextID.Add(1), ws, h.cfg.OutBuffer, cancel)
	rm, err := h.cfg.Rooms.Join(name, c)
	if err != nil {
		h.log.Warn("join failed", "room", name, "err", err)
		_ = ws.Close(websocket.StatusTryAgainLater, truncateReason(err.Error()))
		return
	}

	log := h.log.With("room", name, "conn", c.id)
	go h.writePump(ctx, c, log)
	h.readPump(ctx, c, rm, log)

	// One Leave per connection, whatever ended it.
	_ = rm.Leave(c)
	c.Close(int(websocket.StatusNormalClosure), "")
}

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
func (h *Handler) readPump(ctx context.Context, c *conn, rm *room.Room, log *slog.Logger) {
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
func (h *Handler) writePump(ctx context.Context, c *conn, log *slog.Logger) {
	ticker := time.NewTicker(h.cfg.PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			code, reason := c.closeStatus()
			_ = c.ws.Close(code, truncateReason(reason))
			return
		case frame := <-c.out:
			if err := h.write(ctx, c, frame); err != nil {
				if !isExpectedClose(err) {
					log.Debug("write failed", "err", err)
				}
				c.Close(int(websocket.StatusInternalError), "write failed")
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

func (h *Handler) write(ctx context.Context, c *conn, frame []byte) error {
	ctx, cancel := context.WithTimeout(ctx, h.cfg.WriteTimeout)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageBinary, frame)
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
