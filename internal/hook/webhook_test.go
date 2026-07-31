package hook_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/mesutokul/ycollab/internal/hook"
	"github.com/mesutokul/ycollab/internal/metrics"
)

// A receiver records what arrived, so a test can assert on the request rather
// than on the sender's own idea of what it sent.
type receiver struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []request
	// answer decides the status code for the nth request.
	answer func(n int) int
	// hold, when set, blocks the handler until it is closed.
	hold chan struct{}
	seen chan struct{}
}

type request struct {
	body      []byte
	event     string
	delivery  string
	signature string
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{
		answer: func(int) int { return http.StatusOK },
		seen:   make(chan struct{}, 256),
	}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		n := len(r.requests)
		r.requests = append(r.requests, request{
			body:      body,
			event:     req.Header.Get(hook.HeaderEvent),
			delivery:  req.Header.Get(hook.HeaderDelivery),
			signature: req.Header.Get(hook.HeaderSignature),
		})
		answer, hold := r.answer, r.hold
		r.mu.Unlock()
		select {
		case r.seen <- struct{}{}:
		default:
		}
		if hold != nil {
			<-hold
		}
		w.WriteHeader(answer(n))
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) all() []request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]request(nil), r.requests...)
}

// wait blocks until n requests have arrived, or fails.
func (r *receiver) wait(t *testing.T, n int) []request {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		if got := r.all(); len(got) >= n {
			return got
		}
		select {
		case <-r.seen:
		case <-deadline:
			t.Fatalf("only %d of %d requests arrived", len(r.all()), n)
		}
	}
}

func newWebhook(t *testing.T, cfg hook.Config) (*hook.Webhook, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	cfg.Metrics = metrics.New(reg)
	w, err := hook.NewWebhook(cfg)
	if err != nil {
		t.Fatalf("NewWebhook: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = w.Close(ctx)
	})
	return w, reg
}

func sampleEvent() hook.Event {
	return hook.Event{
		Doc:         "notes",
		Kind:        hook.KindChange,
		At:          time.Unix(1750000000, 0),
		Node:        7,
		Clients:     3,
		Updates:     12,
		StateVector: []byte{1, 2, 3},
		State:       []byte{4, 5},
	}
}

// The receiver is meant to be able to prove a request came from this server, so
// the signature is checked the way a receiver would check it, against the exact
// bytes that arrived.
func TestDeliversASignedEvent(t *testing.T) {
	rec := newReceiver(t)
	secret := []byte("shh")
	w, reg := newWebhook(t, hook.Config{URL: rec.srv.URL, Secret: secret})

	w.Emit(sampleEvent())
	got := rec.wait(t, 1)[0]

	if got.event != string(hook.KindChange) {
		t.Errorf("event header is %q", got.event)
	}
	if len(got.delivery) != 32 {
		t.Errorf("delivery id is %q, want 32 hex characters", got.delivery)
	}
	if err := hook.Verify(secret, got.signature, got.body, time.Now(), time.Minute); err != nil {
		t.Errorf("a receiver could not verify the request: %v", err)
	}

	var body struct {
		Event       string `json:"event"`
		Document    string `json:"document"`
		At          string `json:"at"`
		Node        uint64 `json:"node"`
		Clients     int    `json:"clients"`
		Updates     uint64 `json:"updates"`
		StateVector string `json:"state_vector"`
		State       string `json:"state"`
	}
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, got.body)
	}
	if body.Event != "document.change" || body.Document != "notes" {
		t.Errorf("body says %q on %q", body.Event, body.Document)
	}
	if body.Node != 7 || body.Clients != 3 || body.Updates != 12 {
		t.Errorf("counts are node=%d clients=%d updates=%d", body.Node, body.Clients, body.Updates)
	}
	if _, err := time.Parse(time.RFC3339Nano, body.At); err != nil {
		t.Errorf("at is %q: %v", body.At, err)
	}
	// The updates are base64 because the receiver hands them to Y.applyUpdate.
	sv, err := base64.StdEncoding.DecodeString(body.StateVector)
	if err != nil || string(sv) != string([]byte{1, 2, 3}) {
		t.Errorf("state_vector decoded to %v (%v)", sv, err)
	}
	state, err := base64.StdEncoding.DecodeString(body.State)
	if err != nil || string(state) != string([]byte{4, 5}) {
		t.Errorf("state decoded to %v (%v)", state, err)
	}

	if err := drain(w); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(collector(t, reg, "ycollab_hooks_sent_total")); got != 1 {
		t.Errorf("hooks_sent_total is %v, want 1", got)
	}
}

