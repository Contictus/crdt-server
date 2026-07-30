package room

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/mesutokul/ycollab/internal/cluster"
	"github.com/mesutokul/ycollab/internal/crdt"
	"github.com/mesutokul/ycollab/internal/protocol"
)

// A replica pairs a room with the connections attached to it, which is what a
// two-replica test needs to talk about.
type replica struct {
	t    *testing.T
	room *Room
	name string
}

// newCluster builds n replicas of one document on a shared in-process bus.
//
// The rooms are not started: like the rest of this package's tests, the actor is
// driven through handle, and the bus is driven through pump. Nothing sleeps and
// nothing races, so a failure is a real defect rather than a timing artefact.
func newCluster(t *testing.T, now *time.Time, n int, tweak func(*Config)) ([]*replica, *cluster.Memory) {
	t.Helper()
	bus := cluster.NewMemory()
	t.Cleanup(func() { _ = bus.Close() })

	replicas := make([]*replica, 0, n)
	for i := range n {
		cfg := Config{
			Name:        "doc",
			IdleTimeout: time.Hour,
			Bus:         bus,
			// Ids are fixed rather than random so a failure message is readable.
			NodeID:      uint64(i + 1),
			AntiEntropy: 15 * time.Second,
			Now:         func() time.Time { return *now },
			Logger:      quietLogger(),
		}
		if tweak != nil {
			tweak(&cfg)
		}
		r := New(cfg)
		if err := r.joinBus(context.Background()); err != nil {
			t.Fatalf("replica %d: join bus: %v", i, err)
		}
		t.Cleanup(func() {
			if r.sub != nil {
				_ = r.sub.Close()
			}
		})
		replicas = append(replicas, &replica{t: t, room: r, name: string(rune('A' + i))})
	}
	return replicas, bus
}

// join attaches a connection and completes the client handshake, leaving the
// connection with nothing but the frames the test cares about.
func (r *replica) join(id uint64) *fakeConn {
	r.t.Helper()
	c := &fakeConn{id: id}
	r.room.handle(joinCmd{c})
	r.room.handle(frameCmd{c, protocol.WriteSyncStep1(emptyStateVector(r.t))})
	c.take()
	return c
}

// pump runs the bus until it goes quiet: everything queued for publication is
// published, everything delivered is applied, and anything that produces makes
// more traffic goes round again.
//
// It reports how many envelopes were published, which is what the loop tests
// measure.
func pump(t *testing.T, replicas []*replica) int {
	t.Helper()
	published := 0
	for round := 0; ; round++ {
		if round == 100 {
			t.Fatal("the bus never went quiet: replicas are talking in a loop")
		}
		work := false
		for _, rep := range replicas {
			for {
				select {
				case job := <-rep.room.pub:
					if err := rep.room.cfg.Bus.Publish(context.Background(), rep.room.cfg.Name, job.env); err != nil {
						t.Fatalf("%s: publish: %v", rep.name, err)
					}
					job.counter.Add(1)
					published++
					work = true
					continue
				default:
				}
				break
			}
		}
		for _, rep := range replicas {
			for {
				select {
				case env := <-rep.room.remote:
					rep.room.remoteEnvelope(env)
					work = true
					continue
				default:
				}
				break
			}
		}
		if !work {
			return published
		}
	}
}

// print identifies a replica's document by content and state vector, the same
// way the fanout property test does.
func (r *replica) print() string {
	r.t.Helper()
	return docPrint(r.t, r.room.doc)
}

// canonical is the document those updates describe, built by applying them in
// the order Yjs produced them. Every replica has to end up here.
func canonical(t *testing.T, updates [][]byte) string {
	t.Helper()
	doc := crdt.NewDoc(1)
	for _, u := range updates {
		if err := doc.ApplyUpdate(u); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}
	return docPrint(t, doc)
}

// totals sums a counter across the replicas, which is how a cluster-wide claim
// like "each edit crossed the bus once" is stated.
func totals(reps []*replica) map[string]uint64 {
	out := make(map[string]uint64)
	for _, rep := range reps {
		for name, value := range rep.room.stats.Snapshot() {
			out[name] += value
		}
	}
	return out
}

