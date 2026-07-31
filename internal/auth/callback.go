package auth

// This file is the other way of answering "may this connection open this
// document": ask the application.
//
// The JWT in auth.go is a capability - it names one document and it is signed,
// so this server can decide on its own, with no network and no shared state.
// That is the right shape when the application can mint a token at the moment
// it hands the user a document. It is the wrong shape in three cases that keep
// coming up:
//
//   - the application already has a session cookie and no token endpoint, so
//     adding one is work before anything can be tried at all;
//   - permissions change while a document is open, and a capability that was
//     minted five minutes ago says nothing about that;
//   - subdocuments. Yjs opens a provider per guid, and each guid is a separate
//     document name here, so a per-document capability means the application
//     mints a token for a name it only learns about when Yjs tells it - a round
//     trip in the middle of loading a document.
//
// A callback costs a request per connection and couples this server's
// availability to the application's. Both are real, and both are why the cache,
// the single flight and the fail-open switch below exist.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mesutokul/ycollab/internal/metrics"
	"github.com/mesutokul/ycollab/internal/signature"
)

// Defaults for a Callback.
const (
	// DefaultCallbackTimeout bounds one request. It is short on purpose: a
	// client is holding an open socket waiting for this answer, and a slow
	// authorisation service should look like a refusal rather than a hang.
	DefaultCallbackTimeout = 3 * time.Second
	// DefaultCacheSize bounds how many decisions are remembered at once.
	DefaultCacheSize = 4096
	// callbackUserAgent identifies this server in the receiver's logs.
	callbackUserAgent = "ycollab-auth/1"
	// maxCallbackBody bounds the response that is read, so a URL pointing at
	// something that streams cannot fill this process's memory.
	maxCallbackBody = 8 << 10
	// maxReason bounds the text taken from the endpoint and said to the client.
	maxReason = 200
)

// HeaderSignature carries the HMAC over the request body, so the endpoint can
// tell this server's questions from anybody else's. Without it, the
// authorisation endpoint is a service that answers "does this token work" to
// whoever asks - which is an oracle.
const HeaderSignature = "X-Ycollab-Signature"

// ErrUnavailable is what a Callback returns when it could not get an answer:
// the endpoint timed out, refused the connection, or replied 5xx. It is
// separate from ErrInvalidToken because it is the server's problem, not the
// client's, and only the operator can fix it.
var ErrUnavailable = errors.New("authorization service unavailable")

// ErrDenied is a refusal the endpoint made deliberately. Its wrapped text is
// whatever reason the endpoint gave, and it goes to the client.
var ErrDenied = errors.New("denied")

// CallbackConfig configures a Callback.
type CallbackConfig struct {
	// URL is asked one POST per authorisation. Required.
	URL string
	// Secret signs the request body. Empty leaves the requests unsigned, which
	// means the endpoint cannot tell this server from anyone else who can reach
	// it.
	Secret []byte
	// Timeout bounds one request.
	Timeout time.Duration

	// CacheTTL is how long a decision is reused for the same token and
	// document. Zero disables the cache, which is the default: a decision that
	// is remembered is a revocation that has not taken effect yet.
	//
	// It is also a ceiling. An endpoint may return a shorter ttl to say "ask me
	// again sooner"; a longer one is capped here, because how much staleness
	// this deployment tolerates is the operator's call, not the endpoint's.
	CacheTTL time.Duration
	// CacheSize bounds the number of remembered decisions.
	CacheSize int

	// FailOpen decides what an unreachable endpoint means. False - the default -
	// refuses the connection: if we cannot tell who somebody is, serving them a
	// document is worse than not serving it. True keeps documents readable and
	// writable through an authorisation outage, which some deployments want and
	// which must be a deliberate choice.
	FailOpen bool

	Client  *http.Client
	Metrics *metrics.Metrics
	Logger  *slog.Logger

	// Now is the clock, injectable so the cache's lifetime can be tested
	// without sleeping. It matches auth.Config, which does the same for expiry.
	Now func() time.Time
}

