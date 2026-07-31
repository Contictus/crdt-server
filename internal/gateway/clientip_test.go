package gateway_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mesutokul/ycollab/internal/gateway"
)

// X-Forwarded-For is a request header, so anyone can send one saying anything.
// The whole design rests on it being read only when the machine we are actually
// talking to is a proxy the operator named, and these are the cases that go
// wrong when that is got subtly right instead of exactly right.
func TestClientIP(t *testing.T) {
	for _, tc := range []struct {
		name    string
		proxies []string
		peer    string
		xff     []string
		want    string
	}{
		{
			// The one that matters most: with no proxies configured, the header
			// is not consulted at all, so a client cannot choose its own
			// identity.
			name: "an unconfigured server ignores the header entirely",
			peer: "203.0.113.9:5555", xff: []string{"1.2.3.4"},
			want: "203.0.113.9",
		},
		{
			// And configuring a proxy does not make the header believable from
			// somewhere else.
			name: "a header from an untrusted peer is ignored", proxies: []string{"10.0.0.0/8"},
			peer: "203.0.113.9:5555", xff: []string{"1.2.3.4"},
			want: "203.0.113.9",
		},
		{
			name: "a trusted proxy's header is believed", proxies: []string{"10.0.0.0/8"},
			peer: "10.1.2.3:5555", xff: []string{"198.51.100.7"},
			want: "198.51.100.7",
		},
		{
			// The rightmost untrusted hop is the last address a machine we
			// trust actually saw. The leftmost is the part the client wrote,
			// and taking it is the classic mistake.
			name: "the rightmost untrusted hop wins", proxies: []string{"10.0.0.0/8"},
			peer: "10.1.2.3:5555", xff: []string{"1.1.1.1, 198.51.100.7, 10.9.9.9"},
			want: "198.51.100.7",
		},
		{
			name: "hops split across repeated headers", proxies: []string{"10.0.0.0/8"},
			peer: "10.1.2.3:5555", xff: []string{"1.1.1.1", "198.51.100.7", "10.9.9.9"},
			want: "198.51.100.7",
		},
		{
			name: "a chain of nothing but trusted hops falls back to the peer", proxies: []string{"10.0.0.0/8"},
			peer: "10.1.2.3:5555", xff: []string{"10.4.4.4, 10.9.9.9"},
			want: "10.1.2.3",
		},
		{
			// A hop that will not parse means the chain cannot be reasoned
			// about past it, and everything further left is client-controlled.
			name: "a malformed hop stops the walk", proxies: []string{"10.0.0.0/8"},
			peer: "10.1.2.3:5555", xff: []string{"198.51.100.7, not-an-address"},
			want: "10.1.2.3",
		},
		{
			name: "no header at all", proxies: []string{"10.0.0.0/8"},
			peer: "10.1.2.3:5555",
			want: "10.1.2.3",
		},
		{
			name: "loopback shorthand", proxies: []string{"loopback"},
			peer: "127.0.0.1:5555", xff: []string{"198.51.100.7"},
			want: "198.51.100.7",
		},
		{
			name: "private shorthand covers a cluster network", proxies: []string{"private"},
			peer: "172.20.4.9:5555", xff: []string{"198.51.100.7"},
			want: "198.51.100.7",
		},
		{
			// An IPv4 client on a dual-stack listener arrives as
			// ::ffff:a.b.c.d, which matches no IPv4 prefix until it is unmapped.
			name: "an IPv4-mapped peer is unmapped before matching", proxies: []string{"10.0.0.0/8"},
			peer: "[::ffff:10.1.2.3]:5555", xff: []string{"198.51.100.7"},
			want: "198.51.100.7",
		},
		{
			name: "IPv6 throughout", proxies: []string{"2001:db8::/32"},
			peer: "[2001:db8::1]:5555", xff: []string{"2001:db8:ffff::9"},
			want: "2001:db8::1",
		},
		{
			name: "a bare address is a trusted proxy of one", proxies: []string{"192.0.2.5"},
			peer: "192.0.2.5:5555", xff: []string{"198.51.100.7"},
			want: "198.51.100.7",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxies, err := gateway.ParseProxies(tc.proxies)
			if err != nil {
				t.Fatalf("ParseProxies: %v", err)
			}
			r := httptest.NewRequest(http.MethodGet, "/doc", nil)
			r.RemoteAddr = tc.peer
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := proxies.ClientIP(r); got != tc.want {
				t.Errorf("ClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// A typo in a CIDR block is the difference between a trusted proxy list and an
// empty one, so it is an error at startup rather than a silently narrower set.
func TestParseProxiesRejectsGarbage(t *testing.T) {
	for _, entry := range []string{"nonsense", "10.0.0.0/99", "10.0.0.0/", "1.2.3.4.5"} {
		if _, err := gateway.ParseProxies([]string{entry}); err == nil {
			t.Errorf("ParseProxies accepted %q", entry)
		}
	}
	// Empty entries are skipped, so a trailing comma in the flag is not fatal.
	p, err := gateway.ParseProxies([]string{"", "  "})
	if err != nil {
		t.Fatalf("ParseProxies: %v", err)
	}
	if p.Any() {
		t.Error("blank entries produced a non-empty proxy set")
	}
}

// MaxConns alone is a limit one address can reach on its own, which makes "the
// node is full" something a single client decides.
func TestOneAddressCannotFillTheNode(t *testing.T) {
	srv := newServer(t, gateway.Config{MaxConnsPerIP: 2})

	// Every connection in this test comes from 127.0.0.1, which is the point.
	a := dial(t, srv, "doc")
	_ = dial(t, srv, "doc")

	code, body := dialExpectingRefusal(t, srv, "doc")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("the third connection got %d, want 503: %s", code, body)
	}

	// A slot frees when a connection goes, or the cap would be permanent after
	// the first burst.
	_ = a.ws.CloseNow()
	waitUntil(t, "a slot to free up", func() bool {
		code, _ := dialExpectingRefusal(t, srv, "doc")
		return code == 0
	})
}

// The cap must not be a cap on the whole server: one address at its limit says
// nothing about anybody else.
func TestTheCapIsPerAddressNotGlobal(t *testing.T) {
	// With a trusted loopback proxy the test can present two different client
	// addresses over one local socket, which is exactly what the header is for.
	srv := newServer(t, gateway.Config{
		MaxConnsPerIP: 1,
		Proxies:       mustProxies(t, "loopback"),
	})

	_ = dialAs(t, srv, "doc", "198.51.100.1")
	// The same address again is refused.
	if code, _ := dialAsExpectingRefusal(t, srv, "doc", "198.51.100.1"); code != http.StatusServiceUnavailable {
		t.Fatalf("a second connection from the same address got %d, want 503", code)
	}
	// A different one is not.
	_ = dialAs(t, srv, "doc", "198.51.100.2")
}

// waitUntil polls until cond holds, so a test never guesses how long a handler
// takes to unwind after its socket closes.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func dialContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func mustProxies(t *testing.T, entries ...string) gateway.Proxies {
	t.Helper()
	p, err := gateway.ParseProxies(entries)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// dialAs opens a connection presenting a client address through the header.
func dialAs(t *testing.T, srv *httptest.Server, doc, ip string) *client {
	t.Helper()
	ws, _, err := websocket.Dial(dialContext(t), srv.URL+"/"+doc, &websocket.DialOptions{
		HTTPHeader: http.Header{"X-Forwarded-For": {ip}},
	})
	if err != nil {
		t.Fatalf("dial as %s: %v", ip, err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	return &client{t: t, ws: ws}
}

// dialAsExpectingRefusal reports the HTTP status of a refused upgrade, or 0
// when the connection was accepted.
func dialAsExpectingRefusal(t *testing.T, srv *httptest.Server, doc, ip string) (int, string) {
	t.Helper()
	header := http.Header{}
	if ip != "" {
		header.Set("X-Forwarded-For", ip)
	}
	ws, resp, err := websocket.Dial(dialContext(t), srv.URL+"/"+doc, &websocket.DialOptions{HTTPHeader: header})
	if err == nil {
		_ = ws.CloseNow()
		return 0, ""
	}
	if resp == nil {
		t.Fatalf("dial failed without a response: %v", err)
	}
	return resp.StatusCode, err.Error()
}

func dialExpectingRefusal(t *testing.T, srv *httptest.Server, doc string) (int, string) {
	t.Helper()
	return dialAsExpectingRefusal(t, srv, doc, "")
}
