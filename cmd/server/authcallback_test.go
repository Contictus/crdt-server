package main_test

// The external authorisation callback, against a real server process talking to
// a real endpoint over a real socket.
//
// The point of these tests is the thing the JWT path cannot do: one endpoint,
// one session cookie, and no token minted per document - including for
// subdocuments, whose names the application only learns about when Yjs tells it.

import (
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

	"github.com/mesutokul/ycollab/internal/auth"
	"github.com/mesutokul/ycollab/internal/crdt"
	"github.com/mesutokul/ycollab/internal/protocol"
	"github.com/mesutokul/ycollab/internal/signature"
)

const authCallbackSecret = "an-auth-callback-secret-long-enough-for-hmac"

// askedAbout is one question the server put to the endpoint.
type askedAbout struct {
	Document string `json:"document"`
	Token    string `json:"token"`
	IP       string `json:"ip"`
	Origin   string `json:"origin"`
}

// endpoint is a test authorisation service. It records every question and
// answers with whatever decide returns.
type endpoint struct {
	*httptest.Server
	mu    sync.Mutex
	asked []askedAbout
}

// startEndpoint runs an authorisation endpoint. decide is called per request
// and returns the JSON body to answer with.
func startEndpoint(t *testing.T, decide func(askedAbout) string) *endpoint {
	t.Helper()
	e := &endpoint{}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the request: %v", err)
			return
		}
		// Every request is signed, and the endpoint checks it. An authorisation
		// service that answers unsigned questions answers them for anybody who
		// can reach it.
		if err := signature.Verify([]byte(authCallbackSecret), r.Header.Get(auth.HeaderSignature), body, time.Now(), time.Minute); err != nil {
			t.Errorf("signature: %v", err)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var q askedAbout
		if err := json.Unmarshal(body, &q); err != nil {
			t.Errorf("body: %v", err)
			return
		}
		e.mu.Lock()
		e.asked = append(e.asked, q)
		e.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, decide(q))
	}))
	t.Cleanup(e.Close)
	return e
}

func (e *endpoint) questions() []askedAbout {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]askedAbout(nil), e.asked...)
}

// startCallbackServer starts a server whose only authority is the endpoint.
func startCallbackServer(t *testing.T, e *endpoint, extra ...string) *server {
	t.Helper()
	args := append([]string{"-auth-url", e.URL, "-auth-secret", authCallbackSecret}, extra...)
	return startServer(t, buildServer(t), freePort(t), "", args...)
}

// The whole reason the feature exists: one session token, and the application
// decides per document without minting anything.
func TestOneSessionTokenOpensWhateverTheEndpointAllows(t *testing.T) {
	allowed := map[string]bool{"notes": true, "chapter-one": true}
	e := startEndpoint(t, func(q askedAbout) string {
		if q.Token != "session=abc" || !allowed[q.Document] {
			return `{"allow":false,"reason":"not your document"}`
		}
		return `{"allow":true,"write":true,"subject":"user_42"}`
	})
	srv := startCallbackServer(t, e)

	update := readFixture(t, "text-insert-single", "update-000.bin")
	want := crdt.NewDoc(9)
	if err := want.ApplyUpdate(update); err != nil {
		t.Fatal(err)
	}

	// The same token opens two different documents, which is exactly what a
	// per-document capability cannot do.
	for _, doc := range []string{"notes", "chapter-one"} {
		c := dialRaw(t, srv.addr, doc+"?token=session%3Dabc")
		c.sync()
		c.send(protocol.WriteUpdate(update))

		reader := dialRaw(t, srv.addr, doc+"?token=session%3Dabc")
		if got := reader.sync(); textOf(t, got) != textOf(t, want) {
			t.Fatalf("%s: got %q, want %q", doc, textOf(t, got), textOf(t, want))
		}
	}

	// And a third document, with the same good token, is refused - so the
	// endpoint is genuinely deciding per document and not per session.
	refused := dialRaw(t, srv.addr, "someone-elses?token=session%3Dabc")
	if reason := refused.expectDenied(t); !strings.Contains(reason, "not your document") {
		t.Fatalf("reason %q, want the endpoint's own words", reason)
	}

	// The endpoint was told which document each time, and got the token whole.
	names := map[string]bool{}
	for _, q := range e.questions() {
		names[q.Document] = true
		if q.Token != "session=abc" {
			t.Errorf("the endpoint was given the token as %q", q.Token)
		}
		if q.IP == "" {
			t.Error("the endpoint was not told the client address")
		}
	}
	for _, want := range []string{"notes", "chapter-one", "someone-elses"} {
		if !names[want] {
			t.Errorf("the endpoint was never asked about %q", want)
		}
	}
}

