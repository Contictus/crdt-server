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
	"github.com/mesutokul/ycollab/internal/blob"
	"github.com/mesutokul/ycollab/internal/cluster"
	"github.com/mesutokul/ycollab/internal/gateway"
	"github.com/mesutokul/ycollab/internal/hook"
	"github.com/mesutokul/ycollab/internal/metrics"
	"github.com/mesutokul/ycollab/internal/protocol"
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
		maxMemory    = flag.String("max-memory", os.Getenv("YCOLLAB_MAX_MEMORY"), "cap on what the documents in memory may cost, as bytes or with a suffix (512MB, 4GiB); empty means unlimited. This is the bound to set against a container's memory limit - -max-rooms counts documents, which says nothing about their size")
		maxConns     = flag.Int("max-conns", 0, "cap on concurrent connections to this node; 0 means unlimited")
		shutdown     = flag.Duration("shutdown-timeout", 10*time.Second, "how long to wait for connections to drain")
		logLevel     = flag.String("log-level", envOr("YCOLLAB_LOG_LEVEL", "info"), "debug, info, warn or error")

		databaseURL   = flag.String("database-url", os.Getenv("YCOLLAB_DATABASE_URL"), "PostgreSQL connection string; empty keeps documents in memory only")
		compactAfter  = flag.Int("compact-after", room.DefaultCompactAfter, "fold the update log into a snapshot after this many updates")
		flushInterval = flag.Duration("flush-interval", room.DefaultFlushInterval, "how long an update may sit in memory before it is written")
		durableWrites = flag.Bool("durable-writes", false, "write every update to the database before relaying it; removes the flush window at the cost of a round trip per update. A write that fails is logged and counted and the update is relayed anyway, so this bounds what a crash loses, not what an unreachable database loses")
		retention     = flag.Duration("retention", 0, "delete documents nothing has touched for this long; 0 keeps them forever")

		s3Bucket    = flag.String("s3-bucket", os.Getenv("YCOLLAB_S3_BUCKET"), "keep snapshots and version payloads in this S3 bucket instead of in the database; empty keeps them in PostgreSQL")
		s3Region    = flag.String("s3-region", envOr("YCOLLAB_S3_REGION", "us-east-1"), "region for the S3 signature; services that have no regions accept the default")
		s3Endpoint  = flag.String("s3-endpoint", os.Getenv("YCOLLAB_S3_ENDPOINT"), "address of an S3-compatible service - MinIO, R2, Backblaze, Ceph. Empty means AWS")
		s3Prefix    = flag.String("s3-prefix", os.Getenv("YCOLLAB_S3_PREFIX"), "namespace every object key with this, so one bucket can hold more than one deployment")
		s3AccessKey = flag.String("s3-access-key", os.Getenv("YCOLLAB_S3_ACCESS_KEY"), "access key id; empty falls back to AWS_ACCESS_KEY_ID")
		s3SecretKey = flag.String("s3-secret-key", os.Getenv("YCOLLAB_S3_SECRET_KEY"), "secret access key; empty falls back to AWS_SECRET_ACCESS_KEY")
		s3PathStyle = flag.Bool("s3-path-style", false, "address objects as endpoint/bucket/key on AWS too; forced on for a custom endpoint")
		s3Timeout   = flag.Duration("s3-timeout", blob.DefaultTimeout, "how long one object storage request may take")

		versionInterval = flag.Duration("version-interval", 0, "keep a version of each changed document this often; 0 keeps only the versions asked for through the API")
		versionKeep     = flag.Int("version-keep", room.DefaultVersionKeep, "versions to keep per document; negative keeps every one, which is unbounded storage")

		redisURL    = flag.String("redis-url", os.Getenv("YCOLLAB_REDIS_URL"), "Redis connection string for cross-replica fanout; empty makes this a single node")
		redisPrefix = flag.String("redis-prefix", envOr("YCOLLAB_REDIS_PREFIX", cluster.DefaultPrefix), "namespace for the Redis channel names")
		antiEntropy = flag.Duration("anti-entropy", room.DefaultAntiEntropy, "how often a room announces its state vector to the other replicas")
		tick        = flag.Duration("tick", room.DefaultTick, "how often a room checks awareness timeouts, idleness and whether it owes an announcement")

		jwtSecret        = flag.String("jwt-secret", os.Getenv("YCOLLAB_JWT_SECRET"), "HS256 signing secret; empty leaves the server open to anyone who can reach it. Several may be given, comma-separated, while a key is being rotated")
		jwtLeeway        = flag.Duration("jwt-leeway", auth.DefaultLeeway, "clock skew allowed between whatever mints tokens and this server")
		jwtMaxLifetime   = flag.Duration("jwt-max-lifetime", 0, "reject tokens valid for longer than this; 0 accepts any expiry")
		jwtRequireExpiry = flag.Bool("jwt-require-expiry", true, "reject tokens that never expire")

		authURL       = flag.String("auth-url", os.Getenv("YCOLLAB_AUTH_URL"), "ask this URL whether each connection may open its document, instead of verifying a token here. Cannot be combined with -jwt-secret")
		authSecret    = flag.String("auth-secret", os.Getenv("YCOLLAB_AUTH_SECRET"), "HMAC-SHA256 key the authorization request body is signed with; empty leaves the requests unsigned")
		authTimeout   = flag.Duration("auth-timeout", auth.DefaultCallbackTimeout, "how long one authorization request may take; a client is waiting on it")
		authCacheTTL  = flag.Duration("auth-cache-ttl", 0, "reuse an authorization decision for the same token and document for this long; 0 asks every time, so a revocation takes effect at once")
		authCacheSize = flag.Int("auth-cache-size", auth.DefaultCacheSize, "how many authorization decisions may be remembered at once")
		authFailOpen  = flag.Bool("auth-fail-open", false, "allow every connection while the authorization endpoint is unreachable, instead of refusing them")

		awarenessMaxState   = flag.Int("awareness-max-state", protocol.DefaultMaxState, "largest cursor state a client may publish, in bytes; negative means no limit")
		awarenessMaxClients = flag.Int("awareness-max-clients", protocol.DefaultMaxClients, "how many clients one document will track cursors for; negative means no limit")

		rateMessages     = flag.Float64("rate-messages", gateway.DefaultRateMessages, "messages per second one connection may send; negative means no limit")
		rateMessageBurst = flag.Int("rate-message-burst", gateway.DefaultRateMessageBurst, "how many messages may arrive at once")
		rateBytes        = flag.Float64("rate-bytes", gateway.DefaultRateBytes, "bytes per second one connection may send; negative means no limit")
		rateByteBurst    = flag.Int("rate-byte-burst", gateway.DefaultRateByteBurst, "how many bytes may arrive at once")

		webhookURL     = flag.String("webhook-url", os.Getenv("YCOLLAB_WEBHOOK_URL"), "POST document events to this URL; empty emits nothing")
		webhookSecret  = flag.String("webhook-secret", os.Getenv("YCOLLAB_WEBHOOK_SECRET"), "HMAC-SHA256 key the webhook body is signed with; empty leaves the requests unsigned")
		webhookEvents  = flag.String("webhook-events", "", "comma-separated events to send: "+strings.Join(hookNames(), ", ")+". Empty sends all of them")
		webhookState   = flag.Bool("webhook-state", false, "include the whole document, as a Yjs update, in every event")
		webhookTimeout = flag.Duration("webhook-timeout", hook.DefaultTimeout, "how long one webhook request may take")
		webhookRetries = flag.Int("webhook-retries", hook.DefaultRetries, "how many times a failed delivery is retried")
		webhookQueue   = flag.Int("webhook-queue", hook.DefaultQueue, "events that may be waiting for delivery before they start being dropped")

		maxConnsPerIP  = flag.Int("max-conns-per-ip", 0, "cap on concurrent connections from one client address; 0 means unlimited")
		trustedProxies = flag.String("trusted-proxies", os.Getenv("YCOLLAB_TRUSTED_PROXIES"), `comma-separated CIDR blocks whose X-Forwarded-For header is believed, or "loopback" or "private". Empty uses the socket's peer address and ignores the header, which is the only safe default when clients reach this server directly`)

		adminToken = flag.String("admin-token", os.Getenv("YCOLLAB_ADMIN_TOKEN"), "require this token as an Authorization: Bearer header on the admin endpoints; empty leaves them open to anyone who can reach the address. Several may be given, comma-separated, while a token is being rotated")

		auditLog = flag.String("audit-log", envOr("YCOLLAB_AUDIT_LOG", "-"), `write an audit record per admin request, as one JSON object per line: "-" is stdout, which keeps it clear of the process log on stderr; a path writes to that file; empty keeps no trail at all`)

		adminAddr = flag.String("admin-addr", envOr("YCOLLAB_ADMIN_ADDR", "127.0.0.1:6060"), "address for /metrics, /statsz and /debug/pprof; empty disables them")
		pprof     = flag.Bool("pprof", true, "serve /debug/pprof on the admin address")

		showVersion = flag.Bool("version", false, "print the build and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(versionLine())
		return nil
	}

	level, err := parseLevel(*logLevel)
	if err != nil {
		return err
	}
	memoryBudget, err := parseBytes(*maxMemory)
	if err != nil {
		return fmt.Errorf("-max-memory: %w", err)
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
	var versioning room.Versioning
	var documents *store.Store
	if *databaseURL != "" {
		db, err := store.Open(context.Background(), *databaseURL)
		if err != nil {
			return err
		}
		defer db.Close()
		db.SetMetrics(collectors)

		if *s3Bucket != "" {
			objects, err := blob.New(blob.Config{
				Bucket:    *s3Bucket,
				Region:    *s3Region,
				Endpoint:  *s3Endpoint,
				Prefix:    *s3Prefix,
				PathStyle: *s3PathStyle,
				Timeout:   *s3Timeout,
				Credentials: blob.Credentials{
					AccessKeyID:     *s3AccessKey,
					SecretAccessKey: *s3SecretKey,
					SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
				},
			})
			if err != nil {
				return err
			}
			db.UseBlobs(objects, log)
			log.Info("keeping snapshots and versions in object storage",
				"bucket", *s3Bucket, "endpoint", *s3Endpoint, "prefix", *s3Prefix)
		}
		if err := db.Migrate(context.Background()); err != nil {
			return err
		}
		persistence = db
		documents = db
		versioning = db
		log.Info("persisting documents", "compact_after", *compactAfter)
		if *versionInterval > 0 {
			log.Info("keeping version history", "every", *versionInterval, "keep", *versionKeep)
		}
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

	var hooks hook.Sink
	if *webhookURL != "" {
		events, err := hookKinds(*webhookEvents)
		if err != nil {
			return err
		}
		webhook, err := hook.NewWebhook(hook.Config{
			URL:     *webhookURL,
			Secret:  []byte(*webhookSecret),
			Events:  events,
			Timeout: *webhookTimeout,
			Retries: *webhookRetries,
			Queue:   *webhookQueue,
			Metrics: collectors,
			Logger:  log,
		})
		if err != nil {
			return err
		}
		// Closed after the rooms have stopped, so the events they emit on their
		// way out are still accepted. Deferred functions run in reverse order,
		// and the rooms are drained at the end of run, so this one is safe here.
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), *shutdown)
			defer cancel()
			if err := webhook.Close(ctx); err != nil {
				log.Warn("webhook queue was not drained", "err", err)
			}
		}()
		hooks = webhook
		log.Info("sending document events", "events", len(events), "state", *webhookState)
	}

	manager := room.NewManager(roomCtx, room.ManagerConfig{
		MaxRooms:  *maxRooms,
		MaxMemory: memoryBudget,
		Room: room.Config{
			IdleTimeout:  *idleTimeout,
			AwarenessTTL: *awarenessTTL,
			AwarenessLimits: protocol.Limits{
				MaxState:   *awarenessMaxState,
				MaxClients: *awarenessMaxClients,
			},
			Tick:            *tick,
			Store:           persistence,
			CompactAfter:    *compactAfter,
			FlushInterval:   flushSetting(*flushInterval, *durableWrites),
			Versions:        versioning,
			VersionInterval: *versionInterval,
			VersionKeep:     *versionKeep,
			Bus:             bus,
			AntiEntropy:     *antiEntropy,
			Hooks:           hooks,
			HookState:       *webhookState,
			Metrics:         collectors,
			Logger:          log,
		},
	})
	if memoryBudget > 0 {
		log.Info("bounding what the documents in memory may cost", "max_memory", memoryBudget)
	} else if *maxRooms > 0 {
		// Worth saying: a count of documents is not a bound on memory, and the
		// person who set -max-rooms probably meant one.
		log.Warn("-max-rooms bounds how many documents are held, not how much memory they use; set -max-memory to bound that")
	}
	if bus != nil {
		log.Info("joined the cluster", "node_id", manager.NodeID(), "anti_entropy", *antiEntropy)
	}
	// The cluster counters are read from the room manager at scrape time rather
	// than mirrored into a second set, so /metrics and /statsz cannot disagree.
	registry.MustRegister(metrics.NewClusterCollector(manager.Stats()))
	registry.MustRegister(metrics.NewResidentBytesCollector(manager.ResidentBytes, memoryBudget))

	authorize, err := authorizer(*jwtSecret, auth.Config{
		Leeway:        *jwtLeeway,
		MaxLifetime:   *jwtMaxLifetime,
		RequireExpiry: *jwtRequireExpiry,
	}, auth.CallbackConfig{
		URL:       *authURL,
		Secret:    []byte(*authSecret),
		Timeout:   *authTimeout,
		CacheTTL:  *authCacheTTL,
		CacheSize: *authCacheSize,
		FailOpen:  *authFailOpen,
		Metrics:   collectors,
		Logger:    log,
	}, log)
	if err != nil {
		return err
	}

	proxies, err := gateway.ParseProxies(splitList(*trustedProxies))
	if err != nil {
		return err
	}
	if proxies.Any() {
		log.Info("believing X-Forwarded-For from the configured proxies", "max_conns_per_ip", *maxConnsPerIP)
	} else if *maxConnsPerIP > 0 {
		// Worth saying: behind a proxy with no -trusted-proxies, every client
		// looks like the proxy, so the per-address cap would be a cap on the
		// whole deployment.
		log.Warn("-max-conns-per-ip is counting socket peer addresses; set -trusted-proxies if this server is behind a load balancer")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok")
	})
	mux.Handle("/", gateway.New(gateway.Config{
		Rooms:         manager,
		Origins:       splitList(*origins),
		MaxConns:      *maxConns,
		MaxConnsPerIP: *maxConnsPerIP,
		Proxies:       proxies,
		Rate: gateway.Rate{
			Messages:     *rateMessages,
			MessageBurst: *rateMessageBurst,
			Bytes:        *rateBytes,
			ByteBurst:    *rateByteBurst,
		},
		Authorize: authorize,
		Metrics:   collectors,
		Logger:    log,
	}))

	recorder, err := openAudit(*auditLog, log)
	if err != nil {
		return err
	}
	defer func() { _ = recorder.Close() }()

	adminSrv, err := startAdmin(*adminAddr, *adminToken, *pprof, recorder, registry, manager, documents, log)
	if err != nil {
		return err
	}
	startRetention(roomCtx, documents, *retention, log)
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
		// The build goes on the line an operator is already reading, so
		// correlating a log with an image never needs a second command.
		log.Info("listening", "addr", *addr, "version", buildVersion())
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
// secrets or callback URL, or returns nil when there is neither.
//
// Nil means open, and the server says so loudly at startup. That is the right
// default for a server you are running on your laptop to try the demo, and the
// wrong one everywhere else, which is why it is a warning rather than silence.
func authorizer(secrets string, cfg auth.Config, cb auth.CallbackConfig, log *slog.Logger) (func(*http.Request, string, string) (gateway.Grant, error), error) {
	// The two are alternatives, not layers. Running both would mean deciding
	// which one wins when they disagree, and every answer to that is a surprise
	// to somebody: a callback that is asked only about tokens this server
	// already accepted cannot implement "this user lost access", which is the
	// main reason to want one. An endpoint that wants JWTs can verify them
	// itself - it gets the token verbatim.
	if secrets != "" && cb.URL != "" {
		return nil, errors.New("-jwt-secret and -auth-url are alternatives: pick the one that decides, and let it decide")
	}

	if cb.URL != "" {
		callback, err := auth.NewCallback(cb)
		if err != nil {
			return nil, err
		}
		log.Info("asking an endpoint about every connection", "cache_ttl", cb.CacheTTL, "fail_open", cb.FailOpen)
		return func(r *http.Request, doc, ip string) (gateway.Grant, error) {
			grant, err := callback.Authorize(r.Context(), auth.Request{
				Document: doc,
				Token:    auth.TokenFromRequest(r),
				IP:       ip,
				Origin:   auth.OriginFromRequest(r),
			})
			if err != nil {
				return gateway.Grant{}, err
			}
			return gateway.Grant{Subject: grant.Subject, Write: grant.Write, Owner: grant.Owner}, nil
		}, nil
	}

	if secrets == "" {
		log.Warn("no -jwt-secret and no -auth-url: every client may read and write every document")
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

	return func(r *http.Request, doc, _ string) (gateway.Grant, error) {
		grant, err := verifier.Verify(auth.TokenFromRequest(r), doc)
		if err != nil {
			// The error text goes to the client. auth's errors are written to be
			// said out loud: they name what is wrong with the token and nothing
			// about the key or the check.
			return gateway.Grant{}, err
		}
		return gateway.Grant{Subject: grant.Subject, Write: grant.Write, Owner: grant.Owner}, nil
	}, nil
}

// hookNames is the flag help's list of event names, taken from the package
// rather than written out again here.
func hookNames() []string {
	names := make([]string, 0, len(hook.Kinds))
	for _, k := range hook.Kinds {
		names = append(names, string(k))
	}
	return names
}

// hookKinds parses -webhook-events. An unknown name is an error rather than a
// warning: silently sending nothing because of a typo is how somebody
// discovers, a week later, that their integration was never running.
func hookKinds(s string) ([]hook.Kind, error) {
	names := splitList(s)
	if len(names) == 0 {
		return hook.Kinds, nil
	}
	kinds := make([]hook.Kind, 0, len(names))
	for _, n := range names {
		k, ok := hook.ParseKind(n)
		if !ok {
			return nil, fmt.Errorf("bad -webhook-events %q: known events are %s", n, strings.Join(hookNames(), ", "))
		}
		kinds = append(kinds, k)
	}
	return kinds, nil
}

// flushSetting turns the two flags into the room's one knob. A negative
// interval is how the room is told to write through, which keeps the option out
// of its config as a second field that could disagree with the first.
func flushSetting(interval time.Duration, durable bool) time.Duration {
	if durable {
		return -1
	}
	return interval
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func splitList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
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
