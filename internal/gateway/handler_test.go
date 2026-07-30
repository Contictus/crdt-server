package gateway_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mesutokul/ycollab/internal/crdt"
	"github.com/mesutokul/ycollab/internal/crdt/lib0"
	"github.com/mesutokul/ycollab/internal/gateway"
	"github.com/mesutokul/ycollab/internal/protocol"
	"github.com/mesutokul/ycollab/internal/room"
)

const fixturesDir = "../../testdata/fixtures"

func readFixture(t *testing.T, parts ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{fixturesDir}, parts...)...))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newServer starts a real HTTP server with a real gateway in front of a real
// room manager. Nothing here is faked: the tests below speak the y-websocket
// byte sequence over a socket.
func newServer(t *testing.T, cfg gateway.Config) *httptest.Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	manager := room.NewManager(ctx, room.ManagerConfig{Room: room.Config{
		IdleTimeout: time.Hour,
		Logger:      quietLogger(),
	}})
	cfg.Rooms = manager
	if cfg.Logger == nil {
		cfg.Logger = quietLogger()
	}
	srv := httptest.NewServer(gateway.New(cfg))
	t.Cleanup(func() {
		srv.Close()
		cancel()
		manager.Wait()
	})
	return srv
}

// client is a minimal y-websocket-shaped client.
type client struct {
	t  *testing.T
	ws *websocket.Conn
}

func dial(t *testing.T, srv *httptest.Server, doc string) *client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, srv.URL+"/"+doc, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	return &client{t: t, ws: ws}
}

func (c *client) send(frame []byte) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageBinary, frame); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *client) recv() protocol.Message {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, data, err := c.ws.Read(ctx)
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageBinary {
		c.t.Fatalf("got %v frame, want binary", typ)
	}
	msg, err := protocol.Decode(data)
	if err != nil {
		c.t.Fatalf("decode %x: %v", data, err)
	}
	return msg
}

// expectClose reads until the connection ends and returns the close code.
func (c *client) expectClose() websocket.StatusCode {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := c.ws.Read(ctx); err != nil {
			return websocket.CloseStatus(err)
		}
	}
}

func emptyStateVector(t *testing.T) []byte {
	t.Helper()
	sv, err := crdt.NewDoc(1).EncodeStateVector()
	if err != nil {
		t.Fatal(err)
	}
	return sv
}

// The handshake over a real socket, in the order y-protocols specifies.
func TestHandshakeOverWebSocket(t *testing.T) {
	srv := newServer(t, gateway.Config{})
	c := dial(t, srv, "doc")

	c.send(protocol.WriteSyncStep1(emptyStateVector(t)))
	if msg := c.recv(); !isType[protocol.SyncStep2Message](msg) {
		t.Fatalf("first reply is %T, want SyncStep2", msg)
	}
	if msg := c.recv(); !isType[protocol.SyncStep1Message](msg) {
		t.Fatalf("second reply is %T, want SyncStep1", msg)
	}
}

func isType[T protocol.Message](msg protocol.Message) bool {
	_, ok := msg.(T)
	return ok
}

// Two connections, one document: what one writes the other must receive, and a
// late joiner must be caught up. This is the acceptance criterion in miniature.
func TestTwoClientsConverge(t *testing.T) {
	srv := newServer(t, gateway.Config{})
	a := dial(t, srv, "doc")
	a.send(protocol.WriteSyncStep1(emptyStateVector(t)))
	a.recv()
	a.recv()

	b := dial(t, srv, "doc")
	b.send(protocol.WriteSyncStep1(emptyStateVector(t)))
	b.recv()
	b.recv()

	update := readFixture(t, "text-insert-single", "state.bin")
	a.send(protocol.WriteUpdate(update))

	msg := b.recv()
	got, ok := msg.(protocol.UpdateMessage)
	if !ok {
		t.Fatalf("peer got %T, want UpdateMessage", msg)
	}
	if !bytes.Equal(got.Update, update) {
		t.Fatalf("peer got different bytes\n got %x\nwant %x", got.Update, update)
	}

	// A third client joining afterwards is caught up from the server's copy,
	// not from a replay of the traffic it missed.
	third := dial(t, srv, "doc")
	third.send(protocol.WriteSyncStep1(emptyStateVector(t)))
	step2 := third.recv().(protocol.SyncStep2Message)

	doc := crdt.NewDoc(9)
	if err := doc.ApplyUpdate(step2.Update); err != nil {
		t.Fatalf("apply: %v", err)
	}
	want := crdt.NewDoc(9)
	if err := want.ApplyUpdate(update); err != nil {
		t.Fatal(err)
	}
	gotSV, err := doc.EncodeStateVector()
	if err != nil {
		t.Fatal(err)
	}
	wantSV, err := want.EncodeStateVector()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotSV, wantSV) {
		t.Fatalf("late joiner state vector\n got %x\nwant %x", gotSV, wantSV)
	}
}

