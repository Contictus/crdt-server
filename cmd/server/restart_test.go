package main_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mesutokul/ycollab/internal/crdt"
	"github.com/mesutokul/ycollab/internal/protocol"
	"github.com/mesutokul/ycollab/internal/store"
)

// The Phase 3 acceptance criterion: kill the server mid-session, restart it,
// reconnect, and find the document intact.
//
// It kills a real process rather than cancelling a context, because the two
// failure modes are not the same: a graceful shutdown gets to flush, and a crash
// does not. What survives a crash is what was actually written, which is the
// only thing this test is interested in.
//
//	docker compose -f deploy/docker-compose.yml up -d
//	YCOLLAB_TEST_DATABASE_URL=postgres://ycollab:ycollab@127.0.0.1:5433/ycollab go test ./cmd/server/
const dbEnv = "YCOLLAB_TEST_DATABASE_URL"

type server struct {
	t    *testing.T
	cmd  *exec.Cmd
	addr string
	logs *bytes.Buffer
}

// buildServer compiles the binary once per test binary run.
func buildServer(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "ycollab-server")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	build := exec.Command("go", "build", "-o", out, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build: %v", err)
	}
	return out
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func startServer(t *testing.T, binary, addr, dbURL string, extra ...string) *server {
	t.Helper()
	logs := &bytes.Buffer{}
	args := append([]string{
		"-addr", addr,
		"-database-url", dbURL,
		// Short intervals so the test does not have to wait for a default that
		// was chosen for production.
		"-flush-interval", "20ms",
		"-compact-after", "5",
		"-log-level", "debug",
	}, extra...)
	cmd := exec.Command(binary, args...)
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	s := &server{t: t, cmd: cmd, addr: addr, logs: logs}
	t.Cleanup(s.kill)
	s.waitReady()
	return s
}

func (s *server) waitReady() {
	s.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + s.addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.t.Fatalf("server never became ready\n%s", s.logs)
}

// kill terminates the process without giving it a chance to shut down cleanly.
func (s *server) kill() {
	if s.cmd.Process == nil {
		return
	}
	_ = s.cmd.Process.Kill()
	_, _ = s.cmd.Process.Wait()
}

type client struct {
	t  *testing.T
	ws *websocket.Conn
}

func dial(t *testing.T, addr, doc string) *client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws://"+addr+"/"+doc, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	return &client{t: t, ws: ws}
}

func (c *client) send(frame []byte) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageBinary, frame); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *client) recv() protocol.Message {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, data, err := c.ws.Read(ctx)
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	msg, err := protocol.Decode(data)
	if err != nil {
		c.t.Fatalf("decode: %v", err)
	}
	return msg
}

// sync performs the client half of the handshake and returns what the server
// says the document is.
func (c *client) sync() *crdt.Doc {
	c.t.Helper()
	sv, err := crdt.NewDoc(1).EncodeStateVector()
	if err != nil {
		c.t.Fatal(err)
	}
	c.send(protocol.WriteSyncStep1(sv))

	doc := crdt.NewDoc(1)
	msg := c.recv()
	step2, ok := msg.(protocol.SyncStep2Message)
	if !ok {
		c.t.Fatalf("got %T, want SyncStep2", msg)
	}
	if err := doc.ApplyUpdate(step2.Update); err != nil {
		c.t.Fatalf("apply: %v", err)
	}
	if _, ok := c.recv().(protocol.SyncStep1Message); !ok {
		c.t.Fatal("server did not follow with its own SyncStep1")
	}
	return doc
}

func textOf(t *testing.T, doc *crdt.Doc) string {
	t.Helper()
	var out string
	for _, name := range doc.Roots() {
		out += crdt.AsText(doc.Get(name)).String()
	}
	return out
}

func readFixture(t *testing.T, parts ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{"..", "..", "testdata", "fixtures"}, parts...)...))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

// waitPersisted polls the database, so the test asserts on what was written
// rather than on how long a write is assumed to take.
func waitPersisted(t *testing.T, dbURL, doc string, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	id := store.DocumentID(doc)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		n, err := s.UpdateCount(ctx, id)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if n >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d updates to be written", want)
}

func TestDocumentSurvivesAKill(t *testing.T) {
	dbURL := os.Getenv(dbEnv)
	if dbURL == "" {
		t.Skipf("%s is not set; start deploy/docker-compose.yml to run this", dbEnv)
	}
	binary := buildServer(t)
	addr := freePort(t)
	doc := fmt.Sprintf("restart-%d", time.Now().UnixNano())

	first := startServer(t, binary, addr, dbURL)
	c := dial(t, addr, doc)
	c.sync()

	state := readFixture(t, "text-insert-single", "state.bin")
	c.send(protocol.WriteUpdate(state))
	waitPersisted(t, dbURL, doc, 1)

	want := crdt.NewDoc(9)
	if err := want.ApplyUpdate(state); err != nil {
		t.Fatal(err)
	}

	// No shutdown signal, no close frames: the process simply stops existing.
	first.kill()

	second := startServer(t, binary, addr, dbURL)
	defer second.kill()

	back := dial(t, addr, doc)
	got := back.sync()
	if textOf(t, got) != textOf(t, want) {
		t.Fatalf("document did not survive\n got %q\nwant %q", textOf(t, got), textOf(t, want))
	}
	gotSV, err := got.EncodeStateVector()
	if err != nil {
		t.Fatal(err)
	}
	wantSV, err := want.EncodeStateVector()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotSV, wantSV) {
		t.Fatalf("state vector\n got %x\nwant %x", gotSV, wantSV)
	}
}

