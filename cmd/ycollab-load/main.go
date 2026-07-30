// Command ycollab-load drives a lot of connections at a server and reports what
// came back.
//
//	ycollab-load -url ws://127.0.0.1:8080 -clients 500 -rooms 50 -duration 60s
//
// It speaks the wire protocol directly rather than running real Yjs clients.
// That is the whole point: a real client costs a document, a provider and a
// couple of megabytes, so a few hundred of them make the load generator the
// bottleneck and measure nothing about the server. Wire correctness is not what
// this measures either - the fixtures and the Node soak cover that - so the bot
// is free to be as cheap as it can be.
//
// What it reports is propagation latency: the time from one client sending an
// update to another client in the same room receiving it. That is the number a
// person editing a document actually feels.
package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/mesutokul/ycollab/internal/crdt"
	"github.com/mesutokul/ycollab/internal/protocol"
)

type options struct {
	url      string
	clients  int
	rooms    int
	duration time.Duration
	rate     float64
	chars    int
	token    string
	root     string
	timeout  time.Duration
	// connectAtOnce bounds how many dials are in flight, so the bot does not
	// overrun the listen backlog on its way in.
	connectAtOnce int
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	opts := options{}
	flag.StringVar(&opts.url, "url", "ws://127.0.0.1:8080", "server to connect to")
	flag.IntVar(&opts.clients, "clients", 100, "concurrent connections")
	flag.IntVar(&opts.rooms, "rooms", 10, "documents to spread them over")
	flag.DurationVar(&opts.duration, "duration", 30*time.Second, "how long to keep editing")
	flag.Float64Var(&opts.rate, "rate", 2, "updates per second per client")
	flag.IntVar(&opts.chars, "chars", 8, "characters per update")
	flag.StringVar(&opts.token, "token", "", "token, when the server requires one; the same one is used for every room, so it only works with an open server unless -rooms 1")
	flag.StringVar(&opts.root, "root", "content", "name of the shared text type to write into")
	flag.DurationVar(&opts.timeout, "timeout", 30*time.Second, "how long to wait for the initial sync")
	flag.IntVar(&opts.connectAtOnce, "connect-at-once", 64, "how many connections to open in parallel")
	flag.Parse()

	if opts.connectAtOnce < 1 {
		return errors.New("-connect-at-once must be at least 1")
	}

	if opts.clients < 2 {
		return errors.New("-clients must be at least 2: latency is measured between clients")
	}
	if opts.rooms < 1 || opts.rooms > opts.clients/2 {
		return fmt.Errorf("-rooms must be between 1 and %d, so every room has at least two clients", opts.clients/2)
	}
	if err := selfTest(opts.root); err != nil {
		return fmt.Errorf("the update builder is wrong: %w", err)
	}
	// Measured before the run, while the machine is quiet.
	resolution := clockResolution()

	tracker := newTracker()
	clients := make([]*client, 0, opts.clients)
	for i := range opts.clients {
		clients = append(clients, &client{
			id:      uint64(i + 1),
			room:    fmt.Sprintf("load-%d", i%opts.rooms),
			opts:    opts,
			tracker: tracker,
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fmt.Printf("connecting %d clients over %d rooms at %s\n", opts.clients, opts.rooms, opts.url)
	connectCtx, connectCancel := context.WithTimeout(ctx, opts.timeout)
	var connecting sync.WaitGroup
	var failed atomic.Int64
	// Dialling every client at once overruns the listen backlog and the kernel
	// answers with a refusal - which measures the accept queue, not the server,
	// and is not what any real fleet of clients does. So connections are opened
	// in waves.
	gate := make(chan struct{}, opts.connectAtOnce)
	for _, c := range clients {
		connecting.Add(1)
		go func() {
			defer connecting.Done()
			gate <- struct{}{}
			defer func() { <-gate }()
			if err := c.connect(connectCtx); err != nil {
				failed.Add(1)
				c.fail(err)
			}
		}()
	}
	connecting.Wait()
	connectCancel()
	if n := failed.Load(); n > 0 {
		return fmt.Errorf("%d of %d clients could not connect: %v", n, opts.clients, firstErr(clients))
	}
	fmt.Printf("connected, editing for %s\n", opts.duration)

	var running sync.WaitGroup
	for _, c := range clients {
		running.Add(1)
		go func() {
			defer running.Done()
			c.read(ctx)
		}()
	}

	editCtx, stopEditing := context.WithTimeout(ctx, opts.duration)
	var editing sync.WaitGroup
	started := time.Now()
	for _, c := range clients {
		editing.Add(1)
		go func() {
			defer editing.Done()
			c.edit(editCtx)
		}()
	}
	editing.Wait()
	stopEditing()
	elapsed := time.Since(started)

	// The last updates are still in flight. Wait for them rather than counting
	// them as lost, but not forever: anything still missing after this is the
	// interesting number.
	fmt.Println("editing stopped, waiting for the last updates to land")
	settle(clients, 5*time.Second)

	cancel()
	for _, c := range clients {
		c.close()
	}
	running.Wait()

	report(clients, tracker, elapsed, opts, resolution)
	return nil
}

// tracker remembers when each update was sent, so whoever receives it can say
// how long it took. Every client is in this process, which is what makes the
// measurement possible without clock synchronisation.
type tracker struct {
	mu   sync.Mutex
	sent map[[16]byte]time.Time
}

func newTracker() *tracker { return &tracker{sent: make(map[[16]byte]time.Time)} }

func key(update []byte) [16]byte {
	sum := sha256.Sum256(update)
	return [16]byte(sum[:16])
}

func (t *tracker) record(update []byte, at time.Time) {
	k := key(update)
	t.mu.Lock()
	t.sent[k] = at
	t.mu.Unlock()
}

// latency reports how long an update took to arrive, and whether it was one of
// ours at all: a server may legitimately send us a SyncStep2 built from
// somebody else's structs, which is not a message we timed.
func (t *tracker) latency(update []byte, at time.Time) (time.Duration, bool) {
	k := key(update)
	t.mu.Lock()
	sentAt, ok := t.sent[k]
	t.mu.Unlock()
	if !ok {
		return 0, false
	}
	return at.Sub(sentAt), true
}

type client struct {
	id      uint64
	room    string
	opts    options
	tracker *tracker

	ws    *websocket.Conn
	clock uint64

	mu        sync.Mutex
	latencies []time.Duration
	sent      int64
	received  int64
	untracked int64
	errs      []error
	closed    bool
}

func (c *client) connect(ctx context.Context) error {
	url := c.opts.url + "/" + c.room
	if c.opts.token != "" {
		url += "?token=" + c.opts.token
	}
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return err
	}
	// A document under load is legitimately large, and a SyncStep2 carries all
	// of it.
	ws.SetReadLimit(64 << 20)
	c.ws = ws

	sv, err := crdt.NewDoc(crdt.ClientID(c.id)).EncodeStateVector()
	if err != nil {
		return err
	}
	if err := c.write(ctx, protocol.WriteSyncStep1(sv)); err != nil {
		return err
	}
	// The server answers SyncStep2 then its own SyncStep1 (sync.js:23-28). We do
	// not need the contents; waiting for the first frame is what proves the room
	// is serving us.
	if _, _, err := ws.Read(ctx); err != nil {
		return err
	}
	return nil
}

func (c *client) write(ctx context.Context, frame []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageBinary, frame)
}

// edit sends updates at the configured rate until ctx ends.
func (c *client) edit(ctx context.Context) {
	interval := time.Duration(float64(time.Second) / c.opts.rate)
	// Stagger the start, so a thousand clients do not all write on the same
	// millisecond and measure the scheduler instead of the server.
	select {
	case <-time.After(time.Duration(rand.Int64N(int64(interval)))):
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	text := make([]byte, c.opts.chars)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for i := range text {
				text[i] = byte('a' + rand.IntN(26))
			}
			update := buildUpdate(c.id, c.clock, c.opts.root, string(text))
			c.clock += uint64(c.opts.chars)

			c.tracker.record(update, time.Now())
			if err := c.write(ctx, protocol.WriteUpdate(update)); err != nil {
				if ctx.Err() == nil {
					c.fail(err)
				}
				return
			}
			c.mu.Lock()
			c.sent++
			c.mu.Unlock()
		}
	}
}