// A Request is what the callback is asked about, and - through its tags - the
// JSON body the endpoint receives. One type rather than two, so that a field
// added here cannot be a field the endpoint never gets told about.
type Request struct {
	// Document is the name from the URL. For a subdocument this is its guid.
	Document string `json:"document"`
	// Token is whatever the client presented, verbatim and unverified. It may
	// be empty.
	Token string `json:"token,omitempty"`
	// IP is the client address as this server resolved it, honouring
	// -trusted-proxies.
	IP string `json:"ip,omitempty"`
	// Origin is the browser's Origin header, if any.
	Origin string `json:"origin,omitempty"`
}

// callbackResponse is the JSON the endpoint answers with.
//
// Allow is a pointer because its absence has to be distinguishable from false.
// An endpoint that answers 200 with a body this server cannot read has not said
// "yes" and has not said "no", and guessing either way is worse than saying the
// answer was unreadable.
type callbackResponse struct {
	Allow   *bool  `json:"allow"`
	Write   bool   `json:"write"`
	Subject string `json:"subject"`
	// Owner is the tenant this connection belongs to. The endpoint knows it -
	// it is the thing that just looked the user up - so this is where it
	// belongs; a token would have to carry it and be minted per tenant.
	Owner  string `json:"owner"`
	Reason string `json:"reason"`
	// TTL is seconds this decision may be reused for. It only ever shortens the
	// configured cache lifetime.
	TTL int `json:"ttl"`
}

// A Callback asks an HTTP endpoint whether a connection is allowed.
//
// It is safe for concurrent use, and it is meant to be: every WebSocket upgrade
// goes through one.
type Callback struct {
	cfg    CallbackConfig
	log    *slog.Logger
	metric *metrics.Metrics

	mu sync.Mutex
	// decided is the cache. It is only read and written under mu, which is held
	// for map operations and never across a request.
	decided map[string]decision
	// asking is the single flight: one entry per question currently in the air.
	// A reconnect storm after a deploy is many clients asking the same question
	// at the same moment, and without this each one is its own request to an
	// endpoint that is already the slowest thing in the path.
	asking map[string]*inflight
}

type decision struct {
	grant   Grant
	err     error
	expires time.Time
}

// An answer is one round trip's outcome. Whether it may be remembered is part
// of the answer rather than something the caller infers, because the two cases
// that must never be cached - a fail-open grant, and a client walking away
// mid-question - both look like "no error" from outside.
type answer struct {
	grant Grant
	err   error
	// ttl is the lifetime the endpoint asked for, zero if it asked for none.
	ttl time.Duration
	// cacheable is false for anything that is not a decision about this token.
	cacheable bool
}

type inflight struct {
	done chan struct{}
	answer
}

// NewCallback returns a Callback, or an error if the URL is unusable.
func NewCallback(cfg CallbackConfig) (*Callback, error) {
	if cfg.URL == "" {
		return nil, errors.New("auth: no callback URL")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("auth: bad callback URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("auth: callback URL scheme %q is not http or https", u.Scheme)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultCallbackTimeout
	}
	if cfg.CacheSize <= 0 {
		cfg.CacheSize = DefaultCacheSize
	}
	if cfg.CacheTTL < 0 {
		cfg.CacheTTL = 0
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: cfg.Timeout}
	}
	if cfg.Metrics == nil {
		cfg.Metrics = metrics.Nop()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	c := &Callback{
		cfg:     cfg,
		log:     cfg.Logger.With("auth_callback", redactURL(u)),
		metric:  cfg.Metrics,
		decided: make(map[string]decision),
		asking:  make(map[string]*inflight),
	}
	if len(cfg.Secret) == 0 {
		c.log.Warn("authorization requests are unsigned: set a secret so the endpoint can tell they came from this server")
	} else if u.Scheme != "https" {
		c.log.Warn("authorization URL is plain http: the tokens this server forwards cross the network in the clear")
	}
	if cfg.FailOpen {
		c.log.Warn("fail-open authorization: while the endpoint is unreachable every client may read and write every document")
	}
	return c, nil
}

// redactURL hides a URL's query and credentials before it goes on every log
// line, the same way the webhook does: these URLs are often the whole secret.
func redactURL(u *url.URL) string {
	c := *u
	c.RawQuery = ""
	c.User = nil
	return c.String()
}

// Authorize answers whether req may proceed.
//
// The error is written to the client in a permission-denied frame, so it says
// what is wrong and nothing about why.
func (c *Callback) Authorize(ctx context.Context, req Request) (Grant, error) {
	key := req.Document + "\x00" + req.Token

	if grant, err, ok := c.cached(key); ok {
		c.metric.AuthCache.WithLabelValues("hit").Inc()
		return grant, err
	}
	c.metric.AuthCache.WithLabelValues("miss").Inc()

	// Join whoever is already asking this exact question, or become the asker.
	c.mu.Lock()
	if f, ok := c.asking[key]; ok {
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.grant, f.err
		case <-ctx.Done():
			// This client gave up; the request it was waiting on carries on for
			// whoever else is waiting.
			return Grant{}, ctx.Err()
		}
	}
	f := &inflight{done: make(chan struct{})}
	c.asking[key] = f
	c.mu.Unlock()

	f.answer = c.ask(ctx, req)

	c.mu.Lock()
	delete(c.asking, key)
	if c.cfg.CacheTTL > 0 && f.cacheable {
		c.remember(key, f.grant, f.err, f.ttl)
	}
	c.mu.Unlock()
	close(f.done)
	return f.grant, f.err
}

