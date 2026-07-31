package main_test

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/mesutokul/ycollab/internal/crdt"
	"github.com/mesutokul/ycollab/internal/protocol"
)

// The runbook's per-document restore, run end to end: take the bytes, lose the
// document, put the bytes back, read it again. A backup nobody has restored is
// not a backup, and this is the test that makes the procedure in the runbook a
// claim rather than a hope.
func TestADocumentCanBeBackedUpAndRestored(t *testing.T) {
	dbURL := os.Getenv(dbEnv)
	if dbURL == "" {
		t.Skipf("%s is not set; start deploy/docker-compose.yml to run this", dbEnv)
	}
	srv := startServer(t, buildServer(t), freePort(t), dbURL)
	doc := fmt.Sprintf("restore-%d", time.Now().UnixNano())

	c := dial(t, srv.addr, doc)
	c.sync()
	updates := scenarioUpdatesFor(t, "text-three-client-interleaved")
	for _, u := range updates {
		c.send(protocol.WriteUpdate(u))
	}
	c.send(protocol.WriteSyncStep1(emptyStateVector(t)))
	c.recv()

	// 1. Back it up.
	resp, backup := get(t, srv, "/documents/"+doc, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the backup read returned %d: %s", resp.StatusCode, backup)
	}
	want := textOfUpdate(t, backup)
	if want == "" {
		t.Fatal("the backup is of an empty document, so this test proves nothing")
	}

	// 2. Lose it. The client has to go first: DELETE refuses a document
	//    somebody is editing, which is itself the behaviour the runbook relies
	//    on.
	c.disconnect()
	waitForDelete(t, srv, doc)
	if missing, _ := get(t, srv, "/documents/"+doc, nil); missing.StatusCode != http.StatusNotFound {
		t.Fatalf("the document is still there after DELETE: %d", missing.StatusCode)
	}

	// 3. Put it back.
	restore := post(t, srv, "/documents/"+doc, backup)
	if restore != http.StatusNoContent {
		t.Fatalf("the restore returned %d", restore)
	}

	// 4. And it is the document again, over HTTP and to a client that connects
	//    afterwards - which is the half that matters, because that is who
	//    noticed it was gone.
	after, body := get(t, srv, "/documents/"+doc, nil)
	if after.StatusCode != http.StatusOK {
		t.Fatalf("reading the restored document returned %d: %s", after.StatusCode, body)
	}
	if got := textOfUpdate(t, body); got != want {
		t.Errorf("the restored document reads %q, want %q", got, want)
	}
	reconnected := dial(t, srv.addr, doc)
	if got := textOf(t, reconnected.sync()); got != want {
		t.Errorf("a client that reconnected sees %q, want %q", got, want)
	}
}

// A restore into a document people are editing has to reach them, or the
// clients keep building on a version the server no longer has.
func TestARestoreReachesTheClientsAlreadyConnected(t *testing.T) {
	srv := startServer(t, buildServer(t), freePort(t), "")
	source := fmt.Sprintf("template-%d", time.Now().UnixNano())
	target := fmt.Sprintf("seeded-%d", time.Now().UnixNano())

	// Something to copy, taken the way an operator would take it.
	author := dial(t, srv.addr, source)
	author.sync()
	update := readFixture(t, "text-insert-single", "update-000.bin")
	author.send(protocol.WriteUpdate(update))
	author.send(protocol.WriteSyncStep1(emptyStateVector(t)))
	author.recv()
	_, template := get(t, srv, "/documents/"+source, nil)

	// Somebody is sitting in the empty target document.
	watcher := dial(t, srv.addr, target)
	watcher.sync()

	if code := post(t, srv, "/documents/"+target, template); code != http.StatusNoContent {
		t.Fatalf("the merge returned %d", code)
	}

	// The connection was open the whole time, so the update has to arrive on it.
	got, err := watcher.recvUpdate(10 * time.Second)
	if err != nil {
		t.Fatalf("the client that was already connected never heard about the merge: %v", err)
	}
	doc := crdt.NewDoc(1)
	if err := doc.ApplyUpdate(got); err != nil {
		t.Fatalf("what arrived would not apply: %v", err)
	}
	if want := textOfUpdate(t, template); textOf(t, doc) != want {
		t.Errorf("the client received %q, want %q", textOf(t, doc), want)
	}
}

// A body that is not an update, or one that carries nothing, is refused. Both
// would otherwise be written, published and reported as a success having
// changed nothing.
func TestAMergeThatSaysNothingIsRefused(t *testing.T) {
	srv := startServer(t, buildServer(t), freePort(t), "")
	doc := fmt.Sprintf("merge-bad-%d", time.Now().UnixNano())
	c := dial(t, srv.addr, doc)
	c.sync()

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"empty body", nil},
		{"an update that carries nothing", []byte{0, 0}},
		{"not an update at all", []byte("this is not a Yjs update")},
	} {
		if code := post(t, srv, "/documents/"+doc, tc.body); code != http.StatusBadRequest {
			t.Errorf("%s returned %d, want 400", tc.name, code)
		}
	}
}

func post(t *testing.T, s *server, path string, body []byte) int {
	t.Helper()
	resp, err := http.Post("http://"+s.admin+path, "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// waitForDelete retries DELETE until the room has evicted, because a document
// somebody just disconnected from is still resident for a moment.
func waitForDelete(t *testing.T, s *server, doc string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		req, err := http.NewRequest(http.MethodDelete, "http://"+s.admin+"/documents/"+doc, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNoContent {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("DELETE kept returning %d", resp.StatusCode)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func textOfUpdate(t *testing.T, update []byte) string {
	t.Helper()
	doc := crdt.NewDoc(1)
	if err := doc.ApplyUpdate(update); err != nil {
		t.Fatalf("the update would not apply: %v", err)
	}
	return textOf(t, doc)
}