// The criterion the brief sets for this phase: a client on one replica and a
// client on another edit the same document.
func TestAnUpdateReachesAClientOnAnotherReplica(t *testing.T) {
	now := time.Unix(1700000000, 0)
	reps, _ := newCluster(t, &now, 2, nil)
	a, b := reps[0], reps[1]
	ca, cb := a.join(1), b.join(2)

	update := readFixture(t, "text-insert-single", "update-000.bin")
	a.room.handle(frameCmd{ca, protocol.WriteUpdate(update)})
	pump(t, reps)

	if got, want := b.print(), canonical(t, [][]byte{update}); got != want {
		t.Fatalf("replica B has %s, want %s", got, want)
	}
	msgs := cb.decodeAll(t)
	if len(msgs) != 1 {
		t.Fatalf("the client on B got %d messages, want 1: %#v", len(msgs), msgs)
	}
	relayed, ok := msgs[0].(protocol.UpdateMessage)
	if !ok {
		t.Fatalf("got %T, want an Update", msgs[0])
	}
	// The author's bytes, not our re-encoding of them: a client on another
	// replica must receive exactly what a client on this one would.
	if !bytes.Equal(relayed.Update, update) {
		t.Fatalf("relayed bytes differ\n got %x\nwant %x", relayed.Update, update)
	}
	// And the author is not sent its own edit back through the cluster.
	if got := ca.frames(); len(got) != 0 {
		t.Fatalf("the author received %d frames back: %x", len(got), got)
	}
}

// Three replicas, one document, and traffic from three real Yjs clients spread
// across them. The fixture is the interleaved scenario, so the updates depend on
// each other and arrive at each replica in a different order.
func TestReplicasConvergeOnEveryScenario(t *testing.T) {
	for _, name := range scenarioNames(t) {
		updates := scenarioUpdates(t, name)
		if len(updates) < 2 {
			continue
		}
		t.Run(name, func(t *testing.T) {
			now := time.Unix(1700000000, 0)
			reps, _ := newCluster(t, &now, 3, nil)
			conns := make([]*fakeConn, len(reps))
			for i, rep := range reps {
				conns[i] = rep.join(uint64(i + 1))
			}

			// Round-robin, so each replica authors some updates and receives the
			// rest out of order.
			for i, update := range updates {
				j := i % len(reps)
				reps[j].room.handle(frameCmd{conns[j], protocol.WriteUpdate(update)})
			}
			pump(t, reps)

			want := canonical(t, updates)
			for _, rep := range reps {
				if got := rep.print(); got != want {
					t.Fatalf("replica %s\n got %s\nwant %s", rep.name, got, want)
				}
			}
		})
	}
}

// The loop test. Origin filtering is the only thing stopping an update from
// circulating forever between replicas, so the count of published updates has to
// be exactly the number of edits - not a multiple of it, and not growing.
func TestNoReplicaRepublishesWhatItReceived(t *testing.T) {
	now := time.Unix(1700000000, 0)
	reps, _ := newCluster(t, &now, 3, nil)
	conns := make([]*fakeConn, len(reps))
	for i, rep := range reps {
		conns[i] = rep.join(uint64(i + 1))
	}

	updates := scenarioUpdates(t, "text-three-client-interleaved")
	edits := uint64(len(updates))
	for i, update := range updates {
		j := i % len(reps)
		reps[j].room.handle(frameCmd{conns[j], protocol.WriteUpdate(update)})
	}
	if published := pump(t, reps); uint64(published) != edits {
		t.Fatalf("%d envelopes crossed the bus for %d edits", published, edits)
	}

	got := totals(reps)
	// One envelope per edit, whatever the cluster size: an update crosses the bus
	// once, from the replica whose client wrote it.
	for name, want := range map[string]uint64{
		"published_update":      edits,
		"published_diff":        0,
		"received":              edits * uint64(len(reps)),
		"self_filtered":         edits,
		"remote_update_applied": edits * uint64(len(reps)-1),
		"remote_rejected":       0,
		"remote_dropped":        0,
		"publish_dropped":       0,
	} {
		if got[name] != want {
			t.Fatalf("%s is %d, want %d", name, got[name], want)
		}
	}

	// And with the clients quiet, the cluster is silent. A loop would show up
	// here as traffic nobody asked for.
	if published := pump(t, reps); published != 0 {
		t.Fatalf("%d envelopes were published with nobody typing", published)
	}
}

