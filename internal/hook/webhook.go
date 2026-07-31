package hook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mathrand "math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mesutokul/ycollab/internal/metrics"
)

// Defaults for a Webhook. They are sized for a receiver that answers in
// milliseconds and occasionally does not.
const (
	DefaultTimeout = 5 * time.Second
	DefaultQueue   = 1024
	DefaultWorkers = 4
	DefaultRetries = 2
	DefaultBackoff = 500 * time.Millisecond
	// maxBody bounds how much of a receiver's error response is read, so a
	// misconfigured URL pointing at something that streams cannot fill this
	// process's memory with a log line.
	maxBody = 4 << 10
	// userAgent identifies this server in the receiver's logs.
	userAgent = "ycollab-webhook/1"
)

// Header names. They are the interface a receiver is written against, so they
// are constants rather than string literals scattered through the sending code.
const (
	HeaderEvent     = "X-Ycollab-Event"
	HeaderDelivery  = "X-Ycollab-Delivery"
	HeaderSignature = "X-Ycollab-Signature"
)

// Config configures a Webhook.
type Config struct {
	// URL receives a POST per event. Required.
	URL string
	// Secret signs the body. Empty leaves the requests unsigned, which means a
	// receiver cannot tell this server's events from anybody else's.
	Secret []byte
	// Events selects what is sent. Empty means everything.
	Events []Kind

	// Timeout bounds one request.
	Timeout time.Duration
	// Queue is how many events may be waiting for delivery. When it fills,
	// events are dropped rather than allowed to slow the rooms down.
	Queue int
	// Workers is how many deliveries run at once. More than one matters because
	// a retry sleeps, and one stuck event should not hold up every other
	// document's.
	Workers int
	// Retries is how many times a failed delivery is tried again.
	Retries int
	// Backoff is the wait before the first retry; it doubles after that.
	Backoff time.Duration

	Client  *http.Client
	Metrics *metrics.Metrics
	Logger  *slog.Logger

	// now and sleep exist so the retry logic can be tested without waiting.
	now   func() time.Time
	sleep func(context.Context, time.Duration)
}

