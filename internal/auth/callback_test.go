package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mesutokul/ycollab/internal/auth"
	"github.com/mesutokul/ycollab/internal/signature"
)

// quiet keeps the warnings the constructor writes out of the test output; they
// are deliberate and every one of these tests trips at least one.
func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newCallback wires a Callback to a handler, returning both it and a counter of
// how many requests the handler saw.
func newCallback(t *testing.T, cfg auth.CallbackConfig, h http.HandlerFunc) (*auth.Callback, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	cfg.URL = srv.URL
	if cfg.Logger == nil {
		cfg.Logger = quiet()
	}
	c, err := auth.NewCallback(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, &calls
}

// answer writes the JSON an endpoint would.
func answer(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

func TestTheEndpointDecides(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		wantWrite bool
		wantSub   string
	}{
		{"a writer", `{"allow":true,"write":true,"subject":"user_42"}`, true, "user_42"},
		{"a reader", `{"allow":true,"write":false,"subject":"user_7"}`, false, "user_7"},
		{"no subject", `{"allow":true,"write":true}`, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newCallback(t, auth.CallbackConfig{}, func(w http.ResponseWriter, _ *http.Request) {
				answer(w, tc.body)
			})
			grant, err := c.Authorize(t.Context(), auth.Request{Document: "notes", Token: "t"})
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if grant.Write != tc.wantWrite {
				t.Errorf("write=%v, want %v", grant.Write, tc.wantWrite)
			}
			if grant.Subject != tc.wantSub {
				t.Errorf("subject=%q, want %q", grant.Subject, tc.wantSub)
			}
			if grant.Doc != "notes" {
				t.Errorf("doc=%q, want the document that was asked about", grant.Doc)
			}
		})
	}
}

// A refusal's reason is written to the client, so it has to survive the trip.
func TestARefusalCarriesItsReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"a 200 saying no", 200, `{"allow":false,"reason":"this document is archived"}`, "this document is archived"},
		{"a 403 with JSON", 403, `{"reason":"your trial ended"}`, "your trial ended"},
		{"a 403 with text", 403, `not your document`, "not your document"},
		{"a 401", 401, ``, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newCallback(t, auth.CallbackConfig{}, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			_, err := c.Authorize(t.Context(), auth.Request{Document: "notes"})
			if !errors.Is(err, auth.ErrDenied) {
				t.Fatalf("err=%v, want a denial", err)
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err=%q, want it to carry %q", err, tc.want)
			}
		})
	}
}

// The endpoint being down is not the client's fault, and it must be told apart
// from the endpoint refusing: only one of the two is the operator's to fix.
func TestAnUnreachableEndpointIsRefusedByDefault(t *testing.T) {
	c, _ := newCallback(t, auth.CallbackConfig{}, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	_, err := c.Authorize(t.Context(), auth.Request{Document: "notes"})
	if !errors.Is(err, auth.ErrUnavailable) {
		t.Fatalf("err=%v, want ErrUnavailable", err)
	}
	if errors.Is(err, auth.ErrDenied) {
		t.Error("an outage was reported as a denial")
	}
}

func TestFailOpenServesThroughAnOutage(t *testing.T) {
	c, _ := newCallback(t, auth.CallbackConfig{FailOpen: true}, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	grant, err := c.Authorize(t.Context(), auth.Request{Document: "notes"})
	if err != nil {
		t.Fatalf("fail-open refused a connection: %v", err)
	}
	if !grant.Write {
		t.Error("fail-open granted read only; it is meant to keep the deployment working")
	}
}

// The distinction fail-open exists for is "the endpoint is down". An endpoint
// that is up and answering something unusable is a misconfiguration, and
// opening the doors for it turns a typo in a URL into an open server.
func TestFailOpenDoesNotCoverAMisconfiguredEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"a 404 from the wrong path", 404, `not found`},
		{"a 204 from something that is not this API", 204, ``},
		{"a 400", 400, `bad request`},
		{"200 with no allow field", 200, `{"write":true}`},
		{"200 with a body that is not JSON", 200, `<html>hello</html>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newCallback(t, auth.CallbackConfig{FailOpen: true}, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			_, err := c.Authorize(t.Context(), auth.Request{Document: "notes"})
			if !errors.Is(err, auth.ErrDenied) {
				t.Fatalf("err=%v, want a denial even under fail-open", err)
			}
		})
	}
}

// What the endpoint is told. It decides on this and nothing else, so every
// field is part of the contract.
func TestTheEndpointIsToldWhoIsAsking(t *testing.T) {
	var got struct {
		Document string `json:"document"`
		Token    string `json:"token"`
		IP       string `json:"ip"`
		Origin   string `json:"origin"`
	}
	var sig, agent, contentType string
	secret := []byte("0123456789abcdef0123456789abcdef")

	c, _ := newCallback(t, auth.CallbackConfig{Secret: secret}, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sig = r.Header.Get(auth.HeaderSignature)
		agent = r.Header.Get("User-Agent")
		contentType = r.Header.Get("Content-Type")
		if err := signature.Verify(secret, sig, body, time.Now(), time.Minute); err != nil {
			t.Errorf("the endpoint could not verify the signature: %v", err)
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("body is not the documented JSON: %v", err)
		}
		answer(w, `{"allow":true,"write":true}`)
	})

	if _, err := c.Authorize(t.Context(), auth.Request{
		Document: "chapter-one",
		Token:    "session=abc",
		IP:       "203.0.113.7",
		Origin:   "https://app.example.com",
	}); err != nil {
		t.Fatal(err)
	}

	if got.Document != "chapter-one" || got.Token != "session=abc" ||
		got.IP != "203.0.113.7" || got.Origin != "https://app.example.com" {
		t.Errorf("the endpoint was told %+v", got)
	}
	if sig == "" {
		t.Error("no signature: the endpoint cannot tell this server from anybody else who can reach it")
	}
	if !strings.HasPrefix(agent, "ycollab-auth/") {
		t.Errorf("User-Agent=%q", agent)
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type=%q", contentType)
	}
}

// A token is a document-scoped capability in the JWT path; here it is opaque
// and forwarded whole, because the point is that the endpoint - not this server
// - knows what it means.
func TestTheTokenIsForwardedUnverified(t *testing.T) {
	var got string
	c, _ := newCallback(t, auth.CallbackConfig{}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Token string `json:"token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		got = req.Token
		answer(w, `{"allow":true}`)
	})
	if _, err := c.Authorize(t.Context(), auth.Request{Document: "d", Token: "not-a-jwt at all"}); err != nil {
		t.Fatal(err)
	}
	if got != "not-a-jwt at all" {
		t.Errorf("token arrived as %q", got)
	}
}