// read drains everything the server sends and times the updates it recognises.
func (c *client) read(ctx context.Context) {
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			if ctx.Err() == nil && !c.isClosed() {
				c.fail(err)
			}
			return
		}
		now := time.Now()
		msg, err := protocol.Decode(data)
		if err != nil {
			c.fail(err)
			return
		}
		update, ok := msg.(protocol.UpdateMessage)
		if !ok {
			continue
		}
		latency, tracked := c.tracker.latency(update.Update, now)
		c.mu.Lock()
		c.received++
		if tracked {
			c.latencies = append(c.latencies, latency)
		} else {
			c.untracked++
		}
		c.mu.Unlock()
	}
}

func (c *client) fail(err error) {
	c.mu.Lock()
	c.errs = append(c.errs, err)
	c.mu.Unlock()
}

// firstErr returns the first error any client recorded, which is the one worth
// printing: with five hundred connections failing for one reason, the first is
// as informative as the last and arrives sooner.
func firstErr(clients []*client) error {
	for _, c := range clients {
		if err := c.lastErr(); err != nil {
			return err
		}
	}
	return nil
}

func (c *client) lastErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.errs) == 0 {
		return nil
	}
	return c.errs[len(c.errs)-1]
}

func (c *client) close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	if c.ws != nil {
		_ = c.ws.Close(websocket.StatusNormalClosure, "")
	}
}

