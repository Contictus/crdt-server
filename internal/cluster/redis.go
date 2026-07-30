package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// DefaultPrefix namespaces the channels, so a Redis shared with anything else
	// stays legible.
	DefaultPrefix = "ycollab"
	// publishTimeout bounds one PUBLISH. Redis Pub/Sub does not wait for
	// subscribers, so a publish that takes this long means the server or the
	// network is in trouble, not that a subscriber is slow.
	publishTimeout = 5 * time.Second
	// subscribeTimeout bounds how long a room waits for Redis to confirm its
	// subscription before giving up and refusing to serve the document.
	subscribeTimeout = 10 * time.Second
	// receiveBackoff is how long the receive loop waits after a connection
	// error before trying again. go-redis reconnects for us; this only stops a
	// hard-down Redis from becoming a spin loop.
	receiveBackoff = 500 * time.Millisecond
)

// RedisConfig configures the Redis bus.
type RedisConfig struct {
	// URL is a redis:// or rediss:// connection string.
	URL string
	// Prefix namespaces channel names. Empty means DefaultPrefix.
	Prefix string
	Logger *slog.Logger
}

// Redis is a Bus backed by Redis Pub/Sub.
//
// It holds exactly two connections however many documents are resident: one for
// publishing, taken from the client's pool, and one subscriber that is
// multiplexed across every room. A subscription per room would be simpler to
// write and would cost a Redis connection per document, which is a limit nobody
// wants to discover during an incident.
type Redis struct {
	client *redis.Client
	ps     *redis.PubSub
	prefix string
	log    *slog.Logger

	mu sync.RWMutex
	// handlers maps a channel name to the rooms listening on it. A node normally
	// has one room per name, but the map does not need to assume that.
	handlers map[string][]*subscription
	// pending holds a channel per SUBSCRIBE we are waiting for the server to
	// confirm; see waitSubscribed.
	pending map[string]chan struct{}
	started bool
	closed  bool

	stop chan struct{}
	done chan struct{}
}

// OpenRedis connects to Redis and verifies the connection before returning, so a
// bad URL is a startup error rather than a mystery at the first edit.
func OpenRedis(ctx context.Context, cfg RedisConfig) (*Redis, error) {
	opts, err := redis.ParseURL(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("cluster: bad redis url: %w", err)
	}
	client := redis.NewClient(opts)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("cluster: redis unreachable: %w", err)
	}
	prefix := cfg.Prefix
	if prefix == "" {
		prefix = DefaultPrefix
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Redis{
		client:   client,
		ps:       client.Subscribe(ctx),
		prefix:   prefix,
		log:      log,
		handlers: make(map[string][]*subscription),
		pending:  make(map[string]chan struct{}),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}, nil
}

func (r *Redis) channel(room string) string { return r.prefix + ":room:" + room }

// Publish implements Bus.
func (r *Redis) Publish(ctx context.Context, room string, env Envelope) error {
	ctx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()
	return r.client.Publish(ctx, r.channel(room), env.Encode()).Err()
}

type subscription struct {
	bus     *Redis
	channel string
	deliver func(Envelope)

	once sync.Once
}

// Subscribe implements Bus.
func (r *Redis) Subscribe(ctx context.Context, room string, deliver func(Envelope)) (Subscription, error) {
	channel := r.channel(room)
	sub := &subscription{bus: r, channel: channel, deliver: deliver}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("cluster: bus is closed")
	}
	first := len(r.handlers[channel]) == 0
	r.handlers[channel] = append(r.handlers[channel], sub)
	// The receive loop starts with the first subscription rather than at Open: a
	// PubSub with no channels has no connection to read from.
	if !r.started {
		r.started = true
		go r.receive()
	}
	r.mu.Unlock()

	if first {
		if err := r.subscribeAndWait(ctx, channel); err != nil {
			_ = sub.Close()
			return nil, err
		}
	}
	return sub, nil
}

