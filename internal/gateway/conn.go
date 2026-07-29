package gateway

import (
	"context"
	"sync"

	"github.com/coder/websocket"
)

// DefaultOutBuffer is the per-connection outbound queue depth from the brief.
// It is not a tuning knob so much as a policy: 256 frames is enough to absorb a
// burst, and a client that falls further behind is better served by a reconnect
// and a diff than by a server that keeps buying it memory.
const DefaultOutBuffer = 256

// conn is one WebSocket connection, presented to the room as a room.Conn.
//
// Everything the room calls - Send and Close - must be non-blocking, because
// the room goroutine is shared by every editor of the document. So Send drops
// into a bounded channel and reports failure instead of waiting, and Close only
// records the intent and wakes the write pump.
type conn struct {
	id  uint64
	ws  *websocket.Conn
	out chan []byte

	closeOnce sync.Once
	done      chan struct{}
	// cancel unblocks the read pump, which is otherwise parked in ws.Read.
	cancel context.CancelFunc

	mu     sync.Mutex
	code   websocket.StatusCode
	reason string
}

func newConn(id uint64, ws *websocket.Conn, buffer int, cancel context.CancelFunc) *conn {
	if buffer <= 0 {
		buffer = DefaultOutBuffer
	}
	return &conn{
		id:     id,
		ws:     ws,
		out:    make(chan []byte, buffer),
		done:   make(chan struct{}),
		cancel: cancel,
		code:   websocket.StatusNormalClosure,
	}
}

func (c *conn) ID() uint64 { return c.id }

// Send queues a frame, reporting false when the outbound buffer is full.
func (c *conn) Send(frame []byte) bool {
	select {
	case <-c.done:
		// Already closing. Reporting success keeps the room from treating a
		// disconnect as backpressure and logging a slow client that was merely
		// gone.
		return true
	case c.out <- frame:
		return true
	default:
		return false
	}
}

// Close records why the connection is going away and wakes both pumps. The
// close frame itself is written by the write pump, so no two goroutines ever
// write to the socket.
func (c *conn) Close(code int, reason string) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.code, c.reason = websocket.StatusCode(code), reason
		c.mu.Unlock()
		close(c.done)
		c.cancel()
	})
}

func (c *conn) closeStatus() (websocket.StatusCode, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.code, c.reason
}