// Separate documents are separate rooms. Getting this wrong would be a data
// leak, not just a bug.
func TestDocumentsAreIsolated(t *testing.T) {
	srv := newServer(t, gateway.Config{})
	a := dial(t, srv, "doc-a")
	b := dial(t, srv, "doc-b")
	for _, c := range []*client{a, b} {
		c.send(protocol.WriteSyncStep1(emptyStateVector(t)))
		c.recv()
		c.recv()
	}

	a.send(protocol.WriteUpdate(readFixture(t, "text-insert-single", "state.bin")))

	// b must hear nothing. A short read deadline is the only way to assert an
	// absence, so this one test pays for a timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, data, err := b.ws.Read(ctx); err == nil {
		t.Fatalf("a message crossed documents: %x", data)
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAwarenessIsRelayed(t *testing.T) {
	srv := newServer(t, gateway.Config{})
	a := dial(t, srv, "doc")
	b := dial(t, srv, "doc")

	payload := singleEntry(1001, 1, `{"user":{"name":"ada"}}`)
	a.send(protocol.WriteAwareness(payload))

	msg := b.recv()
	got, ok := msg.(protocol.AwarenessMessage)
	if !ok {
		t.Fatalf("got %T, want AwarenessMessage", msg)
	}
	aw := protocol.NewAwareness()
	if _, err := aw.ApplyUpdate(got.Payload, time.Now()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if state, ok := aw.State(1001); !ok || state != `{"user":{"name":"ada"}}` {
		t.Fatalf("state %q %v", state, ok)
	}

	// queryAwareness must return everything the room knows.
	b.send(protocol.WriteQueryAwareness())
	all := b.recv().(protocol.AwarenessMessage)
	aw2 := protocol.NewAwareness()
	if _, err := aw2.ApplyUpdate(all.Payload, time.Now()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if aw2.Len() != 1 {
		t.Fatalf("query returned %d clients, want 1", aw2.Len())
	}
}

// A bad frame ends the connection that sent it and nothing else. One confused
// client must not be able to interrupt everybody's editing session.
func TestBadFrameClosesOnlyThatConnection(t *testing.T) {
	srv := newServer(t, gateway.Config{})
	good := dial(t, srv, "doc")
	good.send(protocol.WriteSyncStep1(emptyStateVector(t)))
	good.recv()
	good.recv()

	bad := dial(t, srv, "doc")
	bad.send([]byte{0x09, 0x09, 0x09})
	if code := bad.expectClose(); code != websocket.StatusProtocolError {
		t.Fatalf("close code %v, want 1002", code)
	}

	// The survivor is still being served.
	good.send(protocol.WriteSyncStep1(emptyStateVector(t)))
	if msg := good.recv(); !isType[protocol.SyncStep2Message](msg) {
		t.Fatalf("survivor got %T", msg)
	}
}

func TestTextFrameIsRejected(t *testing.T) {
	srv := newServer(t, gateway.Config{})
	c := dial(t, srv, "doc")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if code := c.expectClose(); code != websocket.StatusProtocolError {
		t.Fatalf("close code %v, want 1002", code)
	}
}

// Rejection travels over the upgraded connection as a y-protocols/auth message,
// because that is where the client reads it (y-websocket.js:84-92).
func TestAuthorizeRejectsWithPermissionDenied(t *testing.T) {
	srv := newServer(t, gateway.Config{
		Authorize: func(r *http.Request, _ string) (gateway.Grant, error) {
			if r.URL.Query().Get("token") == "good" {
				return gateway.Grant{Write: true}, nil
			}
			return gateway.Grant{}, errors.New("no such token")
		},
	})

	c := dial(t, srv, "doc?token=bad")
	msg := c.recv()
	denied, ok := msg.(protocol.PermissionDeniedMessage)
	if !ok {
		t.Fatalf("got %T, want PermissionDeniedMessage", msg)
	}
	if denied.Reason != "no such token" {
		t.Fatalf("reason %q", denied.Reason)
	}
	if code := c.expectClose(); code != websocket.StatusPolicyViolation {
		t.Fatalf("close code %v, want 1008", code)
	}

	// The same endpoint with a good token works.
	ok2 := dial(t, srv, "doc?token=good")
	ok2.send(protocol.WriteSyncStep1(emptyStateVector(t)))
	if msg := ok2.recv(); !isType[protocol.SyncStep2Message](msg) {
		t.Fatalf("authorised client got %T", msg)
	}
}

// Authorize is told which document is being opened, because a token names the
// document it is for: without this the check could only ever be "is this a
// valid token", which is a login rather than a capability.
func TestAuthorizeSeesTheDocumentName(t *testing.T) {
	seen := make(chan string, 1)
	srv := newServer(t, gateway.Config{
		Authorize: func(_ *http.Request, doc string) (gateway.Grant, error) {
			seen <- doc
			return gateway.Grant{Write: true}, nil
		},
	})
	dial(t, srv, "the-document")
	select {
	case got := <-seen:
		if got != "the-document" {
			t.Fatalf("Authorize was told %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Authorize was never called")
	}
}

// A read-only connection reads the document and is refused when it writes. The
// refusal has to reach the client before the socket closes, which is the whole
// reason the write pump drains its queue before sending the close frame.
func TestAReadOnlyConnectionIsRefusedWhenItWrites(t *testing.T) {
	srv := newServer(t, gateway.Config{
		Authorize: func(r *http.Request, _ string) (gateway.Grant, error) {
			return gateway.Grant{Write: r.URL.Query().Get("perm") == "write"}, nil
		},
	})

	// A writer puts something in the document first, so the reader has something
	// to read.
	writer := dial(t, srv, "doc?perm=write")
	writer.send(protocol.WriteSyncStep1(emptyStateVector(t)))
	writer.recv()
	writer.recv()
	writer.send(protocol.WriteUpdate(readFixture(t, "text-insert-single", "update-000.bin")))

	reader := dial(t, srv, "doc?perm=read")
	reader.send(protocol.WriteSyncStep1(emptyStateVector(t)))
	step2, ok := reader.recv().(protocol.SyncStep2Message)
	if !ok {
		t.Fatal("a read-only client did not receive the document")
	}
	if len(step2.Update) == 0 {
		t.Fatal("the document came back empty")
	}
	reader.recv() // our SyncStep1

	// The empty answer a well-behaved read-only client sends is not an attempt to
	// write, and must not disconnect it.
	reader.send(protocol.WriteSyncStep2([]byte{0, 0}))
	reader.send(protocol.WriteAwareness(singleEntry(7, 1, `{"user":"reader"}`)))

	// A real edit is refused, with a reason, and then the connection goes.
	reader.send(protocol.WriteUpdate(readFixture(t, "text-insert-single", "update-000.bin")))
	msg := reader.recv()
	denied, ok := msg.(protocol.PermissionDeniedMessage)
	if !ok {
		t.Fatalf("got %T, want PermissionDeniedMessage", msg)
	}
	if denied.Reason == "" {
		t.Fatal("the refusal carried no reason")
	}
	if code := reader.expectClose(); code != websocket.StatusPolicyViolation {
		t.Fatalf("close code %v, want 1008", code)
	}
}

func TestEmptyDocumentNameIsRejected(t *testing.T) {
	srv := newServer(t, gateway.Config{})
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404", resp.StatusCode)
	}
}

// singleEntry builds a one-client awareness payload (awareness.js:194).
func singleEntry(client, clock uint64, state string) []byte {
	e := lib0.NewEncoder()
	e.WriteVarUint(1)
	e.WriteVarUint(client)
	e.WriteVarUint(clock)
	e.WriteVarString(state)
	return e.Bytes()
}
