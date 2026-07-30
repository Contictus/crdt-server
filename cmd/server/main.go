// Command server runs a single ycollab node: a WebSocket endpoint that any
// unmodified y-websocket client can point at.
//
//	go run ./cmd/server -addr :8080 -origins localhost:5173
//
// The document name is the URL path, so ws://host:8080/my-doc is document
// "my-doc".
//
// Both of the things that make it more than one process are optional flags, and
// the server says so at startup when they are missing: -database-url makes
// documents outlive the process, and -redis-url makes replicas share them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promcollectors "github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/mesutokul/ycollab/internal/auth"
	"github.com/mesutokul/ycollab/internal/cluster"
	"github.com/mesutokul/ycollab/internal/gateway"
	"github.com/mesutokul/ycollab/internal/metrics"
	"github.com/mesutokul/ycollab/internal/room"
	"github.com/mesutokul/ycollab/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr         = flag.String("addr", envOr("YCOLLAB_ADDR", ":8080"), "listen address")
		origins      = flag.String("origins", os.Getenv("YCOLLAB_ORIGINS"), "comma-separated allowed Origin patterns; empty means same-origin only")
		idleTimeout  = flag.Duration("idle-timeout", room.DefaultIdleTimeout, "how long an empty room stays resident")
		awarenessTTL = flag.Duration("awareness-ttl", 30*time.Second, "drop a client's cursor after this much silence")
		maxRooms     = flag.Int("max-rooms", 0, "cap on resident rooms; 0 means unlimited")
		shutdown     = flag.Duration("shutdown-timeout", 10*time.Second, "how long to wait for connections to drain")
		logLevel     = flag.String("log-level", envOr("YCOLLAB_LOG_LEVEL", "info"), "debug, info, warn or error")

		databaseURL   = flag.String("database-url", os.Getenv("YCOLLAB_DATABASE_URL"), "PostgreSQL connection string; empty keeps documents in memory only")
		compactAfter  = flag.Int("compact-after", room.DefaultCompactAfter, "fold the update log into a snapshot after this many updates")
		flushInterval = flag.Duration("flush-interval", room.DefaultFlushInterval, "how long an update may sit in memory before it is written")

		redisURL    = flag.String("redis-url", os.Getenv("YCOLLAB_REDIS_URL"), "Redis connection string for cross-replica fanout; empty makes this a single node")
		redisPrefix = flag.String("redis-prefix", envOr("YCOLLAB_REDIS_PREFIX", cluster.DefaultPrefix), "namespace for the Redis channel names")
		antiEntropy = flag.Duration("anti-entropy", room.DefaultAntiEntropy, "how often a room announces its state vector to the other replicas")
		tick        = flag.Duration("tick", room.DefaultTick, "how often a room checks awareness timeouts, idleness and whether it owes an announcement")

		jwtSecret        = flag.String("jwt-secret", os.Getenv("YCOLLAB_JWT_SECRET"), "HS256 signing secret; empty leaves the server open to anyone who can reach it. Several may be given, comma-separated, while a key is being rotated")
		jwtLeeway        = flag.Duration("jwt-leeway", auth.DefaultLeeway, "clock skew allowed between whatever mints tokens and this server")
		jwtMaxLifetime   = flag.Duration("jwt-max-lifetime", 0, "reject tokens valid for longer than this; 0 accepts any expiry")
		jwtRequireExpiry = flag.Bool("jwt-require-expiry", true, "reject tokens that never expire")

		adminAddr = flag.String("admin-addr", envOr("YCOLLAB_ADMIN_ADDR", "127.0.0.1:6060"), "address for /metrics, /statsz and /debug/pprof; empty disables them")
		pprof     = flag.Bool("pprof", true, "serve /debug/pprof on the admin address")
	)
	flag.Parse()

	level, err := parseLevel(*logLevel)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	// One registry per process, built here rather than taken from prometheus's
	// default, so what the endpoint exposes is exactly what this server put in
	// it plus the Go and process collectors we ask for.
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		promcollectors.NewGoCollector(),
		promcollectors.NewProcessCollector(promcollectors.ProcessCollectorOpts{}),
	)
	collectors := metrics.New(registry)

	// Rooms stop when this context is cancelled; the HTTP server is drained
	// first so that connections get a close frame rather than a dead socket.
	roomCtx, stopRooms := context.WithCancel(context.Background())
	defer stopRooms()

	var persistence room.Persistence
	if *databaseURL != "" {
		db, err := store.Open(context.Background(), *databaseURL)
		if err != nil {
			return err
		}
		defer db.Close()
		if err := db.Migrate(context.Background()); err != nil {
			return err
		}
		persistence = db
		log.Info("persisting documents", "compact_after", *compactAfter)
	} else {
		// Worth saying out loud: without a database this process is the only
		// copy of every document it is serving.
		log.Warn("no -database-url: documents live only as long as their room")
	}

	var bus cluster.Bus
	if *redisURL != "" {
		redis, err := cluster.OpenRedis(context.Background(), cluster.RedisConfig{
			URL:    *redisURL,
			Prefix: *redisPrefix,
			Logger: log,
		})
		if err != nil {
			return err
		}
		defer redis.Close()
		bus = redis
	} else {
		// Also worth saying out loud: without a bus, two replicas behind the same
		// load balancer serve the same document name as two unrelated documents.
		log.Warn("no -redis-url: this node does not share documents with any other")
	}

	manager := room.NewManager(roomCtx, room.ManagerConfig{
		MaxRooms: *maxRooms,
		Room: room.Config{
			IdleTimeout:   *idleTimeout,
			AwarenessTTL:  *awarenessTTL,
			Tick:          *tick,
			Store:         persistence,
			CompactAfter:  *compactAfter,
			FlushInterval: *flushInterval,
			Bus:           bus,
			AntiEntropy:   *antiEntropy,
			Metrics:       collectors,
			Logger:        log,
		},
	})
	if bus != nil {
		log.Info("joined the cluster", "node_id", manager.NodeID(), "anti_entropy", *antiEntropy)
	}

	authorize, err := authorizer(*jwtSecret, auth.Config{
		Leeway:        *jwtLeeway,
		MaxLifetime:   *jwtMaxLifetime,
		RequireExpiry: *jwtRequireExpiry,
	}, log)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok")
	})
	mux.Handle("/", gateway.New(gateway.Config{
		Rooms:     manager,
		Origins:   splitList(*origins),
		Authorize: authorize,
		Metrics:   collectors,
		Logger:    log,
	}))

	adminSrv, err := startAdmin(*adminAddr, *pprof, registry, manager, log)
	if err != nil {
		return err
	}
	if adminSrv != nil {
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = adminSrv.Shutdown(ctx)
		}()
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: a WebSocket connection is meant to be long lived,
		// and http.Server's write deadline would kill it mid-session.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", *addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
		close(errc)
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), *shutdown)
	defer cancel()
	// Stop accepting first, then tell the rooms to close their connections with
	// 1001. Clients reconnect and resync from their state vector, so a restart
	// costs a diff and nothing else.
	shutdownErr := srv.Shutdown(drainCtx)
	stopRooms()
	manager.Wait()
	if shutdownErr != nil && !errors.Is(shutdownErr, context.DeadlineExceeded) {
		return shutdownErr
	}
	return nil
}

