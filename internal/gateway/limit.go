package gateway

import (
	"context"
	"time"
)

// Rate bounds how fast one connection may send.
//
// Two dimensions, because they are two different attacks and two different
// mistakes: a client can flood with tiny updates, which costs CPU per message,
// or with large ones, which costs memory and fanout bandwidth. A limit on
// either alone leaves the other open.
type Rate struct {
	// Messages is the sustained rate in messages per second, and MessageBurst
	// how many may arrive at once. Zero disables the message limit.
	Messages     float64
	MessageBurst int
	// Bytes is the sustained rate in bytes per second, and ByteBurst the size of
	// a burst. Zero disables the byte limit.
	Bytes     float64
	ByteBurst int
}

// Defaults, chosen against what a person editing actually produces.
//
// y-prosemirror sends one update per transaction, so typing is on the order of
// ten to thirty messages a second, and a large paste is one big message rather
// than many. These are an order of magnitude above that: they are here to stop a
// client that has gone wrong, not to shape normal traffic.
const (
	DefaultRateMessages     = 200
	DefaultRateMessageBurst = 400
	DefaultRateBytes        = 8 << 20
	DefaultRateByteBurst    = 16 << 20
)

func (r Rate) withDefaults() Rate {
	if r.Messages == 0 {
		r.Messages = DefaultRateMessages
	}
	if r.MessageBurst <= 0 {
		r.MessageBurst = DefaultRateMessageBurst
	}
	if r.Bytes == 0 {
		r.Bytes = DefaultRateBytes
	}
	if r.ByteBurst <= 0 {
		r.ByteBurst = DefaultRateByteBurst
	}
	return r
}

// disabled reports whether this dimension is switched off. A negative rate is
// how a deployment says "no limit", since zero means "use the default".
func disabled(rate float64) bool { return rate < 0 }

// bucket is a token bucket. It is not safe for concurrent use: each connection
// has its own, touched only by its read pump.
type bucket struct {
	rate     float64 // tokens per second
	capacity float64
	tokens   float64
	last     time.Time
}

func newBucket(rate float64, burst int, now time.Time) *bucket {
	return &bucket{
		rate:     rate,
		capacity: float64(burst),
		// Starting full, so a client's opening handshake is never throttled.
		tokens: float64(burst),
		last:   now,
	}
}

// take removes n tokens and reports how long the caller must wait first.
//
// The tokens are taken whether or not the caller has to wait, so the debt is
// paid once: a caller that sleeps for the returned duration and then proceeds
// has already been accounted for.
func (b *bucket) take(n float64, now time.Time) time.Duration {
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Seconds() * b.rate
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.last = now
	}
	b.tokens -= n
	if b.tokens >= 0 {
		return 0
	}
	// A single item larger than the whole bucket would otherwise wait for a
	// deficit it can never repay; the wait is capped at one full refill, which
	// is the honest price of admitting it at all.
	wait := time.Duration(-b.tokens / b.rate * float64(time.Second))
	if maxWait := time.Duration(b.capacity / b.rate * float64(time.Second)); wait > maxWait {
		wait = maxWait
		b.tokens = -b.capacity
	}
	return wait
}

// limiter throttles one connection.
//
// Over the limit it waits rather than closing. A client that bursts is a client
// that is being used, not one that is attacking, and disconnecting it would cost
// a reconnect and a resync to fix a problem that solves itself in a
// millisecond. Waiting also applies backpressure where it belongs: the read pump
// stops reading, the socket buffer fills, and the sender's own TCP stack slows
// it down. Only the offending connection is affected, because the limiter is
// per connection and the room never blocks on it.
type limiter struct {
	messages *bucket
	bytes    *bucket
	now      func() time.Time
	// sleep is injectable so the tests can advance time instead of spending it.
	sleep func(context.Context, time.Duration) error
}

func newLimiter(rate Rate, now func() time.Time) *limiter {
	if now == nil {
		now = time.Now
	}
	rate = rate.withDefaults()
	l := &limiter{now: now, sleep: sleepFor}
	at := now()
	if !disabled(rate.Messages) {
		l.messages = newBucket(rate.Messages, rate.MessageBurst, at)
	}
	if !disabled(rate.Bytes) {
		l.bytes = newBucket(rate.Bytes, rate.ByteBurst, at)
	}
	return l
}

// enabled reports whether anything is being limited at all, so a connection with
// no limits does no work per frame.
func (l *limiter) enabled() bool { return l != nil && (l.messages != nil || l.bytes != nil) }

// wait blocks until this frame may be handled, and reports how long it waited.
func (l *limiter) wait(ctx context.Context, size int) (time.Duration, error) {
	if !l.enabled() {
		return 0, nil
	}
	now := l.now()
	var longest time.Duration
	if l.messages != nil {
		longest = l.messages.take(1, now)
	}
	if l.bytes != nil {
		if d := l.bytes.take(float64(size), now); d > longest {
			longest = d
		}
	}
	if longest <= 0 {
		return 0, nil
	}
	return longest, l.sleep(ctx, longest)
}

func sleepFor(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
