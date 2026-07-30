package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/mesutokul/ycollab/internal/room"
)

// startAdmin serves the endpoints that are for operators, not for clients:
// /metrics, /statsz and /debug/pprof.
//
// They listen on their own address, defaulting to localhost, and deliberately
// not on the port the world connects to. /debug/pprof will dump the heap, block
// every goroutine for a CPU profile and print the command line the process was
// started with; on a public port that is both an information leak and a way to
// stall the server by asking for a thirty-second profile. Putting them behind a
// separate listener means the deployment decides who can reach them - through a
// bind address, a firewall rule or, in Kubernetes, simply not naming the port
// in the Service.
func startAdmin(addr string, withPprof bool, registry *prometheus.Registry, manager *room.Manager, log *slog.Logger) (*http.Server, error) {
	if addr == "" {
		log.Info("no -admin-addr: metrics, stats and pprof are not served")
		return nil, nil
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		// A metrics endpoint that panics takes the server with it, which is a
		// spectacular way for observability to cause an outage.
		ErrorHandling: promhttp.ContinueOnError,
	}))
	// /statsz stays as it was: the cluster counters as plain JSON, because the
	// Phase 4 tests read them and parsing an exposition format to find out
	// whether updates looped is not a test anybody writes.
	mux.HandleFunc("/statsz", func(w http.ResponseWriter, _ *http.Request) {
		body := map[string]any{
			"node_id": manager.NodeID(),
			"rooms":   manager.Len(),
			"cluster": manager.Stats().Snapshot(),
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(body); err != nil {
			log.Debug("could not write stats", "err", err)
		}
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	if withPprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}

	// Listening here rather than inside the goroutine, so a port that is already
	// taken is an error at startup instead of a line in the log that nobody
	// reads.
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("admin server stopped", "err", err)
		}
	}()
	log.Info("serving admin endpoints", "addr", listener.Addr().String(), "pprof", withPprof)
	return srv, nil
}
