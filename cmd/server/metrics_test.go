package main_test

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mesutokul/ycollab/internal/protocol"
)

// scrape reads the admin endpoint's exposition format and returns the samples,
// keyed by the whole metric line up to the value.
func scrape(t *testing.T, s *server) map[string]float64 {
	t.Helper()
	resp, err := http.Get("http://" + s.admin + "/metrics")
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scrape returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]float64)
	for line := range strings.Lines(string(body)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if v, err := strconv.ParseFloat(value, 64); err == nil {
			out[name] = v
		}
	}
	return out
}

// The endpoint has to be real - a live scrape of a live server - because the
// two things that break instrumentation are a collector that is never
// registered and one that is never incremented, and neither shows up in a unit
// test of the metrics package.
func TestMetricsReportWhatTheServerDid(t *testing.T) {
	srv := startServer(t, buildServer(t), freePort(t), "")
	doc := fmt.Sprintf("metrics-%d", time.Now().UnixNano())

	before := scrape(t, srv)
	for _, name := range []string{
		"ycollab_connections_open",
		"ycollab_connections_total",
		"ycollab_rooms_resident",
		"go_goroutines",
	} {
		if _, ok := before[name]; !ok {
			t.Fatalf("%s is not exposed", name)
		}
	}

	a := dialRaw(t, srv.addr, doc)
	a.sync()
	b := dialRaw(t, srv.addr, doc)
	b.sync()

	update := readFixture(t, "text-insert-single", "update-000.bin")
	a.send(protocol.WriteUpdate(update))
	if _, err := b.recvUpdate(10 * time.Second); err != nil {
		t.Fatalf("the peer never received the update: %v", err)
	}

	after := scrape(t, srv)
	checks := []struct {
		name string
		want float64
	}{
		{"ycollab_connections_total", 2},
		{"ycollab_connections_open", 2},
		{"ycollab_rooms_resident", 1},
		{`ycollab_messages_received_total{type="sync_step1"}`, 2},
		{`ycollab_messages_received_total{type="update"}`, 1},
		{"ycollab_frames_sent_total", 1},
		{"ycollab_apply_duration_seconds_count", 1},
		{"ycollab_update_bytes_count", 1},
	}
	for _, c := range checks {
		if got := after[c.name] - before[c.name]; got < c.want {
			t.Fatalf("%s went up by %v, want at least %v", c.name, got, c.want)
		}
	}
	if after["ycollab_bytes_received_total"] <= before["ycollab_bytes_received_total"] {
		t.Fatal("no bytes were counted as received")
	}
	if after["ycollab_bytes_sent_total"] <= before["ycollab_bytes_sent_total"] {
		t.Fatal("no bytes were counted as sent")
	}
}

// The operator endpoints are on their own listener, and the client port serves
// none of them: pprof on a public port dumps the heap and the command line to
// anybody who asks, and lets them stall the server with a thirty-second
// profile.
func TestAdminEndpointsAreNotOnTheClientPort(t *testing.T) {
	srv := startServer(t, buildServer(t), freePort(t), "")

	for _, path := range []string{"/metrics", "/statsz", "/debug/pprof/"} {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get("http://" + srv.addr + path)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			defer resp.Body.Close()
			// The gateway treats any path as a document name and refuses a plain
			// HTTP request; what matters is that it is not the admin handler.
			if resp.StatusCode == http.StatusOK {
				t.Fatalf("%s is served on the client port", path)
			}

			admin, err := http.Get("http://" + srv.admin + path)
			if err != nil {
				t.Fatalf("admin get: %v", err)
			}
			defer admin.Body.Close()
			if admin.StatusCode != http.StatusOK {
				t.Fatalf("%s returned %d on the admin port", path, admin.StatusCode)
			}
		})
	}
}

// And pprof can be turned off while metrics stay on, because a deployment that
// cannot firewall the admin port still wants the numbers.
func TestPprofCanBeDisabled(t *testing.T) {
	srv := startServer(t, buildServer(t), freePort(t), "", "-pprof=false")

	resp, err := http.Get("http://" + srv.admin + "/debug/pprof/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("pprof answered %d with -pprof=false", resp.StatusCode)
	}

	metrics, err := http.Get("http://" + srv.admin + "/metrics")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer metrics.Body.Close()
	if metrics.StatusCode != http.StatusOK {
		t.Fatalf("metrics answered %d", metrics.StatusCode)
	}
}

// Deleting a document is an operator action on the admin listener, and it is
// refused while somebody is editing rather than pulled out from under them.
func TestDeletingADocument(t *testing.T) {
	dbURL := os.Getenv(dbEnv)
	if dbURL == "" {
		t.Skipf("%s is not set; start deploy/docker-compose.yml to run this", dbEnv)
	}
	srv := startServer(t, buildServer(t), freePort(t), dbURL)
	doc := fmt.Sprintf("deleteme-%d", time.Now().UnixNano())

	c := dialRaw(t, srv.addr, doc)
	c.sync()
	c.send(protocol.WriteUpdate(readFixture(t, "text-insert-single", "update-000.bin")))
	waitPersisted(t, dbURL, doc, 1)

	// In use: refused.
	req, err := http.NewRequest(http.MethodDelete, "http://"+srv.admin+"/documents/"+doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("deleting a document in use returned %d, want 409", resp.StatusCode)
	}

	// Once the editor is gone it can be deleted, and the content goes with it.
	c.ws.Close(websocket.StatusNormalClosure, "")
	deadline := time.Now().Add(15 * time.Second)
	for {
		req, err := http.NewRequest(http.MethodDelete, "http://"+srv.admin+"/documents/"+doc, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNoContent {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the document was never deletable: last status %d", resp.StatusCode)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// A client opening it now gets an empty document, not the old one.
	fresh := dialRaw(t, srv.addr, doc)
	if got := textOf(t, fresh.sync()); got != "" {
		t.Fatalf("the deleted document came back as %q", got)
	}
}