// authorizer builds the gateway's authorisation hook from the configured
// secrets, or returns nil when there are none.
//
// Nil means open, and the server says so loudly at startup. That is the right
// default for a server you are running on your laptop to try the demo, and the
// wrong one everywhere else, which is why it is a warning rather than silence.
func authorizer(secrets string, cfg auth.Config, log *slog.Logger) (func(*http.Request, string) (gateway.Grant, error), error) {
	if secrets == "" {
		log.Warn("no -jwt-secret: every client may read and write every document")
		return nil, nil
	}
	for _, s := range splitList(secrets) {
		cfg.Secrets = append(cfg.Secrets, []byte(s))
	}
	verifier, err := auth.NewVerifier(cfg)
	if err != nil {
		return nil, err
	}
	log.Info("requiring signed tokens", "keys", len(cfg.Secrets), "require_expiry", cfg.RequireExpiry)

	return func(r *http.Request, doc string) (gateway.Grant, error) {
		grant, err := verifier.Verify(auth.TokenFromRequest(r), doc)
		if err != nil {
			// The error text goes to the client. auth's errors are written to be
			// said out loud: they name what is wrong with the token and nothing
			// about the key or the check.
			return gateway.Grant{}, err
		}
		return gateway.Grant{Subject: grant.Subject, Write: grant.Write}, nil
	}, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseLevel(s string) (slog.Level, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return 0, fmt.Errorf("bad -log-level %q", s)
	}
	return l, nil
}
