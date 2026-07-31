package gateway_test

import (
	"context"
	"runtime"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mesutokul/ycollab/internal/gateway"
	"github.com/mesutokul/ycollab/internal/protocol"
)

// Every connection costs two goroutines, a read pump and a write pump, and they
// are the highest-volume goroutines in the server. A connection that ends
// without both of them ending is a leak that scales with traffic - a server
// that has served a million connections would be holding two million
// goroutines, and nothing else in this suite would notice.
//
// The connections are ended four different ways, because the ways a connection
// ends are exactly where the pumps' shutdown handshake can be got wrong.
func TestConnectionsLeaveNoGoroutinesBehind(t *testing.T) {
	srv := newServer(t, gateway.Config{})

	// One connection first, so the room exists before the baseline is taken.
	// The room's goroutine is meant to outlive its connections - that is what a
	// resident room is - and counting it as a leak is how this test failed the
	// first time it ran.
	warmup := dial(t, srv, "doc")
	warmup.send(protocol.WriteSyncStep1(emptyStateVector(t)))
	warmup.recv()
	before := settleGoroutines(t, 0)

	update := readFixture(t, "text-insert-single", "update-000.bin")
	for range 25 {
		// 1. A client that closes politely.
		a := dial(t, srv, "doc")
		a.send(protocol.WriteSyncStep1(emptyStateVector(t)))
		a.recv()
		a.send(protocol.WriteUpdate(update))
		_ = a.ws.Close(1000, "done")

		// 2. A client that vanishes without a close frame.
		b := dial(t, srv, "doc")
		b.send(protocol.WriteSyncStep1(emptyStateVector(t)))
		b.recv()
		_ = b.ws.CloseNow()

		// 3. A client the server closes for a protocol error, which goes
		//    through the drain-then-close path in the write pump.
		c := dial(t, srv, "doc")
		c.sendText(t, "this is not a binary frame")
		c.expectClose()

		// 4. A client that never says anything at all.
		d := dial(t, srv, "doc")
		_ = d.ws.CloseNow()
	}

	if after := settleGoroutines(t, before); after > before {
		t.Errorf("%d goroutines before, %d after: %d leaked\n%s",
			before, after, after-before, goroutineDump())
	}
}

func settleGoroutines(t *testing.T, want int) int {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	last := runtime.NumGoroutine()
	stable := 0
	for time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(20 * time.Millisecond)
		n := runtime.NumGoroutine()
		if want > 0 && n <= want {
			return n
		}
		if n == last {
			if stable++; stable >= 5 {
				return n
			}
		} else {
			stable, last = 0, n
		}
	}
	return runtime.NumGoroutine()
}

func goroutineDump() string {
	var b strings.Builder
	_ = pprof.Lookup("goroutine").WriteTo(&b, 1)
	return b.String()
}

// sendText sends a text frame, which y-websocket never does - the server closes
// the connection for it, and that path drains the write queue before the close
// frame goes out.
func (c *client) sendText(t *testing.T, s string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, []byte(s)); err != nil {
		t.Fatalf("write: %v", err)
	}
}
