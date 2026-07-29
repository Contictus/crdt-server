// Command server runs a single ycollab node: a WebSocket endpoint that any
// unmodified y-websocket client can point at.
//
//	go run ./cmd/server -addr :8080 -origins localhost:5173
//
// The document name is the URL path, so ws://host:8080/my-doc is document
// "my-doc". There is no persistence yet: this is the Phase 2 server, and a
// document lives only as long as a room is resident.
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

	"github.com/mesutokul/ycollab/internal/gateway"
	"github.com/mesutokul/ycollab/internal/room"
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
	)
	flag.Parse()

	level, err := parseLevel(*logLevel)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	// Rooms stop when this context is cancelled; the HTTP server is drained
	// first so that connections get a close frame rather than a dead socket.
	roomCtx, stopRooms := context.WithCancel(context.Background())
	defer stopRooms()

	manager := room.NewManager(roomCtx, room.ManagerConfig{
		MaxRooms: *maxRooms,
		Room: room.Config{
			IdleTimeout:  *idleTimeout,
			AwarenessTTL: *awarenessTTL,
			Logger:       log,
		},
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok")
	})
	mux.Handle("/", gateway.New(gateway.Config{
		Rooms:   manager,
		Origins: splitList(*origins),
		Logger:  log,
	}))

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
