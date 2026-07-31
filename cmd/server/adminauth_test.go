package main_test

import (
	"bytes"
	"fmt"
	"net/http"
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