// cached returns a remembered decision if there is a live one.
func (c *Callback) cached(key string) (Grant, error, bool) {
	if c.cfg.CacheTTL <= 0 {
		return Grant{}, nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.decided[key]
	if !ok {
		return Grant{}, nil, false
	}
	if !c.cfg.Now().Before(d.expires) {
		delete(c.decided, key)
		return Grant{}, nil, false
	}
	return d.grant, d.err, true
}

// remember stores a decision. It is called with mu held.
//
// Eviction is a sweep of what has expired, and a wholesale clear only if that
// freed nothing. That is cruder than an LRU and deliberately so: the cache is a
// stampede guard with a lifetime measured in seconds, not a hit-rate
// optimisation, and an LRU's bookkeeping would be on the connection path.
func (c *Callback) remember(key string, grant Grant, err error, ttl time.Duration) {
	life := c.cfg.CacheTTL
	if ttl > 0 && ttl < life {
		life = ttl
	}
	now := c.cfg.Now()
	if len(c.decided) >= c.cfg.CacheSize {
		for k, d := range c.decided {
			if !now.Before(d.expires) {
				delete(c.decided, k)
			}
		}
		if len(c.decided) >= c.cfg.CacheSize {
			clear(c.decided)
		}
	}
	c.decided[key] = decision{grant: grant, err: err, expires: now.Add(life)}
}

// ask makes the request.
func (c *Callback) ask(ctx context.Context, req Request) answer {
	body, err := json.Marshal(req)
	if err != nil {
		// A document name is a string and a token is a string; there is nothing
		// here encoding/json can refuse. Reported rather than ignored so that a
		// future field which can does not fail silently.
		c.metric.AuthRequests.WithLabelValues("error").Inc()
		c.log.Error("could not encode an authorization request", "err", err)
		return c.unavailable()
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(body))
	if err != nil {
		c.metric.AuthRequests.WithLabelValues("error").Inc()
		return c.unavailable()
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", callbackUserAgent)
	if len(c.cfg.Secret) > 0 {
		httpReq.Header.Set(HeaderSignature, signature.Sign(c.cfg.Secret, c.cfg.Now(), body))
	}

	start := c.cfg.Now()
	resp, err := c.cfg.Client.Do(httpReq)
	c.metric.AuthDuration.Observe(c.cfg.Now().Sub(start).Seconds())
	if err != nil {
		// A cancelled parent context is this client going away, not the endpoint
		// failing, and must not be reported as an outage, answered by the
		// fail-open policy, or remembered.
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(ctxErr, context.Canceled) {
			return answer{err: ctxErr}
		}
		c.metric.AuthRequests.WithLabelValues("error").Inc()
		c.log.Warn("authorization request failed", "doc", req.Document, "err", err)
		return c.unavailable()
	}
	defer resp.Body.Close()
	payload, readErr := io.ReadAll(io.LimitReader(resp.Body, maxCallbackBody))

	switch {
	case resp.StatusCode == http.StatusOK:
		// Handled below.
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		// A deliberate refusal. The endpoint may explain itself in a JSON body
		// or in plain text; either way the text reaches the client.
		c.metric.AuthRequests.WithLabelValues("deny").Inc()
		return answer{err: denied(reasonFrom(payload)), cacheable: true}
	case resp.StatusCode == http.StatusRequestTimeout,
		resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode >= 500:
		c.metric.AuthRequests.WithLabelValues("error").Inc()
		c.log.Warn("authorization endpoint is failing", "doc", req.Document, "status", resp.StatusCode)
		return c.unavailable()
	default:
		// 404, 400, a redirect, a 204: the endpoint is reachable and answering
		// something this server was not built to read. That is a
		// misconfiguration, not an outage, and it is refused even under
		// fail-open - otherwise a typo in the URL is a server that lets
		// everybody in and logs a warning about it.
		//
		// It is not remembered either: the fix is a redeploy of the endpoint,
		// and a cache would keep refusing for a lifetime after it landed.
		c.metric.AuthRequests.WithLabelValues("misconfigured").Inc()
		c.log.Error("authorization endpoint answered a status this server cannot act on; refusing regardless of the fail-open setting",
			"status", resp.StatusCode)
		return answer{err: denied("")}
	}

	if readErr != nil {
		c.metric.AuthRequests.WithLabelValues("error").Inc()
		c.log.Warn("could not read the authorization response", "err", readErr)
		return c.unavailable()
	}
	var decoded callbackResponse
	if err := json.Unmarshal(payload, &decoded); err != nil || decoded.Allow == nil {
		// Same reasoning as the default branch above: an unreadable answer is a
		// bug in the endpoint, and fail-open is for an endpoint that is down.
		c.metric.AuthRequests.WithLabelValues("misconfigured").Inc()
		c.log.Error(`authorization endpoint answered 200 without a usable {"allow": bool}; refusing regardless of the fail-open setting`)
		return answer{err: denied("")}
	}

	ttl := time.Duration(decoded.TTL) * time.Second
	if !*decoded.Allow {
		c.metric.AuthRequests.WithLabelValues("deny").Inc()
		return answer{err: denied(decoded.Reason), ttl: ttl, cacheable: true}
	}
	c.metric.AuthRequests.WithLabelValues("allow").Inc()
	return answer{
		grant: Grant{
			Subject: sanitize(decoded.Subject),
			Doc:     req.Document,
			Owner:   sanitize(decoded.Owner),
			Write:   decoded.Write,
		},
		ttl:       ttl,
		cacheable: true,
	}
}

// unavailable applies the fail-open policy to an endpoint that did not answer.
//
// Neither outcome is cacheable. Fail-open is a state to leave the moment the
// endpoint comes back, and a refusal caused by an outage is not a decision about
// the token - remembering either would make the outage outlast itself.
func (c *Callback) unavailable() answer {
	if c.cfg.FailOpen {
		return answer{grant: Grant{Write: true}}
	}
	return answer{err: ErrUnavailable}
}

// denied wraps a reason into an error whose text is safe to say to a client.
func denied(reason string) error {
	reason = sanitize(reason)
	if reason == "" {
		return ErrDenied
	}
	return fmt.Errorf("%w: %s", ErrDenied, reason)
}

// reasonFrom pulls an explanation out of a refusal body, which may be JSON or
// may be whatever the endpoint's framework writes for a 403.
func reasonFrom(payload []byte) string {
	var answer callbackResponse
	if err := json.Unmarshal(payload, &answer); err == nil && answer.Reason != "" {
		return answer.Reason
	}
	if json.Valid(payload) {
		// Valid JSON with no reason in it: saying "{"error":true}" to a user
		// helps nobody.
		return ""
	}
	return string(payload)
}

// sanitize makes text from the endpoint safe to put in a WebSocket frame and a
// close reason. Control characters and invalid UTF-8 are the two ways a string
// from elsewhere breaks a close frame, and a close frame that breaks replaces a
// stated reason with an abrupt disconnect.
func sanitize(s string) string {
	if len(s) > maxReason {
		s = s[:maxReason]
	}
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if !utf8.ValidString(s) {
		return ""
	}
	return s
}

// OriginFromRequest is the Origin header, which the endpoint may want in order
// to tell a first-party page from an embedded one.
func OriginFromRequest(r *http.Request) string { return r.Header.Get("Origin") }

// CacheLen is how many decisions are currently remembered. It exists so the
// bound on the cache can be asserted rather than assumed: an unbounded map keyed
// by token is a map that grows with every token a server on the internet is
// shown.
func (c *Callback) CacheLen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.decided)
}