// A subdocument is just another document name here, and its guid is only known
// once Yjs emits it. With a callback the application never has to mint anything
// for it.
func TestASubdocumentGuidIsJustAnotherQuestion(t *testing.T) {
	e := startEndpoint(t, func(q askedAbout) string {
		// The application's rule: you may open a subdocument of a document you
		// may open. It can express that because it is asked at open time.
		if strings.HasPrefix(q.Document, "book") {
			return `{"allow":true,"write":true,"subject":"user_42"}`
		}
		return `{"allow":false,"reason":"not part of that book"}`
	})
	srv := startCallbackServer(t, e)

	parent := dialRaw(t, srv.addr, "book?token=session%3Dabc")
	parent.sync()
	child := dialRaw(t, srv.addr, "book-chapter-one?token=session%3Dabc")
	child.sync()

	stranger := dialRaw(t, srv.addr, "another-book?token=session%3Dabc")
	if reason := stranger.expectDenied(t); !strings.Contains(reason, "not part of that book") {
		t.Fatalf("reason %q", reason)
	}
}

// Read-only is the endpoint's to grant, and it has to mean the same thing it
// means for a JWT: the client reads the document and is refused when it writes.
func TestTheEndpointCanGrantReadOnly(t *testing.T) {
	e := startEndpoint(t, func(q askedAbout) string {
		if q.Token == "writer" {
			return `{"allow":true,"write":true}`
		}
		return `{"allow":true,"write":false}`
	})
	srv := startCallbackServer(t, e)
	doc := fmt.Sprintf("readonly-%d", time.Now().UnixNano())

	update := readFixture(t, "text-insert-single", "update-000.bin")
	want := crdt.NewDoc(9)
	if err := want.ApplyUpdate(update); err != nil {
		t.Fatal(err)
	}

	writer := dialRaw(t, srv.addr, doc+"?token=writer")
	writer.sync()
	writer.send(protocol.WriteUpdate(update))

	reader := dialRaw(t, srv.addr, doc+"?token=viewer")
	if got := reader.sync(); textOf(t, got) != textOf(t, want) {
		t.Fatalf("the reader saw %q, want %q", textOf(t, got), textOf(t, want))
	}
	reader.send(protocol.WriteUpdate(readFixture(t, "text-three-client-interleaved", "update-001.bin")))
	if reason := reader.expectDenied(t); reason == "" {
		t.Fatal("the refusal carried no reason")
	}

	third := dialRaw(t, srv.addr, doc+"?token=viewer")
	if after := third.sync(); textOf(t, after) != textOf(t, want) {
		t.Fatalf("the document changed to %q", textOf(t, after))
	}
}

// An endpoint that is down refuses connections by default. This is the
// behaviour an operator is choosing between, so it is worth having pinned
// against a real process rather than only in a unit test.
func TestAnEndpointThatIsDownRefusesConnections(t *testing.T) {
	e := startEndpoint(t, func(askedAbout) string { return `{"allow":true,"write":true}` })
	e.Close() // Nothing is listening now.

	srv := startServer(t, buildServer(t), freePort(t), "",
		"-auth-url", e.URL, "-auth-secret", authCallbackSecret)

	c := dialRaw(t, srv.addr, "notes?token=session%3Dabc")
	if reason := c.expectDenied(t); reason == "" {
		t.Fatal("the refusal carried no reason")
	}
}

// The two ways of deciding are alternatives. Running both would mean picking a
// winner when they disagree, and the server says so at startup rather than
// picking one quietly.
func TestTheServerRefusesToUseBothJWTAndACallback(t *testing.T) {
	cmd := exec.Command(buildServer(t),
		"-addr", freePort(t),
		"-admin-addr", "",
		"-jwt-secret", testSecret,
		"-auth-url", "https://example.com/authorize",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the server started with both:\n%s", out)
	}
	if !strings.Contains(string(out), "alternatives") {
		t.Errorf("it failed for some other reason:\n%s", out)
	}
}