func (c *client) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *client) counts() (sent, received, untracked int64, errs int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sent, c.received, c.untracked, len(c.errs)
}

// settle waits until the received count stops moving, or the deadline passes.
func settle(clients []*client, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	previous := int64(-1)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		var total int64
		for _, c := range clients {
			_, received, _, _ := c.counts()
			total += received
		}
		if total == previous {
			return
		}
		previous = total
	}
}

func report(clients []*client, _ *tracker, elapsed time.Duration, opts options, resolution time.Duration) {
	var sent, received, untracked int64
	var errs int
	var latencies []time.Duration
	perRoom := make(map[string]int)
	for _, c := range clients {
		s, r, u, e := c.counts()
		sent += s
		received += r
		untracked += u
		errs += e
		perRoom[c.room]++
		c.mu.Lock()
		latencies = append(latencies, c.latencies...)
		c.mu.Unlock()
	}

	// Every update should reach everybody else in its room. Anything short of
	// that is a message the server dropped, which under load is what
	// backpressure looks like.
	var expected int64
	for _, c := range clients {
		s, _, _, _ := c.counts()
		expected += s * int64(perRoom[c.room]-1)
	}

	fmt.Println()
	fmt.Printf("clients            %d over %d rooms\n", len(clients), opts.rooms)
	fmt.Printf("duration           %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("updates sent       %d (%.0f/s)\n", sent, float64(sent)/elapsed.Seconds())
	fmt.Printf("updates delivered  %d (%.0f/s), expected %d\n",
		received, float64(received)/elapsed.Seconds(), expected)
	if expected > 0 {
		fmt.Printf("delivered ratio    %.4f\n", float64(received)/float64(expected))
	}
	if untracked > 0 {
		fmt.Printf("untimed frames     %d (updates we did not author, e.g. a resync)\n", untracked)
	}
	fmt.Printf("errors             %d\n", errs)

	if len(latencies) == 0 {
		fmt.Println("no latency samples")
		return
	}
	slices.Sort(latencies)
	p50 := percentile(latencies, 0.50)
	p95 := percentile(latencies, 0.95)
	p99 := percentile(latencies, 0.99)
	fmt.Printf("propagation p50    %s\n", p50.Round(time.Microsecond))
	fmt.Printf("propagation p95    %s\n", p95.Round(time.Microsecond))
	fmt.Printf("propagation p99    %s\n", p99.Round(time.Microsecond))
	fmt.Printf("propagation max    %s\n", latencies[len(latencies)-1].Round(time.Microsecond))

	// Saying what the clock can see is the difference between a number and a
	// claim. On Windows the monotonic clock steps in fractions of a millisecond,
	// so a latency below one step is measured as zero - which is worth printing,
	// because "p50 0s" otherwise reads as a bug or as a boast.
	fmt.Printf("clock resolution   %s\n", resolution.Round(time.Nanosecond))
	if p99 < resolution {
		fmt.Printf("                   p99 is below one clock step: this machine cannot measure it more finely\n")
	}
}

// clockResolution measures the smallest step this machine's monotonic clock
// takes.
func clockResolution() time.Duration {
	smallest := time.Second
	for range 200000 {
		a := time.Now()
		b := time.Now()
		if d := b.Sub(a); d > 0 && d < smallest {
			smallest = d
		}
	}
	return smallest
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}
