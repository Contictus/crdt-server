package gateway

import (
	"context"
	"testing"
	"time"
)

// The limiter is tested with a clock the test moves and a sleep the test
// records, so a rate limit measured in seconds costs no seconds to check.
func testLimiter(t *testing.T, rate Rate) (*limiter, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: time.Unix(1700000000, 0)}
	l := newLimiter(rate, clock.Now)
	l.sleep = func(_ context.Context, d time.Duration) error {
		clock.slept += d
		clock.now = clock.now.Add(d)
		return nil
	}
	return l, clock
}

type fakeClock struct {
	now   time.Time
	slept time.Duration
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// A client inside the limit is never delayed, which is the case that matters:
// a limit that taxes normal editing would be a worse bug than the flood it
// prevents.
func TestNormalTrafficIsNotThrottled(t *testing.T) {
	l, clock := testLimiter(t, Rate{Messages: 100, MessageBurst: 100, Bytes: -1})

	// Twenty messages a second for a second, which is faster than a person
	// types.
	for range 20 {
		waited, err := l.wait(context.Background(), 200)
		if err != nil {
			t.Fatal(err)
		}
		if waited != 0 {
			t.Fatalf("waited %s inside the limit", waited)
		}
		clock.advance(50 * time.Millisecond)
	}
	if clock.slept != 0 {
		t.Fatalf("slept %s in total", clock.slept)
	}
}

// Over the limit the caller waits rather than being disconnected. A client that
// bursts is a client being used; killing it would cost a reconnect and a resync
// to fix something that resolves itself in a millisecond.
func TestAFloodIsSlowedNotRefused(t *testing.T) {
	l, clock := testLimiter(t, Rate{Messages: 10, MessageBurst: 10, Bytes: -1})

	// The burst goes straight through.
	for i := range 10 {
		if waited, err := l.wait(context.Background(), 1); err != nil || waited != 0 {
			t.Fatalf("message %d waited %s (%v)", i, waited, err)
		}
	}
	// The next one has to wait for a token, and nothing errors: the connection
	// stays open.
	waited, err := l.wait(context.Background(), 1)
	if err != nil {
		t.Fatalf("the flood was refused rather than slowed: %v", err)
	}
	if waited <= 0 {
		t.Fatal("the eleventh message in a burst of ten was not throttled")
	}
	// Ten a second means a tenth of a second per message.
	if waited > 200*time.Millisecond {
		t.Fatalf("waited %s, want about 100ms", waited)
	}
	if clock.slept != waited {
		t.Fatalf("reported %s but slept %s", waited, clock.slept)
	}
}

// Bytes and messages are two different floods: many tiny updates cost CPU per
// message, a few huge ones cost memory and fanout. A limit on one must not be
// satisfied by the other.
func TestTheByteLimitCatchesLargeFrames(t *testing.T) {
	l, _ := testLimiter(t, Rate{Messages: -1, Bytes: 1000, ByteBurst: 1000})

	if waited, err := l.wait(context.Background(), 1000); err != nil || waited != 0 {
		t.Fatalf("the first frame waited %s (%v)", waited, err)
	}
	waited, err := l.wait(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if waited <= 0 {
		t.Fatal("a second full-burst frame was not throttled")
	}
}

// A frame larger than the whole bucket must not wait for a debt it can never
// repay.
func TestAnOversizedFrameWaitsOnce(t *testing.T) {
	l, _ := testLimiter(t, Rate{Messages: -1, Bytes: 1000, ByteBurst: 1000})

	waited, err := l.wait(context.Background(), 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if waited > time.Second {
		t.Fatalf("waited %s for one frame, want at most one refill", waited)
	}
	// And the connection recovers rather than being penalised forever.
	if _, err := l.wait(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
}

// Waiting has to end when the connection does, or a throttled client would
// hold a goroutine after it has gone.
func TestThrottlingEndsWithTheConnection(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1700000000, 0)}
	l := newLimiter(Rate{Messages: 1, MessageBurst: 1, Bytes: -1}, clock.Now)

	ctx, cancel := context.WithCancel(context.Background())
	if _, err := l.wait(ctx, 1); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := l.wait(ctx, 1); err == nil {
		t.Fatal("a cancelled connection kept waiting")
	}
}

// Zero means the default, negative means off - so a deployment can say "no
// limit" without it being the same word as "unset".
func TestLimitsCanBeDisabled(t *testing.T) {
	l := newLimiter(Rate{Messages: -1, Bytes: -1}, nil)
	if l.enabled() {
		t.Fatal("a limiter with both dimensions off is still enabled")
	}
	if waited, err := l.wait(context.Background(), 1<<20); err != nil || waited != 0 {
		t.Fatalf("a disabled limiter waited %s (%v)", waited, err)
	}

	defaults := newLimiter(Rate{}, nil)
	if !defaults.enabled() {
		t.Fatal("the zero value did not apply the defaults")
	}
}
