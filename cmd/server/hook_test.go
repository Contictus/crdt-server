package main_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mesutokul/ycollab/internal/crdt"
	"github.com/mesutokul/ycollab/internal/hook"
	"github.com/mesutokul/ycollab/internal/protocol"
)

// The webhook is only real if a separate process, configured by flags, reaches
// an endpoint that is not in this test binary's imagination. This starts the
// server the way an operator would, edits a document over a real WebSocket, and
// reads what arrived at the other end.
func TestTheServerCallsAWebhook(t *testing.T) {
	rec := newWebhookReceiver(t)

	const secret = "webhook-test-secret"
	srv := startServer(t, buildServer(t), freePort(t), "",
		"-webhook-url", rec.url,
		"-webhook-secret", secret,
		"-webhook-state",
		// The room reports on its tick, so this is how long the test waits.
		"-tick", "100ms",
	)

	doc := fmt.Sprintf("hooked-%d", time.Now().UnixNano())
	c := dial(t, srv.addr, doc)
	c.sync()
	update := readFixture(t, "text-insert-single", "update-000.bin")
	c.send(protocol.WriteUpdate(update))

	got := rec.await(t, hook.KindChange)

	// Signed with the configured secret, over exactly the bytes that arrived.
	if err := hook.Verify([]byte(secret), got.signature, got.raw, time.Now(), time.Minute); err != nil {
		t.Fatalf("the request was not signed by this server: %v", err)
	}
	if got.body.Document != doc {
		t.Errorf("the event is about %q, want %q", got.body.Document, doc)
	}
	if got.body.Clients != 1 {
		t.Errorf("the event says %d clients", got.body.Clients)
	}
	if got.body.Updates != 1 {
		t.Errorf("the event covers %d updates, want 1", got.body.Updates)
	}
	if got.body.StateVector == "" {
		t.Error("the event carried no state vector")
	}

	// -webhook-state means the receiver gets a document it can open, which is
	// the difference between a notification and something a backend can index.
	state, err := base64.StdEncoding.DecodeString(got.body.State)
	if err != nil {
		t.Fatalf("state is not base64: %v", err)
	}
	fromHook := crdt.NewDoc(1)
	if err := fromHook.ApplyUpdate(state); err != nil {
		t.Fatalf("the state in the event would not apply: %v", err)
	}
	expected := crdt.NewDoc(1)
	if err := expected.ApplyUpdate(update); err != nil {
		t.Fatal(err)
	}
	if got, want := textOf(t, fromHook), textOf(t, expected); got != want {
		t.Errorf("the event carried %q, want %q", got, want)
	}

	// And the server counted it, because a delivery nobody can see on the
	// dashboard is a delivery nobody will notice stopping.
	samples := scrape(t, srv)
	if v := samples[`ycollab_hooks_sent_total{event="document.change"}`]; v < 1 {
		t.Errorf("ycollab_hooks_sent_total is %v", v)
	}
}

// A name that is not an event is a typo, and a typo that silently sends nothing
// is discovered a week later.
func TestAnUnknownWebhookEventIsRefusedAtStartup(t *testing.T) {
	out, err := exec.Command(buildServer(t),
		"-addr", freePort(t),
		"-admin-addr", "",
		"-webhook-url", "http://127.0.0.1:1/hook",
		"-webhook-events", "document.chnage",
	).CombinedOutput()
	if err == nil {
		t.Fatalf("the server started with an unknown event name\n%s", out)
	}
	for _, want := range []string{"document.chnage", "document.change"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("the error does not mention %q:\n%s", want, out)
		}
	}
}

// A URL that cannot work is refused too, rather than logged once at startup and
// forgotten.
func TestABadWebhookURLIsRefusedAtStartup(t *testing.T) {
	out, err := exec.Command(buildServer(t),
		"-addr", freePort(t),
		"-admin-addr", "",
		"-webhook-url", "ftp://example.invalid/hook",
	).CombinedOutput()
	if err == nil {
		t.Fatalf("the server started with a URL it cannot post to\n%s", out)
	}
	if !bytes.Contains(out, []byte("ftp")) {
		t.Errorf("the error does not say what was wrong:\n%s", out)
	}
}

type webhookBody struct {
	Event       string `json:"event"`
	Document    string `json:"document"`
	At          string `json:"at"`
	Node        uint64 `json:"node"`
	Clients     int    `json:"clients"`
	Updates     uint64 `json:"updates"`
	StateVector string `json:"state_vector"`
	State       string `json:"state"`
}

// A delivery keeps the raw bytes as well as the parsed form: the signature is
// over the bytes, and re-encoding the struct would not reproduce them.
type delivery struct {
	body      webhookBody
	raw       []byte
	signature string
}

type webhookReceiver struct {
	url string

	mu         sync.Mutex
	deliveries []delivery
	seen       chan struct{}
}

func newWebhookReceiver(t *testing.T) *webhookReceiver {
	t.Helper()
	r := &webhookReceiver{seen: make(chan struct{}, 64)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		var body webhookBody
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("webhook body is not JSON: %v\n%s", err, raw)
		}
		r.mu.Lock()
		r.deliveries = append(r.deliveries, delivery{
			body: body, raw: raw, signature: req.Header.Get(hook.HeaderSignature),
		})
		r.mu.Unlock()
		select {
		case r.seen <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	r.url = srv.URL
	return r
}

// await blocks until an event of the given kind arrives.
func (r *webhookReceiver) await(t *testing.T, kind hook.Kind) delivery {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		r.mu.Lock()
		var others []string
		for _, d := range r.deliveries {
			if d.body.Event == string(kind) {
				r.mu.Unlock()
				return d
			}
			others = append(others, d.body.Event)
		}
		r.mu.Unlock()
		select {
		case <-r.seen:
		case <-deadline:
			t.Fatalf("no %s event arrived; what did: %s", kind, strings.Join(others, ", "))
		}
	}
}
