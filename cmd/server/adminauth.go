package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
)

// The admin listener started out read-only - metrics, stats, pprof - and the
// argument for having no authentication was that the deployment decides who can
// reach the port. Then it grew DELETE, and then POST, and now a request that
// reaches it can rewrite any document on the server. Network isolation is still
// the first control, but it is a single control, and one misconfigured Service
// or one forwarded port is the whole distance between "internal" and "anybody".
//
// -admin-token adds the second. It is deliberately not the JWT machinery the
// clients use: those tokens are per-document capabilities minted for editors,
// and "may administer this server" is not a document capability. A shared
// secret is the right shape for a surface whose users are an operator, a
// scrape job and a backup script.

// adminRealm is what a 401 advertises. Bearer, not Basic: nothing here should
// make a browser pop up a password box.
const adminRealm = `Bearer realm="ycollab"`

// requireToken wraps a handler so it only runs for requests carrying the token.
//
// An empty token means the check is off, and the caller says so at startup. The
// handler is returned unchanged in that case rather than wrapped with a
// permit-everything check, so there is no configuration under which a bug in
// this file could start letting requests through that should not.
func requireToken(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	// Compared as digests so the comparison is over a fixed length whatever the
	// tokens are: ConstantTimeCompare returns early on a length mismatch, which
	// would otherwise leak the secret's length.
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authorized(want, r) {
			w.Header().Set("WWW-Authenticate", adminRealm)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func authorized(want [32]byte, r *http.Request) bool {
	// Header only. A token in the query string ends up in access logs, in
	// browser history and in the Referer of anything the response links to;
	// the WebSocket endpoint accepts one there because a browser cannot set a
	// header on a WebSocket handshake, and nothing on this listener has that
	// excuse.
	header := r.Header.Get("Authorization")
	scheme, value, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return false
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

// warnIfOpen says out loud that the admin surface is unauthenticated, the same
// way the server does about running without a signing secret. It is a warning
// rather than a refusal because a laptop running the demo has nothing to
// protect, and because the listener defaults to localhost.
func warnIfOpen(token, addr string, log *slog.Logger) {
	if token != "" {
		log.Info("requiring a token on the admin endpoints", "addr", addr)
		return
	}
	log.Warn("no -admin-token: anyone who can reach the admin address may read, overwrite and delete every document",
		"addr", addr)
}