// Anti-entropy exists because Redis Pub/Sub is at-most-once. This drives that
// case directly: an update is published while a replica is not listening, so it
// is simply gone, and the periodic state vector is the only thing that can find
// out.
func TestAntiEntropyRepairsALostUpdate(t *testing.T) {
	now := time.Unix(1700000000, 0)
	reps, _ := newCluster(t, &now, 2, nil)
	a, b := reps[0], reps[1]
	ca := a.join(1)
	cb := b.join(2)

	// B misses the message: its subscription is gone while A publishes.
	if err := b.room.sub.Close(); err != nil {
		t.Fatal(err)
	}
	b.room.sub = nil
	update := readFixture(t, "text-insert-single", "update-000.bin")
	a.room.handle(frameCmd{ca, protocol.WriteUpdate(update)})
	pump(t, reps)
	empty := docPrint(t, crdt.NewDoc(1))
	if got := b.print(); got != empty {
		t.Fatalf("replica B has %s; the test did not manage to lose the message", got)
	}
	cb.take()

	// B comes back and, on its next tick past the anti-entropy interval,
	// announces where it is.
	if err := b.room.joinBus(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(16 * time.Second)
	b.room.handle(tickCmd{now})
	pump(t, reps)

	if got, want := b.print(), canonical(t, [][]byte{update}); got != want {
		t.Fatalf("replica B after anti-entropy\n got %s\nwant %s", got, want)
	}
	if got := a.room.stats.AnsweredStateVector.Load(); got != 1 {
		t.Fatalf("answered_state_vector is %d, want 1", got)
	}
	if got := a.room.stats.PublishedDiff.Load(); got != 1 {
		t.Fatalf("published_diff is %d, want 1", got)
	}
	// The client on B hears about it too, rather than the repair stopping at the
	// server.
	msgs := cb.decodeAll(t)
	if len(msgs) != 1 {
		t.Fatalf("the client on B got %d messages, want 1: %#v", len(msgs), msgs)
	}
	if _, ok := msgs[0].(protocol.UpdateMessage); !ok {
		t.Fatalf("got %T, want an Update", msgs[0])
	}
}

// Announcing costs a message; answering an announcement from a replica that is
// already up to date must cost nothing, or every room would generate traffic
// proportional to the cluster size forever.
func TestAnnouncingWhenNobodyIsBehindProducesNoDiff(t *testing.T) {
	now := time.Unix(1700000000, 0)
	reps, _ := newCluster(t, &now, 2, nil)
	a, b := reps[0], reps[1]
	ca := a.join(1)
	b.join(2)

	a.room.handle(frameCmd{ca, protocol.WriteUpdate(readFixture(t, "text-insert-single", "update-000.bin"))})
	pump(t, reps)

	now = now.Add(16 * time.Second)
	a.room.handle(tickCmd{now})
	b.room.handle(tickCmd{now})
	if published := pump(t, reps); published != 2 {
		t.Fatalf("%d envelopes for two announcements, want 2", published)
	}
	for _, rep := range reps {
		if got := rep.room.stats.PublishedDiff.Load(); got != 0 {
			t.Fatalf("%s published %d diffs while in sync", rep.name, got)
		}
	}
}

// An empty room has nobody to keep in sync, and a cluster where every replica
// announces every document it has ever held only gets slower.
func TestAnEmptyRoomDoesNotAnnounce(t *testing.T) {
	now := time.Unix(1700000000, 0)
	reps, _ := newCluster(t, &now, 2, nil)
	now = now.Add(16 * time.Second)
	if published := pump(t, reps); published != 0 {
		t.Fatalf("an empty room published %d envelopes", published)
	}
	reps[0].room.handle(tickCmd{now})
	if published := pump(t, reps); published != 0 {
		t.Fatalf("an empty room announced: %d envelopes", published)
	}
}

// Cursors are the visible half of the acceptance criterion, and they have to
// cross replicas the same way updates do.
func TestAwarenessCrossesTheCluster(t *testing.T) {
	now := time.Unix(1700000000, 0)
	reps, _ := newCluster(t, &now, 2, nil)
	a, b := reps[0], reps[1]
	ca := a.join(1)
	cb := b.join(2)

	a.room.handle(frameCmd{ca, protocol.WriteAwareness(singleEntry(1001, 1, `{"user":{"name":"one"}}`))})
	pump(t, reps)

	if got := b.room.aw.Len(); got != 1 {
		t.Fatalf("replica B knows %d cursors, want 1", got)
	}
	msgs := cb.decodeAll(t)
	if len(msgs) != 1 {
		t.Fatalf("the client on B got %d messages, want 1: %#v", len(msgs), msgs)
	}
	if _, ok := msgs[0].(protocol.AwarenessMessage); !ok {
		t.Fatalf("got %T, want Awareness", msgs[0])
	}

	// When the connection goes, the cursor goes everywhere, not just where it was
	// connected.
	a.room.handle(leaveCmd{ca})
	pump(t, reps)
	if got := b.room.aw.Len(); got != 0 {
		t.Fatalf("replica B still shows %d cursors after the client left", got)
	}
}

// A timeout is a local conclusion from local silence. Publishing it would let a
// replica with a slow bus connection delete cursors that are alive elsewhere.
func TestAnAwarenessTimeoutIsNotPublished(t *testing.T) {
	now := time.Unix(1700000000, 0)
	reps, _ := newCluster(t, &now, 2, nil)
	a, b := reps[0], reps[1]
	ca := a.join(1)
	b.join(2)

	a.room.handle(frameCmd{ca, protocol.WriteAwareness(singleEntry(1001, 1, `{"user":{"name":"one"}}`))})
	pump(t, reps)
	if got := b.room.aw.Len(); got != 1 {
		t.Fatalf("replica B knows %d cursors, want 1", got)
	}

	// Past the timeout, with no tick on B so only A sweeps.
	now = now.Add(31 * time.Second)
	a.room.handle(tickCmd{now})
	published := pump(t, reps)
	if got := a.room.aw.Len(); got != 0 {
		t.Fatalf("replica A did not sweep: %d cursors left", got)
	}
	// The only thing A may have published is its state vector.
	if published > 1 {
		t.Fatalf("a sweep published %d envelopes", published)
	}
	if got := a.room.stats.PublishedAwareness.Load(); got != 1 {
		t.Fatalf("published_awareness is %d, want 1 - the client's own update and nothing else", got)
	}
}

// Every update is written by the replica whose client authored it. Writing it on
// both would put two copies of the same bytes in the log for no gain.
func TestRemoteUpdatesAreNotPersisted(t *testing.T) {
	now := time.Unix(1700000000, 0)
	stores := make([]*fakeStore, 2)
	i := 0
	reps, _ := newCluster(t, &now, 2, func(cfg *Config) {
		stores[i] = &fakeStore{}
		cfg.Store = stores[i]
		i++
	})
	a, b := reps[0], reps[1]
	ca := a.join(1)
	b.join(2)

	// The rooms are driven by hand here, so their persist goroutines were never
	// started; record's queue is what the room would have handed over.
	update := readFixture(t, "text-insert-single", "update-000.bin")
	a.room.handle(frameCmd{ca, protocol.WriteUpdate(update)})
	pump(t, reps)

	if got := len(a.room.jobs); got != 1 {
		t.Fatalf("the authoring replica queued %d writes, want 1", got)
	}
	if got := len(b.room.jobs); got != 0 {
		t.Fatalf("the receiving replica queued %d writes, want 0", got)
	}
	if got, want := b.print(), canonical(t, [][]byte{update}); got != want {
		t.Fatalf("replica B\n got %s\nwant %s; it should still have applied the update", got, want)
	}
}

// emptyUpdate is a claim about the encoding, so it is worth pinning: if a diff
// for a peer that is not missing anything ever stopped encoding to these bytes,
// every anti-entropy round would publish a useless update.
func TestEmptyDiffIsRecognised(t *testing.T) {
	doc := crdt.NewDoc(1)
	if err := doc.ApplyUpdate(readFixture(t, "text-insert-single", "update-000.bin")); err != nil {
		t.Fatal(err)
	}
	sv, err := doc.EncodeStateVector()
	if err != nil {
		t.Fatal(err)
	}
	diff, err := doc.EncodeDiff(sv)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(diff, emptyUpdate) {
		t.Fatalf("a diff against our own state vector is %x, want %x", diff, emptyUpdate)
	}
	if !isEmptyUpdate(diff) {
		t.Fatal("isEmptyUpdate did not recognise it")
	}

	// And a diff that does carry something must not be mistaken for empty.
	behind, err := crdt.NewDoc(2).EncodeStateVector()
	if err != nil {
		t.Fatal(err)
	}
	real, err := doc.EncodeDiff(behind)
	if err != nil {
		t.Fatal(err)
	}
	if isEmptyUpdate(real) {
		t.Fatalf("a real diff %x was taken for empty", real)
	}
}

// A room that cannot reach the bus must refuse rather than serve a document that
// silently ignores the other replicas.
func TestARoomThatCannotJoinTheBusRefusesToServe(t *testing.T) {
	bus := cluster.NewMemory()
	if err := bus.Close(); err != nil {
		t.Fatal(err)
	}
	r := New(Config{Name: "doc", Bus: bus, NodeID: 1, Logger: quietLogger()})
	c := &fakeConn{id: 1}
	if err := r.Join(c); err != nil {
		t.Fatalf("join: %v", err)
	}

	r.Run(context.Background())

	closed, code := c.status()
	if !closed {
		t.Fatal("the connection was left open")
	}
	if code != CloseInternalError {
		t.Fatalf("closed with %d, want %d", code, CloseInternalError)
	}
}
