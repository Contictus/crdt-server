package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"

	"github.com/mesutokul/ycollab/internal/audit"
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

// adminTokens is the set of credentials that may use the administrative
// surface.
//
// It is a set rather than one secret for the same reason -jwt-secret takes
// several: a single shared token cannot be rotated without a window in which
// either the old one is gone before its holders were updated, or the new one is
// not accepted yet. Two at once makes the rotation "add, update the holders,
// remove", and the audit log's credential fingerprint is how the last step is
// made safe - it says whether the old token is still being used before anybody
// takes it away.
type adminTokens struct {
	// digests are the SHA-256 of each token. Comparison is over digests so it
	// is over a fixed length whatever the tokens are: ConstantTimeCompare
	// returns early on a length mismatch, which would otherwise leak the
	// secret's length.
	digests [][32]byte
	// prints are the audit fingerprints, in the same order.
	prints []string
}

// parseAdminTokens reads the comma-separated flag. Empty entries are dropped,
// so a trailing comma is not a token that matches nothing.
func parseAdminTokens(spec string) adminTokens {
	var t adminTokens
	for _, s := range splitList(spec) {
		t.digests = append(t.digests, sha256.Sum256([]byte(s)))
		t.prints = append(t.prints, audit.Fingerprint(s))
	}
	return t
}

func (t adminTokens) enabled() bool { return len(t.digests) > 0 }

// match reports which credential a request presented, if any.
//
// Every token is compared even after one has matched. Stopping early would make
// how long the check takes depend on which token matched, which is a small leak
// about the order of a list that is not secret - but the loop is over at most a
// handful of entries, and a check that is constant-time except for the part
// somebody added later is the usual way this goes wrong.
func (t adminTokens) match(r *http.Request) (string, bool) {
	// Header only. A token in the query string ends up in access logs, in
	// browser history and in the Referer of anything the response links to;
	// the WebSocket endpoint accepts one there because a browser cannot set a
	// header on a WebSocket handshake, and nothing on this listener has that
	// excuse.
	header := r.Header.Get("Authorization")
	scheme, value, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return audit.CredentialBad, false
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(value)))

	found := -1
	for i, want := range t.digests {
		if subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
			found = i
		}
	}
	if found < 0 {
		return audit.CredentialBad, false
	}
	return t.prints[found], true
}

// credentialKey carries the fingerprint from the token check to the audit
// middleware, which runs inside it.
type credentialKey struct{}

// credentialOf is the fingerprint of whatever authorised this request.
func credentialOf(r *http.Request) string {
	if s, ok := r.Context().Value(credentialKey{}).(string); ok {
		return s
	}
	return audit.CredentialNone
}

// requireToken wraps a handler so it only runs for requests carrying a token.
//
// No tokens means the check is off, and the caller says so at startup. The
// handler is returned unchanged in that case rather than wrapped with a
// permit-everything check, so there is no configuration under which a bug in
// this file could start letting requests through that should not.
//
// A refusal is audited here rather than by the middleware inside, because the
// request never gets that far - and a request that was refused is the most
// interesting line the audit log has: somebody without the credential using the
// surface anyway.
func requireToken(tokens adminTokens, rec *audit.Recorder, next http.Handler) http.Handler {
	if !tokens.enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		print, ok := tokens.match(r)
		if !ok {
			rec.Record(audit.Event{
				Action:     actionOf(r),
				Status:     http.StatusUnauthorized,
				Credential: audit.CredentialBad,
				IP:         requestIP(r),
				Method:     r.Method,
				Path:       r.URL.Path,
				Err:        "unauthorized",
			})
			w.Header().Set("WWW-Authenticate", adminRealm)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, withCredential(r, print))
	})
}

func withCredential(r *http.Request, print string) *http.Request {
	return r.WithContext(contextWithCredential(r, print))
}

// warnIfOpen says out loud that the admin surface is unauthenticated, the same
// way the server does about running without a signing secret. It is a warning
// rather than a refusal because a laptop running the demo has nothing to
// protect, and because the listener defaults to localhost.
func warnIfOpen(tokens adminTokens, addr string, log *slog.Logger) {
	if tokens.enabled() {
		log.Info("requiring a token on the admin endpoints", "addr", addr, "credentials", len(tokens.digests))
		return
	}
	log.Warn("no -admin-token: anyone who can reach the admin address may read, overwrite and delete every document",
		"addr", addr)
}
