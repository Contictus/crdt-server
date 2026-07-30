package metrics_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/mesutokul/ycollab/internal/metrics"
)

// Every collector has to reach the registry: one that is built and never
// registered is invisible, and nothing else in the system would notice.
func TestEveryCollectorIsRegistered(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)

	// Touch the vectors, because a CounterVec with no observed label values
	// gathers as nothing at all.
	m.ConnectionsOpen.Inc()
	m.ConnectionsTotal.Inc()
	m.CloseCode(1008)
	m.RoomsResident.Set(3)
	m.RoomsStarted.Inc()
	m.MessagesReceived.WithLabelValues("update").Inc()
	m.FramesSent.Inc()
	m.FramesDropped.Inc()
	m.BytesReceived.Add(10)
	m.BytesSent.Add(20)
	m.ApplyDuration.Observe(0.001)
	m.ApplyFailed.Inc()
	m.UpdateBytes.Observe(64)
	m.Denied.WithLabelValues("read_only").Inc()
	m.StoreDuration.WithLabelValues("append").Observe(0.01)
	m.StoreFailed.WithLabelValues("append").Inc()
	m.Compactions.Inc()
	m.LoadDuration.Observe(0.05)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	seen := make(map[string]bool, len(families))
	for _, f := range families {
		seen[f.GetName()] = true
	}
	for _, name := range []string{
		"ycollab_connections_open",
		"ycollab_connections_total",
		"ycollab_connections_closed_total",
		"ycollab_rooms_resident",
		"ycollab_rooms_started_total",
		"ycollab_messages_received_total",
		"ycollab_frames_sent_total",
		"ycollab_frames_dropped_total",
		"ycollab_bytes_received_total",
		"ycollab_bytes_sent_total",
		"ycollab_apply_duration_seconds",
		"ycollab_apply_failed_total",
		"ycollab_update_bytes",
		"ycollab_denied_total",
		"ycollab_store_duration_seconds",
		"ycollab_store_failed_total",
		"ycollab_compactions_total",
		"ycollab_document_load_seconds",
	} {
		if !seen[name] {
			t.Errorf("%s is not registered", name)
		}
	}

	// And the close code becomes a label rather than part of the name.
	const want = `
# HELP ycollab_connections_closed_total Connections closed, by WebSocket close code.
# TYPE ycollab_connections_closed_total counter
ycollab_connections_closed_total{code="1008"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "ycollab_connections_closed_total"); err != nil {
		t.Error(err)
	}
}

// Nop is what every package falls back to, so counting into it must be safe -
// and must not register anything, since two of them in one process would
// otherwise be a duplicate-registration panic.
func TestNopCountsIntoNothing(t *testing.T) {
	a, b := metrics.Nop(), metrics.Nop()
	for _, m := range []*metrics.Metrics{a, b} {
		m.ConnectionsOpen.Inc()
		m.CloseCode(1001)
		m.MessagesReceived.WithLabelValues("awareness").Inc()
		m.ApplyDuration.Observe(0.5)
		m.StoreFailed.WithLabelValues("load").Inc()
	}

	// A registry that was never given anything gathers nothing, which is the
	// point: Nop is free and invisible.
	reg := prometheus.NewRegistry()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 0 {
		t.Fatalf("Nop registered %d metric families", len(families))
	}
}

// A nil *Metrics is not something the server creates, but Snapshot-style
// helpers are called from tests and tools, so the label helper must not be the
// one place that panics.
func TestCloseCodeLabelsAreStable(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	m.CloseCode(1000)
	m.CloseCode(1000)
	m.CloseCode(1011)

	if got := testutil.ToFloat64(m.ConnectionsClosed.WithLabelValues("1000")); got != 2 {
		t.Fatalf("code 1000 counted %v times, want 2", got)
	}
	if got := testutil.ToFloat64(m.ConnectionsClosed.WithLabelValues("1011")); got != 1 {
		t.Fatalf("code 1011 counted %v times, want 1", got)
	}
}

// fakeStats stands in for the room manager's counters.
type fakeStats struct{ values map[string]uint64 }

func (f fakeStats) Snapshot() map[string]uint64 { return f.values }

// The cluster counters are read at scrape time from the room's own atomics, so
// /metrics and /statsz cannot drift apart. That is worth a test, because the
// alternative - mirroring them into Prometheus counters - is the obvious
// implementation and the one that drifts.
func TestClusterCollectorReadsAtScrapeTime(t *testing.T) {
	stats := fakeStats{values: map[string]uint64{
		"published_update": 1,
		"self_filtered":    0,
	}}
	reg := prometheus.NewRegistry()
	reg.MustRegister(metrics.NewClusterCollector(stats))

	// Changing the underlying numbers must show up without re-registering.
	stats.values["published_update"] = 7
	stats.values["self_filtered"] = 7

	const want = `
# HELP ycollab_cluster_published_update_total Updates from local clients relayed to the other replicas.
# TYPE ycollab_cluster_published_update_total counter
ycollab_cluster_published_update_total 7
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "ycollab_cluster_published_update_total"); err != nil {
		t.Error(err)
	}

	// A counter that is zero is still exported: "no envelopes were dropped" is a
	// fact somebody wants to graph, and a missing series looks like a broken
	// scrape.
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 2 {
		t.Fatalf("gathered %d families, want 2", len(families))
	}
}
