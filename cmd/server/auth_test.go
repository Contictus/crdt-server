package main_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"

	"github.com/mesutokul/ycollab/internal/auth"
	"github.com/mesutokul/ycollab/internal/crdt"
	"github.com/mesutokul/ycollab/internal/protocol"
)

// The Phase 5 acceptance criterion, against a real server process: a request
// with no token, a bad token or a token for another document never sees the
// document, and a read-only token reads it but cannot change it.
//
// This one needs no Docker: authorisation happens before a room exists, so
// there is nothing to persist and nothing to fan out.
const testSecret = "a-test-secret-that-is-long-enough-for-hs256"

func mintToken(t *testing.T, doc string, perm auth.Permission, ttl time.Duration) string {
	t.Helper()
	token, err := auth.Sign([]byte(testSecret), auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "test",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
		},
		Doc:  doc,
		Perm: perm,
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return token
}

// startAuthServer starts a server that requires signed tokens.
func startAuthServer(t *testing.T) *server {
	t.Helper()
	return startServer(t, buildServer(t), freePort(t), "", "-jwt-secret", testSecret)
}

// dialRaw connects without failing the test on a rejected upgrade, because some
// of these connections are meant to be refused.
func dialRaw(t *testing.T, addr, path string) *client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws://"+addr+"/"+path, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	return &client{t: t, ws: ws}
}

// expectDenied asserts the connection is refused with a permission-denied
// message and a 1008 close, which is the shape y-websocket's client reads
// (y-websocket.js:84-92).
func (c *client) expectDenied(t *testing.T) string {
	t.Helper()
	msg := c.recv()
	denied, ok := msg.(protocol.PermissionDeniedMessage)
	if !ok {
		t.Fatalf("got %T, want PermissionDeniedMessage", msg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err := c.ws.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
		t.Fatalf("close status %v, want 1008", got)
	}
	return denied.Reason
}

func TestAConnectionWithoutAGoodTokenSeesNothing(t *testing.T) {
	srv := startAuthServer(t)
	doc := fmt.Sprintf("secret-%d", time.Now().UnixNano())

	cases := []struct {
		name string
		path string
	}{
		{"no token at all", doc},
		{"not a token", doc + "?token=hello"},
		{"signed with another key", doc + "?token=" + signedElsewhere(t, doc)},
		{"expired", doc + "?token=" + mintToken(t, doc, auth.PermissionWrite, -time.Hour)},
		{"minted for another document", doc + "?token=" + mintToken(t, "another-document", auth.PermissionWrite, time.Hour)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := dialRaw(t, srv.addr, tc.path)
			if reason := c.expectDenied(t); reason == "" {
				t.Fatal("the refusal carried no reason")
			}
		})
	}
}

// signedElsewhere mints a structurally valid token with a key the server does
// not have.
func signedElsewhere(t *testing.T, doc string) string {
	t.Helper()
	token, err := auth.Sign([]byte("a-completely-different-secret-of-sufficient-length"), auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
		Doc:              doc,
		Perm:             auth.PermissionWrite,
	})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// The other half: a write token works, and a read token reads but is refused
// when it writes.
func TestAReadTokenReadsAndCannotWrite(t *testing.T) {
	srv := startAuthServer(t)
	doc := fmt.Sprintf("perms-%d", time.Now().UnixNano())
	update := readFixture(t, "text-insert-single", "update-000.bin")

	writer := dialRaw(t, srv.addr, doc+"?token="+mintToken(t, doc, auth.PermissionWrite, time.Hour))
	writer.sync()
	writer.send(protocol.WriteUpdate(update))

	want := crdt.NewDoc(9)
	if err := want.ApplyUpdate(update); err != nil {
		t.Fatal(err)
	}

	reader := dialRaw(t, srv.addr, doc+"?token="+mintToken(t, doc, auth.PermissionRead, time.Hour))
	got := reader.sync()
	if textOf(t, got) != textOf(t, want) {
		t.Fatalf("a read-only client saw %q, want %q", textOf(t, got), textOf(t, want))
	}

	// It answers our SyncStep1 with nothing, which is not an attempt to write.
	reader.send(protocol.WriteSyncStep2([]byte{0, 0}))

	// A real edit is refused with a reason and a 1008 close.
	reader.send(protocol.WriteUpdate(readFixture(t, "text-three-client-interleaved", "update-001.bin")))
	if reason := reader.expectDenied(t); reason == "" {
		t.Fatal("the refusal carried no reason")
	}

	// And the document is unchanged: a third client sees exactly what the writer
	// put there.
	third := dialRaw(t, srv.addr, doc+"?token="+mintToken(t, doc, auth.PermissionRead, time.Hour))
	if after := third.sync(); textOf(t, after) != textOf(t, want) {
		t.Fatalf("the document changed to %q", textOf(t, after))
	}
}

// Without a secret the server is open, which is what makes the demo work with
// no setup - and it is a decision worth pinning, not an accident.
func TestWithoutASecretEverybodyMayWrite(t *testing.T) {
	srv := startServer(t, buildServer(t), freePort(t), "")
	doc := fmt.Sprintf("open-%d", time.Now().UnixNano())

	c := dialRaw(t, srv.addr, doc)
	c.sync()
	update := readFixture(t, "text-insert-single", "update-000.bin")
	c.send(protocol.WriteUpdate(update))

	want := crdt.NewDoc(9)
	if err := want.ApplyUpdate(update); err != nil {
		t.Fatal(err)
	}
	other := dialRaw(t, srv.addr, doc)
	if got := other.sync(); textOf(t, got) != textOf(t, want) {
		t.Fatalf("got %q, want %q", textOf(t, got), textOf(t, want))
	}
}
