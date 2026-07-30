package room

import "sync/atomic"

// Stats counts what crosses the cluster bus. One instance is shared by every
// room on a node, which is what makes it useful: the numbers a node exposes are
// about the node, not about whichever document happened to be busy.
//
// These are counters rather than gauges, and the interesting facts are in their
// differences over time. In particular the brief's Phase 4 acceptance criterion
// - that origin filtering keeps update loops at zero - is checked by watching
// PublishedUpdate and PublishedDiff stop growing once the clients go quiet. A
// loop would show up as unbounded growth with nobody typing.
//
// Prometheus is Phase 6. Until then these are exposed as JSON on /statsz.
type Stats struct {
	// PublishedUpdate counts updates a local client authored and we relayed to
	// the other replicas.
	PublishedUpdate atomic.Uint64
	// PublishedDiff counts updates we published in answer to another replica's
	// state vector.
	PublishedDiff atomic.Uint64
	// PublishedAwareness counts awareness updates relayed from local clients.
	PublishedAwareness atomic.Uint64
	// PublishedStateVector counts our periodic anti-entropy announcements.
	PublishedStateVector atomic.Uint64
	// PublishFailed counts envelopes the bus refused.
	PublishFailed atomic.Uint64
	// PublishDropped counts envelopes dropped because the room's publish queue
	// was full. Anti-entropy repairs the loss; the counter is here so a node
	// under that much pressure is not silent about it.
	PublishDropped atomic.Uint64

	// Received counts envelopes delivered to a room by the bus, including our
	// own.
	Received atomic.Uint64
	// SelfFiltered counts envelopes we published and then received back. Pub/Sub
	// delivers to the publisher too, so this grows in normal operation - it is
	// the loop prevention working, not a fault.
	SelfFiltered atomic.Uint64
	// RemoteDropped counts envelopes dropped because a room's inbound queue was
	// full.
	RemoteDropped atomic.Uint64
	// RemoteUpdateApplied and RemoteAwarenessApplied count what we took from
	// other replicas.
	RemoteUpdateApplied    atomic.Uint64
	RemoteAwarenessApplied atomic.Uint64
	// RemoteRejected counts envelopes that would not apply. Unlike a bad frame
	// from a client, there is nothing to disconnect, so these are counted and
	// logged.
	RemoteRejected atomic.Uint64
	// AnsweredStateVector counts anti-entropy announcements we answered with a
	// diff, which is the same thing as the number of times a message loss or a
	// slow replica was actually repaired.
	AnsweredStateVector atomic.Uint64
}

// Snapshot returns the counters as plain numbers, for /statsz and for tests.
func (s *Stats) Snapshot() map[string]uint64 {
	if s == nil {
		return map[string]uint64{}
	}
	return map[string]uint64{
		"published_update":         s.PublishedUpdate.Load(),
		"published_diff":           s.PublishedDiff.Load(),
		"published_awareness":      s.PublishedAwareness.Load(),
		"published_state_vector":   s.PublishedStateVector.Load(),
		"publish_failed":           s.PublishFailed.Load(),
		"publish_dropped":          s.PublishDropped.Load(),
		"received":                 s.Received.Load(),
		"self_filtered":            s.SelfFiltered.Load(),
		"remote_dropped":           s.RemoteDropped.Load(),
		"remote_update_applied":    s.RemoteUpdateApplied.Load(),
		"remote_awareness_applied": s.RemoteAwarenessApplied.Load(),
		"remote_rejected":          s.RemoteRejected.Load(),
		"answered_state_vector":    s.AnsweredStateVector.Load(),
	}
}
