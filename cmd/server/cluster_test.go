package main_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/mesutokul/ycollab/internal/crdt"
	"github.com/mesutokul/ycollab/internal/crdt/lib0"
	"github.com/mesutokul/ycollab/internal/protocol"
)

// The Phase 4 acceptance criterion, against real processes and a real Redis:
// two clients on different replicas edit the same document and stay in sync,
// and a counter shows that origin filtering keeps update loops at zero.
//
// These start two copies of the actual binary rather than two managers in one
// process, because the thing being tested is what crosses the network between
// them.
//
//	docker compose -f deploy/docker-compose.yml up -d
//	YCOLLAB_TEST_REDIS_URL=redis://127.0.0.1:6380 go test ./cmd/server/
const redisEnv = "YCOLLAB_TEST_REDIS_URL"

// statsz is what /statsz returns.
type statsz struct {
	NodeID  uint64            `json:"node_id"`
	Rooms   int               `json:"rooms"`
	Cluster map[string]uint64 `json:"cluster"`
}

func (s *server) stats(t *testing.T) statsz {
	t.Helper()
	resp, err := http.Get("http://" + s.admin + "/statsz")
	if err != nil {
		t.Fatalf("statsz: %v", err)
	}
	defer resp.Body.Close()
	var out statsz
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode statsz: %v", err)
	}
	return out
}

// clusterTotals sums a counter across the replicas.
func clusterTotals(t *testing.T, nodes ...*server) map[string]uint64 {
	t.Helper()
	out := make(map[string]uint64)
	for _, n := range nodes {
		for name, value := range n.stats(t).Cluster {
			out[name] += value
		}
	}
	return out
}

// recvUpdate waits for the next update the server pushes, ignoring the awareness
// traffic a real session is full of.
func (c *client) recvUpdate(timeout time.Duration) ([]byte, error) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			return nil, err
		}
		msg, err := protocol.Decode(data)
		if err != nil {
			return nil, err
		}
		switch v := msg.(type) {
		case protocol.UpdateMessage:
			return v.Update, nil
		case protocol.SyncStep2Message:
			return v.Update, nil
		}
	}
}

// startCluster starts n replicas sharing one Redis, and returns them.
func startCluster(t *testing.T, n int, extra ...string) []*server {
	t.Helper()
	redisURL := os.Getenv(redisEnv)
	if redisURL == "" {
		t.Skipf("%s is not set; start deploy/docker-compose.yml to run this", redisEnv)
	}
	binary := buildServer(t)
	// A prefix per test run, so one test's channels are not another's.
	prefix := fmt.Sprintf("ycollab-test-%d", time.Now().UnixNano())

	nodes := make([]*server, 0, n)
	for range n {
		args := append([]string{
			"-redis-url", redisURL,
			"-redis-prefix", prefix,
		}, extra...)
		// No database: this test is about the bus, and a shared Postgres would let
		// a replica catch up through storage and hide a broken fanout.
		nodes = append(nodes, startServer(t, binary, freePort(t), "", args...))
	}
	return nodes
}

// The criterion itself.
func TestClientsOnDifferentReplicasStayInSync(t *testing.T) {
	// A short anti-entropy interval, so the announcements are frequent enough
	// during the test to prove they do not turn into update traffic.
	nodes := startCluster(t, 3, "-anti-entropy", "300ms", "-tick", "100ms")
	doc := fmt.Sprintf("cluster-%d", time.Now().UnixNano())

	clients := make([]*client, len(nodes))
	for i, node := range nodes {
		clients[i] = dial(t, node.addr, doc)
		clients[i].sync()
	}

	// Each client authors one update, and every other client must receive exactly
	// those bytes.
	updates := make([][]byte, len(clients))
	for i := range clients {
		updates[i] = readFixture(t, "text-three-client-interleaved", fmt.Sprintf("update-%03d.bin", i))
	}
	for i, c := range clients {
		c.send(protocol.WriteUpdate(updates[i]))
		for j, other := range clients {
			if j == i {
				continue
			}
			got, err := other.recvUpdate(15 * time.Second)
			if err != nil {
				t.Fatalf("client %d never received client %d's update: %v\n%s", j, i, err, nodes[j].logs)
			}
			if !bytes.Equal(got, updates[i]) {
				t.Fatalf("client %d received\n got %x\nwant %x", j, got, updates[i])
			}
		}
	}

	// Every replica ends up with the same document, which is what the clients
	// would see if they reconnected.
	want := crdt.NewDoc(9)
	for _, u := range updates {
		if err := want.ApplyUpdate(u); err != nil {
			t.Fatal(err)
		}
	}
	wantSV, err := want.EncodeStateVector()
	if err != nil {
		t.Fatal(err)
	}
	for i, node := range nodes {
		got := dial(t, node.addr, doc).sync()
		gotSV, err := got.EncodeStateVector()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gotSV, wantSV) {
			t.Fatalf("replica %d state vector\n got %x\nwant %x", i, gotSV, wantSV)
		}
		if got, want := textOf(t, got), textOf(t, want); got != want {
			t.Fatalf("replica %d\n got %q\nwant %q", i, got, want)
		}
	}

	// The loop counter. One envelope per edit, whatever the cluster size, and
	// every replica filtered out exactly the envelopes it had published itself.
	edits := uint64(len(updates))
	totals := clusterTotals(t, nodes...)
	if got := totals["published_update"]; got != edits {
		t.Fatalf("published_update is %d for %d edits\n%s", got, edits, nodes[0].logs)
	}
	if got := totals["self_filtered"]; got != edits {
		t.Fatalf("self_filtered is %d, want %d", got, edits)
	}
	if got, want := totals["remote_update_applied"], edits*uint64(len(nodes)-1); got != want {
		t.Fatalf("remote_update_applied is %d, want %d", got, want)
	}
	if got := totals["remote_rejected"]; got != 0 {
		t.Fatalf("remote_rejected is %d, want 0", got)
	}

	// And with the clients quiet, update traffic stops. Anti-entropy keeps
	// announcing - that is its job - but an update that circulated would show up
	// here as growth nobody asked for.
	before := clusterTotals(t, nodes...)
	time.Sleep(2 * time.Second)
	after := clusterTotals(t, nodes...)
	for _, name := range []string{"published_update", "published_diff"} {
		if after[name] != before[name] {
			t.Fatalf("%s grew from %d to %d with nobody typing", name, before[name], after[name])
		}
	}
	if after["published_state_vector"] <= before["published_state_vector"] {
		t.Fatalf("anti-entropy stopped announcing: %d then %d",
			before["published_state_vector"], after["published_state_vector"])
	}
}

