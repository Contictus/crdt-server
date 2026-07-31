package main_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The admin listener can rewrite and delete any document on the server, so a
// token has to actually gate it - every endpoint, not the ones somebody
// remembered.
func TestTheAdminEndpointsRequireTheToken(t *testing.T) {
	const token = "admin-test-token"
	srv := startServer(t, buildServer(t), freePort(t), "", "-admin-token", token)
	doc := fmt.Sprintf("guarded-%d", time.Now().UnixNano())

	// Something to try to read, created over a real connection.
	c := dial(t, srv.addr, doc)
	c.sync()

	for _, path := range []string{
		"/metrics",
		"/statsz",
		"/documents/" + doc,
		"/debug/pprof/",
	} {
		t.Run(path, func(t *testing.T) {
			// No credential at all.
			if code, _ := adminGet(t, srv, path, ""); code != http.StatusUnauthorized {
				t.Errorf("without a token: %d, want 401", code)
			}
			// A wrong one, and one that is a prefix of the right one - the
			// comparison must not stop at the first differing byte or at a
			// length mismatch.
			for _, bad := range []string{"wrong", token[:len(token)-1], token + "x", ""} {
				if code, _ := adminGet(t, srv, path, bad); code != http.StatusUnauthorized {
					t.Errorf("with token %q: %d, want 401", bad, code)
				}
			}
			// The right one.
			if code, body := adminGet(t, srv, path, token); code != http.StatusOK {
				t.Errorf("with the token: %d, want 200: %s", code, body)
			}
		})
	}

	// The mutating verbs are behind it too, and 401 rather than a document
	// being merged or deleted is the whole point.
	if code := adminPost(t, srv, "/documents/"+doc, []byte{1, 2, 3}, ""); code != http.StatusUnauthorized {
		t.Errorf("unauthenticated POST: %d, want 401", code)
	}
	if code := adminDelete(t, srv, "/documents/"+doc, ""); code != http.StatusUnauthorized {
		t.Errorf("unauthenticated DELETE: %d, want 401", code)
	}
	// And the document is still there, which is the assertion that matters.
	if code, _ := adminGet(t, srv, "/documents/"+doc, token); code != http.StatusOK {
		t.Errorf("the document is gone after a refused DELETE: %d", code)
	}
}

// A load balancer probing liveness cannot be expected to hold an operator
// credential, and the answer says nothing about the documents.
func TestHealthzStaysOpen(t *testing.T) {
	srv := startServer(t, buildServer(t), freePort(t), "", "-admin-token", "admin-test-token")
	if code, body := adminGet(t, srv, "/healthz", ""); code != http.StatusOK {
		t.Fatalf("/healthz without a token: %d, want 200: %s", code, body)
	}
}

// Only the Authorization header. A token in a query string ends up in access
// logs, in browser history and in the Referer of anything the response links to.
func TestTheTokenIsNotAcceptedInTheQueryString(t *testing.T) {
	const token = "admin-test-token"
	srv := startServer(t, buildServer(t), freePort(t), "", "-admin-token", token)
	if code, _ := adminGet(t, srv, "/statsz?token="+token, ""); code != http.StatusUnauthorized {
		t.Errorf("a token in the query string was accepted: %d", code)
	}
}

// Without the flag the endpoints stay open, which is what every earlier test in
// this package relies on and what a laptop running the demo wants.
func TestWithoutATokenTheAdminEndpointsAreOpen(t *testing.T) {
	srv := startServer(t, buildServer(t), freePort(t), "")
	if code, body := adminGet(t, srv, "/statsz", ""); code != http.StatusOK {
		t.Fatalf("/statsz: %d, want 200: %s", code, body)
	}
	// And the server says so, because an open admin surface is worth a line in
	// the log rather than silence.
	if !bytes.Contains(srv.logs.Bytes(), []byte("no -admin-token")) {
		t.Error("the server did not warn that its admin endpoints are open")
	}
}

func adminRequest(t *testing.T, s *server, method, path, token string, body []byte) (int, []byte) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, "http://"+s.admin+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out := make([]byte, 512)
	n, _ := resp.Body.Read(out)
	return resp.StatusCode, out[:n]
}

func adminGet(t *testing.T, s *server, path, token string) (int, []byte) {
	t.Helper()
	return adminRequest(t, s, http.MethodGet, path, token, nil)
}

func adminPost(t *testing.T, s *server, path string, body []byte, token string) int {
	t.Helper()
	code, _ := adminRequest(t, s, http.MethodPost, path, token, body)
	return code
}

func adminDelete(t *testing.T, s *server, path, token string) int {
	t.Helper()
	code, _ := adminRequest(t, s, http.MethodDelete, path, token, nil)
	return code
}

// The flag became comma-separated so a token can be rotated. That change can
// split a credential somebody was already using, and the dangerous part is not
// that the whole token stops working - it is that each fragment starts working.
// "sk_live_abcdef,ghi" would have made "ghi" an administrator password.
func TestAnAdminTokenFragmentIsRefusedAtStartup(t *testing.T) {
	binary := buildServer(t)
	for _, tc := range []struct {
		name  string
		token string
		start bool
	}{
		{"a token cut in half by a comma", "zqxfirsthalf,zqxsecondhalf", false},
		{"one short token", "short", false},
		{"a trailing comma is not a token", "a-perfectly-good-admin-token,", true},
		{"a real rotation", "an-old-admin-token-x,a-new-admin-token-y", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Bounded, because a server given a usable token does not exit on
			// its own: the question here is only whether it refuses to start.
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "-addr", freePort(t), "-admin-addr", freePort(t),
				"-admin-token", tc.token)
			out, err := cmd.CombinedOutput()
			if tc.start {
				// It had to be killed rather than exiting, so err is the kill.
				// The only failure this test cares about is a refusal.
				if strings.Contains(string(out), "admin-token") {
					t.Fatalf("a usable token was refused:\n%s", out)
				}
				return
			}
			if err == nil {
				t.Fatalf("the server started with %q:\n%s", tc.token, out)
			}
			if !strings.Contains(string(out), "admin-token") {
				t.Fatalf("it failed for some other reason:\n%s", out)
			}
			// And the refusal must not print the credential it is complaining
			// about, in a message that goes to a log. Only distinctive pieces
			// are checked: a fragment like "a" appears in ordinary log prose,
			// and matching on it would fail this test for the wrong reason.
			for _, piece := range strings.Split(tc.token, ",") {
				if len(piece) >= 5 && strings.Contains(string(out), piece) {
					t.Errorf("the error printed the token: %s", out)
				}
			}
		})
	}
}
