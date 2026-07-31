package main

import (
	"context"
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
	"github.com/mesutokul/ycollab/internal/store"
)

// deleteDocument removes a document from memory and from the database.
//
// It is on the admin listener, which is the authorisation: this endpoint is
// destructive and irreversible, and the deployment decides who can reach the
// port. A token would be the wrong answer here - the tokens this server
// understands are per-document capabilities minted for editors, not operator
// credentials.
//
// A document somebody is currently editing is refused rather than deleted out
// from under them. Once the room is out of memory, nothing is left to write a
// snapshot back, though a client connecting the moment afterwards will of course
// create the document again, empty.
func deleteDocument(manager *room.Manager, documents *store.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "" {
			http.Error(w, "no document name", http.StatusBadRequest)
			return
		}
		if !manager.Evict(name) {
			http.Error(w, "the document is in use", http.StatusConflict)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		existed, err := documents.Delete(ctx, store.DocumentID(name))
		if err != nil {
			log.Error("could not delete document", "document", name, "err", err)
			http.Error(w, "could not delete the document", http.StatusInternalServerError)
			return
		}
		log.Warn("document deleted", "document", name, "existed", existed)
		if !existed {
			http.Error(w, "no such document", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// retentionInterval is how often idle documents are swept. Retention is
// measured in days, so sweeping more often than this buys nothing and costs a
// scan.
const retentionInterval = 6 * time.Hour

// startRetention deletes documents nothing has touched for the given period,
// on a timer, until ctx ends.
//
// Off unless asked for, and the flag takes a duration rather than a count of
// days so that "how long is a document kept" is a decision the deployment
// states rather than one this server assumes. A collaborative document nobody
// has opened in months is still somebody's document; deleting it by default
// would be the server making a policy call it has no standing to make.
func startRetention(ctx context.Context, documents *store.Store, keep time.Duration, log *slog.Logger) {
	if documents == nil || keep <= 0 {
		return
	}
	log.Info("deleting documents that go untouched", "for", keep, "checked_every", retentionInterval)
	sweep := func() {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		n, err := documents.DeleteIdle(ctx, time.Now().Add(-keep))
		if err != nil {
			log.Error("retention sweep failed", "err", err)
			return
		}
		if n > 0 {
			log.Warn("deleted idle documents", "count", n, "idle_for", keep)
		}
	}
	go func() {
		// One at startup, so a deployment that sets the flag does not wait six
		// hours to find out whether it works.
		sweep()
		ticker := time.NewTicker(retentionInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweep()
			}
		}
	}()
}

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
func startAdmin(addr string, withPprof bool, registry *prometheus.Registry, manager *room.Manager, documents *store.Store, log *slog.Logger) (*http.Server, error) {
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
	// Reading works with or without a database: a resident room can be read
	// from memory, and without a store that is the only copy there is. A typed
	// nil would satisfy the interface and then panic, so the conversion is
	// explicit.
	var persistence room.Persistence
	if documents != nil {
		persistence = documents
		mux.HandleFunc("DELETE /documents/{name}", deleteDocument(manager, documents, log))
	}
	mux.HandleFunc("GET /documents/{name}", getDocument(manager, persistence, log))
	mux.HandleFunc("POST /documents/{name}", mergeDocument(manager, persistence, log))
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