// Redis Pub/Sub is at-most-once, and a replica that was not subscribed when a
// message went out has no way to learn of it from the bus. This drives that case
// the way production produces it: the second replica has no room for the
// document at all while the first one is edited, so it starts from nothing and
// has to catch up.
//
// There is no database here, so anti-entropy is the only mechanism that can do
// it.
func TestAReplicaCatchesUpByAntiEntropy(t *testing.T) {
	nodes := startCluster(t, 2, "-anti-entropy", "300ms", "-tick", "100ms")
	doc := fmt.Sprintf("catchup-%d", time.Now().UnixNano())

	first := dial(t, nodes[0].addr, doc)
	first.sync()
	update := readFixture(t, "text-insert-single", "update-000.bin")
	first.send(protocol.WriteUpdate(update))

	// The second replica has never heard of this document. Its room is created by
	// this connection, after the edit was published and lost.
	second := dial(t, nodes[1].addr, doc)
	if got := second.sync(); textOf(t, got) != "" {
		t.Skipf("the second replica already had the document (%q); the race could not be arranged", textOf(t, got))
	}

	got, err := second.recvUpdate(20 * time.Second)
	if err != nil {
		t.Fatalf("the second replica never caught up: %v\n%s", err, nodes[1].logs)
	}
	want := crdt.NewDoc(9)
	if err := want.ApplyUpdate(update); err != nil {
		t.Fatal(err)
	}
	repaired := crdt.NewDoc(1)
	if err := repaired.ApplyUpdate(got); err != nil {
		t.Fatalf("apply the repair: %v", err)
	}
	if textOf(t, repaired) != textOf(t, want) {
		t.Fatalf("the repair carried %q, want %q", textOf(t, repaired), textOf(t, want))
	}
	if n := clusterTotals(t, nodes...)["answered_state_vector"]; n == 0 {
		t.Fatal("nothing answered a state vector, so the document arrived some other way")
	}
}

// Awareness is the visible half: a cursor on one replica has to appear on the
// other, because that is what the acceptance criterion means by live cursors.
func TestCursorsCrossReplicas(t *testing.T) {
	nodes := startCluster(t, 2)
	doc := fmt.Sprintf("cursors-%d", time.Now().UnixNano())

	a := dial(t, nodes[0].addr, doc)
	a.sync()
	b := dial(t, nodes[1].addr, doc)
	b.sync()

	payload := awarenessEntry(t, 4242, 1, `{"user":{"name":"ada","color":"#ff0000"}}`)
	a.send(protocol.WriteAwareness(payload))

	aw := protocol.NewAwareness()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for {
		_, data, err := b.ws.Read(ctx)
		if err != nil {
			t.Fatalf("the cursor never arrived: %v\n%s", err, nodes[1].logs)
		}
		msg, err := protocol.Decode(data)
		if err != nil {
			t.Fatal(err)
		}
		update, ok := msg.(protocol.AwarenessMessage)
		if !ok {
			continue
		}
		if _, err := aw.ApplyUpdate(update.Payload, time.Now()); err != nil {
			t.Fatalf("apply awareness: %v", err)
		}
		state, present := aw.State(4242)
		if !present {
			continue
		}
		if state != `{"user":{"name":"ada","color":"#ff0000"}}` {
			t.Fatalf("the cursor arrived as %s", state)
		}
		return
	}
}

// awarenessEntry builds a one-client awareness update the way a client would
// (awareness.js:194).
func awarenessEntry(_ *testing.T, client, clock uint64, state string) []byte {
	e := lib0.NewEncoder()
	e.WriteVarUint(1)
	e.WriteVarUint(client)
	e.WriteVarUint(clock)
	e.WriteVarString(state)
	return e.Bytes()
}
