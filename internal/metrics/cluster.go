package metrics

import (
	"sort"

	"github.com/prometheus/client_golang/prometheus"
)

// Snapshotter is anything that can report a set of counters by name. It exists
// so this package does not import internal/room, which imports this one.
type Snapshotter interface {
	Snapshot() map[string]uint64
}

// clusterCollector exposes the cluster counters as Prometheus metrics.
//
// They are read at scrape time rather than mirrored into a second set of
// counters. The room already maintains them as atomics on its hot path, and
// incrementing a Prometheus counter alongside each one would be two sources of
// truth for the same fact - the kind of duplication that ends with a dashboard
// and a JSON endpoint disagreeing during an incident.
type clusterCollector struct {
	stats Snapshotter
	descs map[string]*prometheus.Desc
	names []string
}

// clusterHelp documents each counter. A name with no entry here is still
// exported, with a generic description: a new counter appearing in a build
// nobody updated should show up on the dashboard rather than vanish.
var clusterHelp = map[string]string{
	"published_update":         "Updates from local clients relayed to the other replicas.",
	"published_diff":           "Updates published in answer to another replica's state vector.",
	"published_awareness":      "Awareness updates relayed to the other replicas.",
	"published_state_vector":   "Anti-entropy announcements published.",
	"publish_failed":           "Envelopes the bus refused.",
	"publish_dropped":          "Envelopes dropped because a room's publish queue was full.",
	"received":                 "Envelopes delivered by the bus, including our own.",
	"self_filtered":            "Envelopes we published and received back, then dropped. This is loop prevention working.",
	"remote_dropped":           "Envelopes dropped because a room's inbound queue was full.",
	"remote_update_applied":    "Updates taken from other replicas.",
	"remote_awareness_applied": "Awareness updates taken from other replicas.",
	"remote_rejected":          "Envelopes from other replicas that would not apply.",
	"answered_state_vector":    "Anti-entropy announcements answered with a diff, i.e. losses repaired.",
}

// NewClusterCollector returns a collector for the room manager's counters.
func NewClusterCollector(stats Snapshotter) prometheus.Collector {
	c := &clusterCollector{stats: stats, descs: make(map[string]*prometheus.Desc)}
	for name := range stats.Snapshot() {
		c.names = append(c.names, name)
	}
	// Sorted, so Describe and Collect are deterministic and a diff of two
	// scrapes is readable.
	sort.Strings(c.names)
	for _, name := range c.names {
		help, ok := clusterHelp[name]
		if !ok {
			help = "Cluster counter " + name + "."
		}
		c.descs[name] = prometheus.NewDesc("ycollab_cluster_"+name+"_total", help, nil, nil)
	}
	return c
}

func (c *clusterCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, name := range c.names {
		ch <- c.descs[name]
	}
}

func (c *clusterCollector) Collect(ch chan<- prometheus.Metric) {
	snapshot := c.stats.Snapshot()
	for _, name := range c.names {
		ch <- prometheus.MustNewConstMetric(c.descs[name], prometheus.CounterValue, float64(snapshot[name]))
	}
}