// A tampered body, a wrong key and a replayed request all have to fail, or the
// signature is decoration.
func TestVerifyRejectsWhatItShould(t *testing.T) {
	secret := []byte("shh")
	body := []byte(`{"event":"document.change"}`)
	now := time.Unix(1750000000, 0)
	sig := hook.Sign(secret, now, body)

	if err := hook.Verify(secret, sig, body, now, time.Minute); err != nil {
		t.Fatalf("the honest case failed: %v", err)
	}
	for _, tc := range []struct {
		name   string
		secret []byte
		sig    string
		body   []byte
		now    time.Time
	}{
		{"tampered body", secret, sig, []byte(`{"event":"document.store"}`), now},
		{"wrong key", []byte("other"), sig, body, now},
		{"replayed later", secret, sig, body, now.Add(time.Hour)},
		{"missing signature", secret, "t=1750000000", body, now},
		{"not hex", secret, "t=1750000000,v1=zz", body, now},
		{"empty header", secret, "", body, now},
	} {
		if err := hook.Verify(tc.secret, tc.sig, tc.body, tc.now, time.Minute); err == nil {
			t.Errorf("%s was accepted", tc.name)
		}
	}
	// The timestamp is signed, so moving it invalidates the signature rather
	// than extending its life.
	moved := "t=" + "1750003600" + sig[len("t=1750000000"):]
	if err := hook.Verify(secret, moved, body, now.Add(time.Hour), time.Minute); err == nil {
		t.Error("a request replayed with a fresh timestamp was accepted")
	}
}

// A receiver that is overloaded gets another chance; one that says the request
// is wrong does not, because repeating it unchanged would only be wrong again.
func TestRetriesServerErrorsButNotRejections(t *testing.T) {
	t.Run("server error", func(t *testing.T) {
		rec := newReceiver(t)
		rec.answer = func(n int) int {
			if n < 2 {
				return http.StatusInternalServerError
			}
			return http.StatusOK
		}
		w, reg := newWebhook(t, hook.Config{URL: rec.srv.URL, Retries: 3, Backoff: time.Millisecond})
		w.Emit(sampleEvent())
		got := rec.wait(t, 3)
		if err := drain(w); err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Fatalf("%d requests, want exactly 3", len(got))
		}
		// The retries are the same event, so the receiver can recognise them.
		if got[0].delivery != got[1].delivery || got[1].delivery != got[2].delivery {
			t.Error("the retries carried different delivery ids")
		}
		if v := testutil.ToFloat64(collector(t, reg, "ycollab_hooks_sent_total")); v != 1 {
			t.Errorf("hooks_sent_total is %v, want 1", v)
		}
		if v := testutil.ToFloat64(collector(t, reg, "ycollab_hook_attempts_total")); v != 3 {
			t.Errorf("hook_attempts_total is %v, want 3", v)
		}
	})

	t.Run("rejection", func(t *testing.T) {
		rec := newReceiver(t)
		rec.answer = func(int) int { return http.StatusBadRequest }
		w, reg := newWebhook(t, hook.Config{URL: rec.srv.URL, Retries: 3, Backoff: time.Millisecond})
		w.Emit(sampleEvent())
		rec.wait(t, 1)
		if err := drain(w); err != nil {
			t.Fatal(err)
		}
		if got := rec.all(); len(got) != 1 {
			t.Fatalf("%d requests, want exactly 1", len(got))
		}
		if v := testutil.ToFloat64(collector(t, reg, "ycollab_hooks_failed_total")); v != 1 {
			t.Errorf("hooks_failed_total is %v, want 1", v)
		}
	})

	t.Run("gives up", func(t *testing.T) {
		rec := newReceiver(t)
		rec.answer = func(int) int { return http.StatusServiceUnavailable }
		w, _ := newWebhook(t, hook.Config{URL: rec.srv.URL, Retries: 2, Backoff: time.Millisecond})
		w.Emit(sampleEvent())
		rec.wait(t, 3)
		if err := drain(w); err != nil {
			t.Fatal(err)
		}
		if got := rec.all(); len(got) != 3 {
			t.Fatalf("%d requests, want 1 attempt plus 2 retries", len(got))
		}
	})
}