// subscribeAndWait sends SUBSCRIBE and waits for the server to confirm it.
//
// The wait is the point. go-redis's Subscribe returns as soon as the command has
// been written, so a Publish issued immediately afterwards can reach Redis
// before the subscription exists and be delivered to nobody. For a room that is
// exactly the case that matters: the room subscribes and then serves a client
// whose first edit must not vanish. The confirmation arrives on the receive loop
// as a *redis.Subscription, which is why the loop uses Receive rather than
// ReceiveMessage - the latter swallows them.
func (r *Redis) subscribeAndWait(ctx context.Context, channel string) error {
	ready := make(chan struct{})
	r.mu.Lock()
	r.pending[channel] = ready
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.pending, channel)
		r.mu.Unlock()
	}()

	if err := r.ps.Subscribe(ctx, channel); err != nil {
		return fmt.Errorf("cluster: subscribe %s: %w", channel, err)
	}
	ctx, cancel := context.WithTimeout(ctx, subscribeTimeout)
	defer cancel()
	select {
	case <-ready:
		return nil
	case <-r.stop:
		return errors.New("cluster: bus is closed")
	case <-ctx.Done():
		return fmt.Errorf("cluster: subscribe %s: %w", channel, ctx.Err())
	}
}

// confirmed releases whoever is waiting for this channel's subscription.
func (r *Redis) confirmed(channel string) {
	r.mu.Lock()
	ready, ok := r.pending[channel]
	if ok {
		delete(r.pending, channel)
	}
	r.mu.Unlock()
	if ok {
		close(ready)
	}
}

// Close removes this listener, and unsubscribes the channel once nobody is left.
func (s *subscription) Close() error {
	var err error
	s.once.Do(func() {
		r := s.bus
		r.mu.Lock()
		listeners := r.handlers[s.channel]
		for i, other := range listeners {
			if other == s {
				r.handlers[s.channel] = append(listeners[:i:i], listeners[i+1:]...)
				break
			}
		}
		empty := len(r.handlers[s.channel]) == 0
		if empty {
			delete(r.handlers, s.channel)
		}
		closed := r.closed
		r.mu.Unlock()

		if empty && !closed {
			ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
			defer cancel()
			err = r.ps.Unsubscribe(ctx, s.channel)
		}
	})
	return err
}

// receive is the one goroutine reading the subscriber connection. It fans each
// message out to the rooms listening on that channel.
//
// A message for a channel nobody listens to is normal: UNSUBSCRIBE and an
// in-flight message race, and losing that race is exactly the case the room's
// own lookup handles by ignoring it.
func (r *Redis) receive() {
	defer close(r.done)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-r.stop
		cancel()
	}()

	for {
		received, err := r.ps.Receive(ctx)
		if err != nil {
			select {
			case <-r.stop:
				return
			default:
			}
			// go-redis reconnects and resubscribes on its own, so this is
			// reported and retried rather than treated as fatal. While it lasts,
			// replicas fall back to being independent servers that each still
			// serve their own clients correctly; anti-entropy closes the gap when
			// the connection comes back.
			r.log.Warn("cluster: receive failed", "err", err)
			select {
			case <-r.stop:
				return
			case <-time.After(receiveBackoff):
			}
			continue
		}
		switch msg := received.(type) {
		case *redis.Subscription:
			// Also the reconnect path: go-redis resubscribes on its own and the
			// confirmations come back through here, where there is nobody left
			// waiting for them.
			if msg.Kind == "subscribe" {
				r.confirmed(msg.Channel)
			}
		case *redis.Message:
			r.dispatch(msg)
		}
	}
}

func (r *Redis) dispatch(msg *redis.Message) {
	env, err := Decode([]byte(msg.Payload))
	if err != nil {
		r.log.Warn("cluster: undecodable envelope", "channel", msg.Channel, "err", err)
		return
	}
	r.mu.RLock()
	listeners := r.handlers[msg.Channel]
	r.mu.RUnlock()
	for _, sub := range listeners {
		sub.deliver(env)
	}
}

// Close shuts the bus down. Rooms should have closed their subscriptions first;
// this does not wait for them.
func (r *Redis) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	started := r.started
	r.mu.Unlock()

	close(r.stop)
	err := r.ps.Close()
	if started {
		<-r.done
	}
	if cerr := r.client.Close(); err == nil {
		err = cerr
	}
	return err
}
