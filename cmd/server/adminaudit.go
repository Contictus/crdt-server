package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mesutokul/ycollab/internal/audit"
)

// The administrative surface's audit trail: what was attempted, against which
// document, by which credential, and what came of it.
//
// The wrapping is per route rather than one layer around the whole mux, for a
// reason that is not style. http.ServeMux fills in the path wildcards - the
// document name - as part of matching, so a wrapper registered *as* a route's
// handler sees them and a wrapper outside the mux does not. The alternative was
// a second copy of the routing table inside the audit layer, parsing
// "/documents/{name}/versions/{id}" again and drifting from the real one.

// openAudit builds the recorder from the flag.
//
// The default is stdout, and it is a default rather than something to switch on
// because an audit trail nobody enabled is an audit trail nobody has on the day
// it is needed. It costs a line per administrative request, and the process log
// is on stderr, so the two streams are already separate wherever they are
// collected - a container, a unit file, a shell with `>` - with no
// configuration.
//
// A path is for a deployment that wants the trail somewhere its log collector
// is not. Nothing here rotates that file: an audit trail that a server truncates
// on its own schedule is a strange thing to hand an auditor, and logrotate
// already exists.
func openAudit(dest string, log *slog.Logger) (*audit.Recorder, error) {
	switch dest {
	case "":
		log.Warn("no -audit-log: nothing records who reads, overwrites or deletes documents through the admin endpoints")
		return audit.Discard(), nil
	case "-":
		log.Info("writing an audit record per admin request", "to", "stdout")
		return audit.New(os.Stdout, false), nil
	}
	// Append, never truncate: a restart must not be a way to lose the trail.
	f, err := os.OpenFile(dest, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit log: %w", err)
	}
	log.Info("writing an audit record per admin request", "to", dest)
	return audit.New(f, true), nil
}

// contextWithCredential is here rather than in adminauth.go so the key, the
// getter and the setter are next to the thing that reads them.
func contextWithCredential(r *http.Request, print string) context.Context {
	return context.WithValue(r.Context(), credentialKey{}, print)
}

// audited wraps a handler so that every request through it produces one audit
// record, whatever the outcome.
func audited(rec *audit.Recorder, action string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &auditedWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)

		// A write is measured by what came in and a read by what went out. Both
		// are sizes; neither is content.
		size := rw.written
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			size = r.ContentLength
			if size < 0 {
				size = 0
			}
		}
		rec.Record(audit.Event{
			Action:     action,
			Status:     rw.status,
			Document:   r.PathValue("name"),
			Credential: credentialOf(r),
			IP:         requestIP(r),
			Method:     r.Method,
			Path:       r.URL.Path,
			Bytes:      size,
			DurationMS: time.Since(start).Milliseconds(),
			Err:        rw.problem(),
		})
	})
}

// auditedFunc is audited for the handler shape the routes are written in.
func auditedFunc(rec *audit.Recorder, action string, next http.HandlerFunc) http.HandlerFunc {
	h := audited(rec, action, next)
	return h.ServeHTTP
}

// auditedWriter watches the status code and the size going past.
//
// It keeps the first line of an error body too. Every failure on this surface is
// answered with http.Error, whose body is the same short sentence the caller
// was told, so recording it says nothing the client does not already know and
// turns "status 500" into "could not delete the document".
type auditedWriter struct {
	http.ResponseWriter
	status      int
	written     int64
	wroteHeader bool
	// firstLine is captured only for a failure, so a 200 response body - which
	// is a document - never reaches the audit trail.
	firstLine string
}

func (w *auditedWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *auditedWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.status >= 400 && w.firstLine == "" {
		w.firstLine = firstLineOf(b)
	}
	n, err := w.ResponseWriter.Write(b)
	w.written += int64(n)
	return n, err
}

// Flush keeps a streaming handler working through the wrapper. promhttp uses
// one, and a ResponseWriter that silently stops implementing http.Flusher is the
// kind of thing that shows up as a hung scrape rather than as an error.
func (w *auditedWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		if !w.wroteHeader {
			w.WriteHeader(http.StatusOK)
		}
		f.Flush()
	}
}

func (w *auditedWriter) problem() string {
	if w.status < 400 {
		return ""
	}
	if w.firstLine != "" {
		return w.firstLine
	}
	return strconv.Itoa(w.status)
}

// firstLineOf takes the first line of an error body, bounded. http.Error writes
// one short sentence; anything longer is not something this trail should carry.
func firstLineOf(b []byte) string {
	const max = 200
	if len(b) > max {
		b = b[:max]
	}
	s := strings.TrimSpace(string(b))
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return s
}

// requestIP is the peer address of an administrative request.
//
// Deliberately the socket's peer and not X-Forwarded-For, unlike the WebSocket
// endpoint. This listener is meant to be reached directly - it defaults to
// localhost and the deployment decides who can route to it - so there is no
// trusted proxy to believe, and believing the header would let the caller write
// its own address into the audit trail.
func requestIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// actionOf names a request in the server's vocabulary, for the refusals that
// never reach a route and so have no action of their own.
//
// It is a prefix match rather than the routing table because it is only ever
// asked about requests that were turned away at the door: getting "which shape
// of thing were they after" right is enough, and a wrong guess here cannot
// affect what the server does.
func actionOf(r *http.Request) string {
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/documents/"):
		if strings.Contains(path, "/versions") {
			return "version.access"
		}
		return "document.access"
	case strings.HasPrefix(path, "/debug/pprof"):
		return actionProfile
	case path == "/metrics":
		return actionMetrics
	case path == "/statsz":
		return actionStats
	default:
		return "unknown"
	}
}

// The vocabulary. Named constants rather than literals at the call sites,
// because these strings are what somebody greps for a year from now and a typo
// in one of them is a query that silently returns nothing.
const (
	actionDocumentRead   = "document.read"
	actionDocumentWrite  = "document.write"
	actionDocumentDelete = "document.delete"
	actionVersionList    = "version.list"
	actionVersionRead    = "version.read"
	actionVersionTake    = "version.take"
	actionProfile        = "profile.read"
	actionStats          = "stats.read"
	actionMetrics        = "metrics.read"
)