// The whole point of the queue: a receiver that has stopped answering must cost
// the rooms nothing. Emit is called from the room goroutine, and everything
// every connected client does is behind that goroutine.
func TestAFullQueueDropsRatherThanBlocking(t *testing.T) {
	rec := newReceiver(t)
	rec.hold = make(chan struct{})
	defer close(rec.hold)

	w, reg := newWebhook(t, hook.Config{
		URL: rec.srv.URL, Queue: 2, Workers: 1, Retries: 0, Timeout: time.Hour,
	})

	// One event will be in the handler, blocked; two more fit in the queue;
	// everything after that has to be dropped.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			w.Emit(sampleEvent())
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Emit blocked: a slow receiver is stalling the room goroutine")
	}

	dropped := testutil.ToFloat64(collector(t, reg, "ycollab_hooks_dropped_total"))
	if dropped == 0 {
		t.Fatal("nothing was dropped, so something absorbed 200 events")
	}
	if dropped > 200 {
		t.Fatalf("dropped %v of 200", dropped)
	}
}

// Shutdown has to deliver what was accepted: the last change event a room emits
// as it evicts is the one a receiver most wants.
func TestCloseDeliversWhatIsQueued(t *testing.T) {
	rec := newReceiver(t)
	w, err := hook.NewWebhook(hook.Config{URL: rec.srv.URL, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		w.Emit(sampleEvent())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := rec.all(); len(got) != 20 {
		t.Fatalf("%d of 20 events were delivered", len(got))
	}
	// Closing twice is what happens when a shutdown path runs twice; it must
	// not panic on an already-closed channel.
	if err := w.Close(ctx); err != nil {
		t.Errorf("second Close: %v", err)
	}
	// And an event emitted after the close is dropped rather than sent on a
	// closed channel.
	w.Emit(sampleEvent())
}

// A deployment that only wants to know about writes should not be sent a
// request on every tick of every document being typed into.
func TestOnlySelectedEventsAreSent(t *testing.T) {
	rec := newReceiver(t)
	w, _ := newWebhook(t, hook.Config{URL: rec.srv.URL, Events: []hook.Kind{hook.KindStore}})

	w.Emit(sampleEvent())
	stored := sampleEvent()
	stored.Kind = hook.KindStore
	w.Emit(stored)

	got := rec.wait(t, 1)
	if err := drain(w); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("%d requests, want only the store event", len(got))
	}
	if got[0].event != string(hook.KindStore) {
		t.Errorf("delivered %q", got[0].event)
	}
}

// A URL that cannot work should be refused at startup, where somebody is
// watching, rather than at the first edit.
func TestRejectsAConfigurationThatCannotWork(t *testing.T) {
	for _, url := range []string{"", "ftp://example.invalid/hook", "://nope"} {
		if _, err := hook.NewWebhook(hook.Config{URL: url}); err == nil {
			t.Errorf("NewWebhook accepted %q", url)
		}
	}
}

// An endpoint that is not there must not turn into a stuck worker.
func TestAnUnreachableEndpointIsCountedAndDropped(t *testing.T) {
	w, reg := newWebhook(t, hook.Config{
		// Port 0 is not connectable, so this fails in the dial rather than
		// waiting for a timeout.
		URL: "http://127.0.0.1:0/hook", Retries: 1, Backoff: time.Millisecond,
	})
	w.Emit(sampleEvent())
	if err := drain(w); err != nil {
		t.Fatal(err)
	}
	if v := testutil.ToFloat64(collector(t, reg, "ycollab_hooks_failed_total")); v != 1 {
		t.Errorf("hooks_failed_total is %v, want 1", v)
	}
}

// Two rooms emitting at once is the normal case, and Emit is on the room
// goroutine, so the race detector has to see it concurrent.
func TestConcurrentEmitters(t *testing.T) {
	rec := newReceiver(t)
	w, _ := newWebhook(t, hook.Config{URL: rec.srv.URL, Queue: 64})
	var wg sync.WaitGroup
	var sent atomic.Int64
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				w.Emit(sampleEvent())
				sent.Add(1)
			}
		}()
	}
	wg.Wait()
	if err := drain(w); err != nil {
		t.Fatal(err)
	}
	if sent.Load() != 200 {
		t.Fatalf("emitted %d", sent.Load())
	}
}

// drain closes a webhook and waits for its queue.
func drain(w *hook.Webhook) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return w.Close(ctx)
}

// collector finds a registered metric by name so a test can read its value.
func collector(t *testing.T, reg *prometheus.Registry, name string) prometheus.Collector {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var total float64
	found := false
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		found = true
		for _, m := range f.GetMetric() {
			total += m.GetCounter().GetValue() + m.GetGauge().GetValue()
		}
	}
	if !found {
		t.Fatalf("%s was never recorded", name)
	}
	// testutil.ToFloat64 wants a collector, and a CounterVec's total is what
	// these tests are asserting on, so it is handed a fresh gauge holding it.
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: name})
	g.Set(total)
	return g
}