func TestNoCacheByDefault(t *testing.T) {
	c, calls := newCallback(t, auth.CallbackConfig{}, func(w http.ResponseWriter, _ *http.Request) {
		answer(w, `{"allow":true,"write":true}`)
	})
	for range 3 {
		if _, err := c.Authorize(t.Context(), auth.Request{Document: "d", Token: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 3 {
		t.Errorf("%d requests for 3 connections; a decision was remembered with the cache off, so a revocation would not take effect", calls.Load())
	}
}

func TestTheCacheAnswersUntilItExpires(t *testing.T) {
	now := time.Unix(1750000000, 0)
	c, calls := newCallback(t, auth.CallbackConfig{
		CacheTTL: 30 * time.Second,
		Now:      func() time.Time { return now },
	}, func(w http.ResponseWriter, _ *http.Request) {
		answer(w, `{"allow":true,"write":true}`)
	})

	req := auth.Request{Document: "d", Token: "t"}
	for range 5 {
		if _, err := c.Authorize(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("%d requests for 5 connections with the same token; the cache did not answer", calls.Load())
	}
	// A different token is a different question.
	if _, err := c.Authorize(t.Context(), auth.Request{Document: "d", Token: "other"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Errorf("%d requests; a second token was answered from the first one's decision", calls.Load())
	}
	// And a different document is too, which is what stops a token for one
	// document being a token for every document.
	if _, err := c.Authorize(t.Context(), auth.Request{Document: "other", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Errorf("%d requests; a second document was answered from the first one's decision", calls.Load())
	}

	now = now.Add(31 * time.Second)
	if _, err := c.Authorize(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 {
		t.Errorf("%d requests; the cache answered past its lifetime", calls.Load())
	}
}

// The endpoint may say "ask me again sooner". It may not say "ask me again
// later": how much staleness a deployment tolerates is the operator's call.
func TestTheEndpointMayOnlyShortenTheCache(t *testing.T) {
	now := time.Unix(1750000000, 0)
	c, calls := newCallback(t, auth.CallbackConfig{
		CacheTTL: time.Hour,
		Now:      func() time.Time { return now },
	}, func(w http.ResponseWriter, _ *http.Request) {
		answer(w, `{"allow":true,"write":true,"ttl":5}`)
	})
	req := auth.Request{Document: "d", Token: "t"}
	if _, err := c.Authorize(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	now = now.Add(6 * time.Second)
	if _, err := c.Authorize(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Errorf("%d requests; the endpoint asked to be re-asked after 5s and was not", calls.Load())
	}

	// The other direction: a ttl longer than the configured lifetime is capped.
	c2, calls2 := newCallback(t, auth.CallbackConfig{
		CacheTTL: 10 * time.Second,
		Now:      func() time.Time { return now },
	}, func(w http.ResponseWriter, _ *http.Request) {
		answer(w, `{"allow":true,"write":true,"ttl":86400}`)
	})
	if _, err := c2.Authorize(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	now = now.Add(11 * time.Second)
	if _, err := c2.Authorize(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if calls2.Load() != 2 {
		t.Errorf("%d requests; the endpoint held a decision past -auth-cache-ttl", calls2.Load())
	}
}

// A deploy disconnects every client at once and they all reconnect at once. If
// each reconnect were its own request, the endpoint would take the whole storm.
func TestIdenticalQuestionsAskedAtOnceMakeOneRequest(t *testing.T) {
	release := make(chan struct{})
	c, calls := newCallback(t, auth.CallbackConfig{}, func(w http.ResponseWriter, _ *http.Request) {
		<-release
		answer(w, `{"allow":true,"write":true}`)
	})

	const clients = 50
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	for range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.Authorize(context.Background(), auth.Request{Document: "d", Token: "t"})
			errs <- err
		}()
	}
	// Wait for the goroutines to pile up on the one request in flight before
	// letting it answer. Without this the test could serialise and pass for the
	// wrong reason.
	deadline := time.After(5 * time.Second)
	for calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("no request was made")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("a waiter got an error: %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("%d requests for %d simultaneous connections with the same token", calls.Load(), clients)
	}
}

// A client that gives up mid-question must not take the answer away from the
// others waiting on it, and must not leave its own giving-up in the cache.
func TestAWaiterGivingUpDoesNotCancelTheQuestion(t *testing.T) {
	release := make(chan struct{})
	c, calls := newCallback(t, auth.CallbackConfig{CacheTTL: time.Minute}, func(w http.ResponseWriter, _ *http.Request) {
		<-release
		answer(w, `{"allow":true,"write":true}`)
	})

	req := auth.Request{Document: "d", Token: "t"}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := c.Authorize(context.Background(), req)
		done <- err
	}()
	<-started
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}

	// A second client joins and then walks away.
	ctx, cancel := context.WithCancel(context.Background())
	joined := make(chan error, 1)
	go func() {
		_, err := c.Authorize(ctx, req)
		joined <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-joined; !errors.Is(err, context.Canceled) {
		t.Errorf("the client that walked away got %v", err)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the first client was refused because a second one left: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("%d requests", calls.Load())
	}
}

// The reason reaches the client inside a WebSocket permission-denied frame and
// then a close reason, which must be valid UTF-8 and free of control
// characters. A string from somebody else's server is neither by default.
func TestAReasonFromElsewhereIsMadeSafeToRepeat(t *testing.T) {
	long := strings.Repeat("x", 5000)
	c, _ := newCallback(t, auth.CallbackConfig{}, func(w http.ResponseWriter, _ *http.Request) {
		body, _ := json.Marshal(map[string]any{
			"allow":  false,
			"reason": "no\x00\x07\nway " + long,
		})
		_, _ = w.Write(body)
	})
	_, err := c.Authorize(t.Context(), auth.Request{Document: "d"})
	if !errors.Is(err, auth.ErrDenied) {
		t.Fatalf("err=%v", err)
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\x00\x07\n") {
		t.Errorf("control characters survived into %q", msg)
	}
	if len(msg) > 300 {
		t.Errorf("the reason is %d bytes; it has to fit a close frame", len(msg))
	}
}

// The cache is bounded. Without this, a server facing tokens it has never seen
// before - which is every server facing the internet - grows a map per token.
func TestTheCacheIsBounded(t *testing.T) {
	now := time.Unix(1750000000, 0)
	c, _ := newCallback(t, auth.CallbackConfig{
		CacheTTL:  time.Hour,
		CacheSize: 64,
		Now:       func() time.Time { return now },
	}, func(w http.ResponseWriter, _ *http.Request) {
		answer(w, `{"allow":true,"write":true}`)
	})
	for i := range 5000 {
		if _, err := c.Authorize(t.Context(), auth.Request{Document: "d", Token: fmt.Sprintf("t%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if n := c.CacheLen(); n > 64 {
		t.Errorf("%d decisions remembered with a size of 64", n)
	}
}

// An outage is not remembered even with the cache on: the point of fail-open is
// to end the moment the endpoint comes back.
func TestAnOutageIsNotCachedUnderFailOpen(t *testing.T) {
	var down atomic.Bool
	down.Store(true)
	c, _ := newCallback(t, auth.CallbackConfig{FailOpen: true, CacheTTL: time.Hour},
		func(w http.ResponseWriter, _ *http.Request) {
			if down.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			answer(w, `{"allow":false,"reason":"you were removed"}`)
		})

	req := auth.Request{Document: "d", Token: "t"}
	if _, err := c.Authorize(t.Context(), req); err != nil {
		t.Fatalf("fail-open refused during the outage: %v", err)
	}
	down.Store(false)
	if _, err := c.Authorize(t.Context(), req); !errors.Is(err, auth.ErrDenied) {
		t.Fatalf("err=%v; the endpoint came back and said no, and the outage was still being replayed", err)
	}
}

func TestNewCallbackRejectsAnUnusableURL(t *testing.T) {
	for _, url := range []string{"", "ftp://example.com/auth", "://nope"} {
		if _, err := auth.NewCallback(auth.CallbackConfig{URL: url, Logger: quiet()}); err == nil {
			t.Errorf("%q was accepted", url)
		}
	}
}