// A client that was editing when the server died keeps its own copy, so
// reconnecting must both restore what the server had and take back what the
// client had that the server never wrote. That is the "without user-visible
// loss" half of the acceptance criterion.
func TestClientResyncsAfterAKill(t *testing.T) {
	dbURL := os.Getenv(dbEnv)
	if dbURL == "" {
		t.Skipf("%s is not set; start deploy/docker-compose.yml to run this", dbEnv)
	}
	binary := buildServer(t)
	addr := freePort(t)
	doc := fmt.Sprintf("resync-%d", time.Now().UnixNano())

	updates := [][]byte{
		readFixture(t, "text-three-client-interleaved", "update-000.bin"),
		readFixture(t, "text-three-client-interleaved", "update-001.bin"),
	}

	first := startServer(t, binary, addr, dbURL)
	writer := dial(t, addr, doc)
	writer.sync()
	writer.send(protocol.WriteUpdate(updates[0]))
	waitPersisted(t, dbURL, doc, 1)
	first.kill()

	// The client's own document holds both updates; the server only ever saw
	// the first.
	client := crdt.NewDoc(1)
	for _, u := range updates {
		if err := client.ApplyUpdate(u); err != nil {
			t.Fatal(err)
		}
	}

	second := startServer(t, binary, addr, dbURL)
	defer second.kill()

	back := dial(t, addr, doc)
	serverDoc := back.sync()
	// The server's SyncStep1 asks for what it is missing; the client answers
	// with a diff, which is how the update that was never written comes back.
	serverSV, err := serverDoc.EncodeStateVector()
	if err != nil {
		t.Fatal(err)
	}
	diff, err := client.EncodeDiff(serverSV)
	if err != nil {
		t.Fatal(err)
	}
	back.send(protocol.WriteSyncStep2(diff))
	waitPersisted(t, dbURL, doc, 2)

	third := dial(t, addr, doc)
	final := third.sync()
	if got, want := textOf(t, final), textOf(t, client); got != want {
		t.Fatalf("resync lost content\n got %q\nwant %q", got, want)
	}
}

// Compaction has to leave the document readable: a snapshot that does not
// rebuild what it replaced is worse than no snapshot at all.
func TestCompactedDocumentSurvivesAKill(t *testing.T) {
	dbURL := os.Getenv(dbEnv)
	if dbURL == "" {
		t.Skipf("%s is not set; start deploy/docker-compose.yml to run this", dbEnv)
	}
	binary := buildServer(t)
	addr := freePort(t)
	doc := fmt.Sprintf("compact-%d", time.Now().UnixNano())

	// -compact-after 5, so this crosses the threshold.
	var updates [][]byte
	for i := range 8 {
		updates = append(updates, readFixture(t, "text-three-client-interleaved", fmt.Sprintf("update-%03d.bin", i)))
	}

	first := startServer(t, binary, addr, dbURL)
	c := dial(t, addr, doc)
	c.sync()
	for _, u := range updates {
		c.send(protocol.WriteUpdate(u))
	}

	// Wait until a snapshot exists - which is what proves compaction ran - and
	// until the updates that came after it have been written too. A crash can
	// only preserve what was written, and this test is about compaction, not
	// about the width of the flush window.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id := store.DocumentID(doc)
	deadline := time.Now().Add(15 * time.Second)
	for {
		loaded, err := db.Load(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		// 8 updates, compaction at 5: a snapshot plus the 3 that followed it.
		if loaded.Snapshot != nil && len(loaded.Updates) >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("compaction never ran: snapshot=%v updates=%d", loaded.Snapshot != nil, len(loaded.Updates))
		}
		time.Sleep(20 * time.Millisecond)
	}

	first.kill()
	second := startServer(t, binary, addr, dbURL)
	defer second.kill()

	want := crdt.NewDoc(9)
	for _, u := range updates {
		if err := want.ApplyUpdate(u); err != nil {
			t.Fatal(err)
		}
	}
	got := dial(t, addr, doc).sync()
	if textOf(t, got) != textOf(t, want) {
		t.Fatalf("compacted document did not survive\n got %q\nwant %q", textOf(t, got), textOf(t, want))
	}
}