func (c *Config) setDefaults() {
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.Queue <= 0 {
		c.Queue = DefaultQueue
	}
	if c.Workers <= 0 {
		c.Workers = DefaultWorkers
	}
	if c.Retries < 0 {
		c.Retries = 0
	}
	if c.Backoff <= 0 {
		c.Backoff = DefaultBackoff
	}
	if c.Client == nil {
		c.Client = &http.Client{Timeout: c.Timeout}
	}
	if c.Metrics == nil {
		c.Metrics = metrics.Nop()
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.sleep == nil {
		c.sleep = sleepCtx
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// A Webhook delivers events to an HTTP endpoint.
type Webhook struct {
	cfg    Config
	log    *slog.Logger
	metric *metrics.Metrics
	want   map[Kind]bool

	queue chan Event

	wg sync.WaitGroup
	// stop is cancelled by Close, which is what wakes a worker out of a backoff
	// sleep instead of making shutdown wait for it.
	stop    context.CancelFunc
	stopped context.Context
	// closeMu makes "is the queue still open" and "send to it" one step. A
	// send on a closed channel panics, and Emit is called from every room
	// goroutine, so the check cannot be a plain bool read. It is a read lock on
	// a path that runs once per document per tick, not per update.
	closeMu  sync.RWMutex
	isClosed bool
}

// NewWebhook starts a webhook sender. Call Close to drain it.
func NewWebhook(cfg Config) (*Webhook, error) {
	if cfg.URL == "" {
		return nil, errors.New("hook: no URL")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("hook: bad URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("hook: URL scheme %q is not http or https", u.Scheme)
	}
	cfg.setDefaults()

	want := make(map[Kind]bool, len(Kinds))
	if len(cfg.Events) == 0 {
		for _, k := range Kinds {
			want[k] = true
		}
	} else {
		for _, k := range cfg.Events {
			want[k] = true
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	w := &Webhook{
		cfg:     cfg,
		log:     cfg.Logger.With("webhook", redact(u)),
		metric:  cfg.Metrics,
		want:    want,
		queue:   make(chan Event, cfg.Queue),
		stop:    cancel,
		stopped: ctx,
	}
	if len(cfg.Secret) == 0 {
		w.log.Warn("webhook requests are unsigned: set a secret so the receiver can tell they came from this server")
	} else if u.Scheme != "https" {
		w.log.Warn("webhook URL is plain http: the events, and the signature proving they are ours, cross the network in the clear")
	}
	w.wg.Add(cfg.Workers)
	for range cfg.Workers {
		go w.work()
	}
	return w, nil
}

// redact hides a URL's query and credentials, because webhook URLs are often
// the whole secret and this one is about to be attached to every log line.
func redact(u *url.URL) string {
	c := *u
	c.RawQuery = ""
	c.User = nil
	return c.String()
}

// Emit queues an event. It never blocks: a full queue drops the event and says
// so in a counter. The room goroutine calls this, and everything a client does
// is behind that goroutine, so waiting here would be waiting on the receiver
// with the document held hostage.
func (w *Webhook) Emit(e Event) {
	if !w.want[e.Kind] {
		return
	}
	w.closeMu.RLock()
	defer w.closeMu.RUnlock()
	if w.isClosed {
		return
	}
	select {
	case w.queue <- e:
		w.metric.HookQueue.Set(float64(len(w.queue)))
	default:
		w.metric.HooksDropped.Inc()
		// Not logged per event: a receiver that is down would turn every
		// keystroke in every document into a log line. The counter is the
		// signal, and the alert is on the counter.
	}
}

// Close stops accepting events, delivers what is queued, and returns when the
// workers have stopped or ctx expires.
func (w *Webhook) Close(ctx context.Context) error {
	w.closeMu.Lock()
	if w.isClosed {
		w.closeMu.Unlock()
		return nil
	}
	// Set the flag and close the queue under the same lock Emit reads it under,
	// so no send can be in flight when the channel closes.
	w.isClosed = true
	close(w.queue)
	w.closeMu.Unlock()

	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		// Out of time. Cancelling the workers aborts the request in flight and
		// makes them drop the rest of the queue, so the process is not held
		// open past the deadline it was given.
		w.stop()
		<-done
		return ctx.Err()
	}
}

func (w *Webhook) work() {
	defer w.wg.Done()
	for e := range w.queue {
		// Close cancels this once it has run out of patience, and then the rest
		// of the queue is abandoned rather than delivered. Without the check a
		// forced shutdown still walks the whole queue, paying a request timeout
		// per event: with a receiver that is not answering, that turned a ten
		// second shutdown deadline into a minute. Measured, not theorised.
		if w.stopped.Err() != nil {
			w.metric.HooksDropped.Inc()
			continue
		}
		w.metric.HookQueue.Set(float64(len(w.queue)))
		w.deliver(e)
	}
}

// deliver sends one event, retrying the failures that are worth retrying.
func (w *Webhook) deliver(e Event) {
	body, err := json.Marshal(payloadOf(e))
	if err != nil {
		// Nothing in Event is unmarshalable, so this is a programming error
		// rather than a delivery one.
		w.metric.HooksFailed.WithLabelValues(string(e.Kind), "encode").Inc()
		w.log.Error("could not encode a hook event", "err", err)
		return
	}
	// One delivery id for every attempt at this event, so a receiver that got a
	// request whose response we never saw can recognise the retry as the same
	// event rather than a second one.
	delivery := newDeliveryID()

	backoff := w.cfg.Backoff
	for attempt := 0; ; attempt++ {
		retryable, err := w.post(e, body, delivery)
		if err == nil {
			w.metric.HooksSent.WithLabelValues(string(e.Kind)).Inc()
			return
		}
		if !retryable || attempt >= w.cfg.Retries || w.stopped.Err() != nil {
			w.metric.HooksFailed.WithLabelValues(string(e.Kind), reasonOf(err)).Inc()
			w.log.Warn("giving up on a hook event",
				"event", e.Kind, "doc", e.Doc, "delivery", delivery, "attempts", attempt+1, "err", err)
			return
		}
		// Jitter, so a receiver that just came back does not get every replica's
		// retries in the same millisecond.
		wait := backoff + time.Duration(mathrand.Int64N(int64(backoff/2)+1))
		w.cfg.sleep(w.stopped, wait)
		backoff *= 2
	}
}

// post makes one request. It reports whether the failure is worth another try.
func (w *Webhook) post(e Event, body []byte, delivery string) (retryable bool, err error) {
	// Derived from w.stopped, not from Background: a forced shutdown has to
	// abort the request in flight, or the process waits out this timeout after
	// having already decided not to.
	ctx, cancel := context.WithTimeout(w.stopped, w.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set(HeaderEvent, string(e.Kind))
	req.Header.Set(HeaderDelivery, delivery)
	if len(w.cfg.Secret) > 0 {
		req.Header.Set(HeaderSignature, Sign(w.cfg.Secret, w.cfg.now(), body))
	}

	started := w.cfg.now()
	w.metric.HookAttempts.Inc()
	resp, err := w.cfg.Client.Do(req)
	metrics.Observe(w.metric.HookDuration, started)
	if err != nil {
		// Could not reach it, or it did not answer in time. Both are worth
		// another try; neither says anything about the event itself.
		return true, transportError{err}
	}
	defer resp.Body.Close()
	// The body has to be read for the connection to be reused, and a receiver
	// that explains itself is worth quoting in the log.
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	_, _ = io.Copy(io.Discard, resp.Body)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return false, nil
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		// Overloaded or broken, not wrong. Try again.
		return true, statusError{resp.StatusCode, string(bytes.TrimSpace(snippet))}
	default:
		// 4xx is the receiver saying the request is wrong. Repeating it
		// unchanged would only be wrong again.
		return false, statusError{resp.StatusCode, string(bytes.TrimSpace(snippet))}
	}
}

type transportError struct{ err error }

func (e transportError) Error() string { return e.err.Error() }
func (e transportError) Unwrap() error { return e.err }

type statusError struct {
	code    int
	message string
}

func (e statusError) Error() string {
	if e.message == "" {
		return "http " + strconv.Itoa(e.code)
	}
	return "http " + strconv.Itoa(e.code) + ": " + e.message
}

// reasonOf turns a failure into a metric label from a fixed set, because a
// label taken from a remote server's response is an unbounded label.
func reasonOf(err error) string {
	var s statusError
	if errors.As(err, &s) {
		switch {
		case s.code >= 500:
			return "server_error"
		case s.code == http.StatusTooManyRequests:
			return "throttled"
		default:
			return "rejected"
		}
	}
	return "transport"
}

// payload is the JSON body. It is a separate type from Event so the wire format
// is written down in one place and does not drift when a field is added to the
// internal struct.
type payload struct {
	Event       Kind   `json:"event"`
	Document    string `json:"document"`
	At          string `json:"at"`
	Node        uint64 `json:"node,omitempty"`
	Clients     int    `json:"clients"`
	Updates     uint64 `json:"updates"`
	StateVector string `json:"state_vector,omitempty"`
	State       string `json:"state,omitempty"`
}

func payloadOf(e Event) payload {
	return payload{
		Event:    e.Kind,
		Document: e.Doc,
		// RFC 3339 with nanoseconds, in UTC: a receiver ordering events from
		// three replicas needs more than second resolution.
		At:      e.At.UTC().Format(time.RFC3339Nano),
		Node:    e.Node,
		Clients: e.Clients,
		Updates: e.Updates,
		// Base64 rather than hex: these are Yjs updates, and the JavaScript on
		// the other end feeds them to Y.applyUpdate after one atob.
		StateVector: encode(e.StateVector),
		State:       encode(e.State),
	}
}

func encode(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// Sign returns the value of the signature header for a body.
//
// The timestamp is inside the signed text, not just beside it, so a captured
// request cannot be replayed later with a fresh timestamp: changing it breaks
// the signature. A receiver should reject a timestamp far from its own clock
// for the same reason.
func Sign(secret []byte, at time.Time, body []byte) string {
	t := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(t))
	mac.Write([]byte("."))
	mac.Write(body)
	return "t=" + t + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a signature header against a body. It is what a receiver
// written in Go would do, and what this package's tests use, so the format has
// exactly one reader and one writer.
//
// tolerance rejects a signature whose timestamp is too far from now; zero
// accepts any age.
func Verify(secret []byte, header string, body []byte, now time.Time, tolerance time.Duration) error {
	var ts, sig string
	for part := range strings.SplitSeq(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			sig = v
		}
	}
	if ts == "" || sig == "" {
		return errors.New("hook: malformed signature header")
	}
	secs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errors.New("hook: bad signature timestamp")
	}
	if tolerance > 0 {
		age := now.Sub(time.Unix(secs, 0))
		if age < -tolerance || age > tolerance {
			return fmt.Errorf("hook: signature is %s old", age)
		}
	}
	want, err := hex.DecodeString(sig)
	if err != nil {
		return errors.New("hook: signature is not hex")
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	if !hmac.Equal(want, mac.Sum(nil)) {
		return errors.New("hook: signature does not match")
	}
	return nil
}

func newDeliveryID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any platform this runs on, and a
		// delivery id is a label rather than a secret; a clock reading is a
		// good enough fallback to keep this from being a panic.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}
