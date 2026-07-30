package cluster_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/mesutokul/ycollab/internal/cluster"
)

// These run against a real Redis, for the same reason the store tests run
// against a real Postgres: the interesting behaviour is the library's and the
// server's, and a mock would only test the mock.
//
//	docker compose -f deploy/docker-compose.yml up -d
//	YCOLLAB_TEST_REDIS_URL=redis://127.0.0.1:6380 go test ./internal/cluster/
const redisEnv = "YCOLLAB_TEST_REDIS_URL"

func testBus(t *testing.T) *cluster.Redis {
	t.Helper()
	url := os.Getenv(redisEnv)
	if url == "" {
		t.Skipf("%s is not set; start deploy/docker-compose.yml to run these", redisEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Every test gets its own channel prefix, so a run does not see the
	// leftovers of the previous one and parallel runs do not collide.
	bus, err := cluster.OpenRedis(ctx, cluster.RedisConfig{
		URL:    url,
		Prefix: fmt.Sprintf("ycollab-test-%d", time.Now().UnixNano()),
	})
	if err != nil {
		t.Fatalf("open redis: %v", err)
	}
	t.Cleanup(func() {
		if err := bus.Close(); err != nil {
			t.Errorf("close bus: %v", err)
		}
	})
	return bus
}

// collector gathers envelopes off the bus goroutine.
type collector struct {
	mu   sync.Mutex
	got  []cluster.Envelope
	seen chan struct{}
}

func newCollector() *collector {
	return &collector{seen: make(chan struct{}, 64)}
}

func (c *collector) deliver(env cluster.Envelope) {
	c.mu.Lock()
	c.got = append(c.got, env)
	c.mu.Unlock()
	select {
	case c.seen <- struct{}{}:
	default:
	}
}

// wait blocks until at least n envelopes have arrived.
func (c *collector) wait(t *testing.T, n int) []cluster.Envelope {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		c.mu.Lock()
		got := append([]cluster.Envelope(nil), c.got...)
		c.mu.Unlock()
		if len(got) >= n {
			return got
		}
		select {
		case <-c.seen:
		case <-deadline:
			t.Fatalf("timed out with %d of %d envelopes", len(got), n)
		}
	}
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.got)
}

// Two subscribers on one channel, one publisher: the shape a document held by
// three replicas has.
func TestRedisDeliversToEveryReplica(t *testing.T) {
	bus := testBus(t)
	ctx := context.Background()

	a, b := newCollector(), newCollector()
	if _, err := bus.Subscribe(ctx, "doc", a.deliver); err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Subscribe(ctx, "doc", b.deliver); err != nil {
		t.Fatal(err)
	}

	want := cluster.Envelope{Origin: 1234, Kind: cluster.KindUpdate, Payload: []byte{1, 0, 255, 7}}
	if err := bus.Publish(ctx, "doc", want); err != nil {
		t.Fatal(err)
	}

	for name, c := range map[string]*collector{"a": a, "b": b} {
		got := c.wait(t, 1)[0]
		if got.Origin != want.Origin || got.Kind != want.Kind || !bytes.Equal(got.Payload, want.Payload) {
			t.Fatalf("%s received %+v, want %+v", name, got, want)
		}
	}
}

// Binary payloads are the whole point: a Yjs update is arbitrary bytes, and a
// transport that mangles them would corrupt documents rather than fail loudly.
func TestRedisCarriesArbitraryBytes(t *testing.T) {
	bus := testBus(t)
	ctx := context.Background()

	c := newCollector()
	if _, err := bus.Subscribe(ctx, "doc", c.deliver); err != nil {
		t.Fatal(err)
	}

	payload := make([]byte, 256)
	for i := range payload {
		payload[i] = byte(i)
	}
	if err := bus.Publish(ctx, "doc", cluster.Envelope{Origin: 9, Kind: cluster.KindUpdate, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if got := c.wait(t, 1)[0].Payload; !bytes.Equal(got, payload) {
		t.Fatalf("payload came back as %x", got)
	}
}

// A room that evicts unsubscribes, and after that its channel must cost nothing.
func TestRedisStopsDeliveringAfterUnsubscribe(t *testing.T) {
	bus := testBus(t)
	ctx := context.Background()

	gone, staying := newCollector(), newCollector()
	sub, err := bus.Subscribe(ctx, "doc", gone.deliver)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Subscribe(ctx, "doc", staying.deliver); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(ctx, "doc", cluster.Envelope{Origin: 1, Kind: cluster.KindUpdate}); err != nil {
		t.Fatal(err)
	}
	gone.wait(t, 1)
	staying.wait(t, 1)

	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(ctx, "doc", cluster.Envelope{Origin: 1, Kind: cluster.KindUpdate}); err != nil {
		t.Fatal(err)
	}
	// The one still listening is the synchronisation point: once it has the
	// second message, the first would have had it too.
	staying.wait(t, 2)
	if n := gone.count(); n != 1 {
		t.Fatalf("a closed subscription received %d messages, want 1", n)
	}
}

// Documents must not leak into each other, which for one multiplexed connection
// means the demultiplexing has to be right.
func TestRedisKeepsDocumentsApart(t *testing.T) {
	bus := testBus(t)
	ctx := context.Background()

	one, two := newCollector(), newCollector()
	if _, err := bus.Subscribe(ctx, "one", one.deliver); err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Subscribe(ctx, "two", two.deliver); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(ctx, "two", cluster.Envelope{Origin: 5, Kind: cluster.KindAwareness, Payload: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	two.wait(t, 1)
	if n := one.count(); n != 0 {
		t.Fatalf("the other document received %d messages", n)
	}
}

// Two buses is what two replicas are.
func TestRedisConnectsSeparateProcesses(t *testing.T) {
	url := os.Getenv(redisEnv)
	if url == "" {
		t.Skipf("%s is not set", redisEnv)
	}
	ctx := context.Background()
	prefix := fmt.Sprintf("ycollab-test-%d", time.Now().UnixNano())

	open := func() *cluster.Redis {
		bus, err := cluster.OpenRedis(ctx, cluster.RedisConfig{URL: url, Prefix: prefix})
		if err != nil {
			t.Fatalf("open redis: %v", err)
		}
		t.Cleanup(func() { _ = bus.Close() })
		return bus
	}
	left, right := open(), open()

	c := newCollector()
	if _, err := right.Subscribe(ctx, "doc", c.deliver); err != nil {
		t.Fatal(err)
	}
	if err := left.Publish(ctx, "doc", cluster.Envelope{Origin: 77, Kind: cluster.KindStateVector, Payload: []byte{0}}); err != nil {
		t.Fatal(err)
	}
	if got := c.wait(t, 1)[0]; got.Origin != 77 || got.Kind != cluster.KindStateVector {
		t.Fatalf("received %+v", got)
	}
}

func TestRedisRejectsABadURL(t *testing.T) {
	if _, err := cluster.OpenRedis(context.Background(), cluster.RedisConfig{URL: "not-a-url"}); err == nil {
		t.Fatal("a bad url was accepted")
	}
}
