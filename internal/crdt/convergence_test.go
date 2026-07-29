package crdt_test

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	"github.com/mesutokul/ycollab/internal/crdt"
)

// propertyIterations is the number of random cases each property runs. The
// brief asks for at least a thousand; the scenarios are small enough that this
// still finishes in well under a second.
const propertyIterations = 1000

// scenarioUpdates returns a scenario's incremental updates in the order Yjs
// produced them.
func scenarioUpdates(t *testing.T, dir string) [][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if n := e.Name(); len(n) > 7 && n[:7] == "update-" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	updates := make([][]byte, 0, len(names))
	for _, n := range names {
		updates = append(updates, readFixture(t, filepath.Join(dir, n)))
	}
	return updates
}

// fingerprint identifies a document by what it means, not by how it is
// serialised.
//
// Byte equality is too strong here: the same document can be represented with
// different struct boundaries depending on the order updates arrived in, since
// an update that arrives whole is integrated whole while the same content
// arriving piecemeal stays split. Yjs behaves the same way. What must match is
// the content, the state vector and the delete set.
func fingerprint(t *testing.T, doc *crdt.Doc) string {
	t.Helper()
	roots := make(map[string]any)
	for _, name := range doc.Roots() {
		t := doc.Get(name)
		// Render each root both ways rather than guessing its kind. The text
		// rendering is what makes this split-insensitive: "wxyz" and "w","x",
		// "y","z" are the same text but different sequence elements, and which
		// one a document ends up with depends only on how the updates were
		// batched.
		entries := make(map[string]any)
		for _, key := range crdt.AsMap(t).Keys() {
			if v, ok := crdt.AsMap(t).Get(key); ok {
				entries[key] = jsonSafe(v)
			}
		}
		roots[name] = map[string]any{
			"text": crdt.AsText(t).String(),
			"map":  entries,
		}
	}
	sv := make(map[string]uint64)
	for client, clock := range doc.StateVector() {
		sv[strconv.FormatUint(uint64(client), 10)] = uint64(clock)
	}
	ds := make(map[string][][2]uint64)
	for client, ranges := range doc.DeleteSet().Clients {
		out := make([][2]uint64, len(ranges))
		for i, r := range ranges {
			out[i] = [2]uint64{uint64(r.Clock), uint64(r.Len)}
		}
		ds[strconv.FormatUint(uint64(client), 10)] = out
	}
	b, err := json.Marshal(map[string]any{"roots": roots, "sv": sv, "ds": ds})
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	return string(b)
}

// canonicalDoc applies updates in the order Yjs produced them.
func canonicalDoc(t *testing.T, updates [][]byte) *crdt.Doc {
	t.Helper()
	doc := crdt.NewDoc(1)
	for _, u := range updates {
		if err := doc.ApplyUpdate(u); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}
	return doc
}

// Property: the order updates arrive in does not affect the document.
//
// This is the whole point of a CRDT, and the one thing a wire-compatible engine
// cannot get subtly wrong without diverging from clients in production.
func TestPropertyOrderDoesNotMatter(t *testing.T) {
	rng := rand.New(rand.NewSource(20260729))
	for _, dir := range scenarioDirs(t) {
		name := filepath.Base(dir)
		updates := scenarioUpdates(t, dir)
		if len(updates) < 2 {
			continue
		}
		want := fingerprint(t, canonicalDoc(t, updates))

		for i := range propertyIterations {
			perm := rng.Perm(len(updates))
			doc := crdt.NewDoc(1)
			for _, j := range perm {
				if err := doc.ApplyUpdate(updates[j]); err != nil {
					t.Fatalf("%s: iteration %d: apply: %v", name, i, err)
				}
			}
			if n := doc.PendingCount(); n != 0 {
				t.Fatalf("%s: iteration %d: %d updates still pending for order %v", name, i, n, perm)
			}
			if got := fingerprint(t, doc); got != want {
				t.Fatalf("%s: order %v diverged\n got %s\nwant %s", name, perm, got, want)
			}
		}
	}
}

// Property: delivering an update more than once changes nothing. Redis fanout
// and websocket reconnects both make duplicates routine.
func TestPropertyDuplicatesAreHarmless(t *testing.T) {
	rng := rand.New(rand.NewSource(6180339))
	for _, dir := range scenarioDirs(t) {
		name := filepath.Base(dir)
		updates := scenarioUpdates(t, dir)
		if len(updates) == 0 {
			continue
		}
		want := fingerprint(t, canonicalDoc(t, updates))

		for i := range propertyIterations {
			doc := crdt.NewDoc(1)
			for _, u := range updates {
				repeats := 1 + rng.Intn(3)
				for range repeats {
					if err := doc.ApplyUpdate(u); err != nil {
						t.Fatalf("%s: iteration %d: apply: %v", name, i, err)
					}
				}
			}
			if got := fingerprint(t, doc); got != want {
				t.Fatalf("%s: iteration %d: duplicate delivery diverged\n got %s\nwant %s", name, i, got, want)
			}
		}
	}
}

// Property: two replicas that saw disjoint halves of the traffic converge once
// they exchange diffs against each other's state vectors. This is the sync
// handshake Phase 2 implements, run against random splits.
func TestPropertyReplicasConvergeAfterExchange(t *testing.T) {
	rng := rand.New(rand.NewSource(2718281))
	for _, dir := range scenarioDirs(t) {
		name := filepath.Base(dir)
		updates := scenarioUpdates(t, dir)
		if len(updates) < 2 {
			continue
		}
		want := fingerprint(t, canonicalDoc(t, updates))

		for i := range propertyIterations {
			a := crdt.NewDoc(1)
			b := crdt.NewDoc(2)
			// Recorded so a failure names the delivery pattern that caused it.
			assignment := make([]int, 0, len(updates))
			for _, u := range updates {
				// Every update goes to at least one replica, and sometimes both.
				choice := rng.Intn(3)
				assignment = append(assignment, choice)
				switch choice {
				case 0:
					if err := a.ApplyUpdate(u); err != nil {
						t.Fatalf("%s: apply to a: %v", name, err)
					}
				case 1:
					if err := b.ApplyUpdate(u); err != nil {
						t.Fatalf("%s: apply to b: %v", name, err)
					}
				default:
					if err := a.ApplyUpdate(u); err != nil {
						t.Fatalf("%s: apply to a: %v", name, err)
					}
					if err := b.ApplyUpdate(u); err != nil {
						t.Fatalf("%s: apply to b: %v", name, err)
					}
				}
			}

			// Exchange until nothing changes. More than two rounds are
			// sometimes needed: a delete for structs a replica has not received
			// yet is held pending and is therefore invisible to its peers until
			// those structs arrive, so the deletion propagates one round behind
			// the structs it refers to. Yjs behaves the same way (pendingDs).
			// Phase 4's anti-entropy has to keep running for this reason - one
			// exchange is not a guarantee of convergence.
			const maxRounds = 6
			for range maxRounds {
				if fingerprint(t, a) == fingerprint(t, b) {
					break
				}
				svA, err := a.EncodeStateVector()
				if err != nil {
					t.Fatalf("%s: encode sv: %v", name, err)
				}
				svB, err := b.EncodeStateVector()
				if err != nil {
					t.Fatalf("%s: encode sv: %v", name, err)
				}
				forA, err := b.EncodeDiff(svA)
				if err != nil {
					t.Fatalf("%s: diff for a: %v", name, err)
				}
				forB, err := a.EncodeDiff(svB)
				if err != nil {
					t.Fatalf("%s: diff for b: %v", name, err)
				}
				if err := a.ApplyUpdate(forA); err != nil {
					t.Fatalf("%s: apply diff to a: %v", name, err)
				}
				if err := b.ApplyUpdate(forB); err != nil {
					t.Fatalf("%s: apply diff to b: %v", name, err)
				}
			}

			gotA := fingerprint(t, a)
			gotB := fingerprint(t, b)
			if gotA != gotB {
				t.Fatalf("%s: iteration %d: replicas diverged with delivery %v\n a %s\n b %s", name, i, assignment, gotA, gotB)
			}
			if gotA != want {
				t.Fatalf("%s: iteration %d: replicas agree but differ from the reference\n got %s\nwant %s", name, i, gotA, want)
			}
		}
	}
}

// Property: a document rebuilt from its own serialised state is the same
// document, no matter which random subset of updates built it. This is what
// makes snapshot compaction safe.
func TestPropertySnapshotRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1414213))
	for _, dir := range scenarioDirs(t) {
		name := filepath.Base(dir)
		updates := scenarioUpdates(t, dir)
		if len(updates) == 0 {
			continue
		}
		for i := range propertyIterations {
			doc := crdt.NewDoc(1)
			for _, j := range rng.Perm(len(updates)) {
				if err := doc.ApplyUpdate(updates[j]); err != nil {
					t.Fatalf("%s: apply: %v", name, err)
				}
			}
			snapshot, err := doc.EncodeStateAsUpdate(nil)
			if err != nil {
				t.Fatalf("%s: encode: %v", name, err)
			}
			restored := crdt.NewDoc(2)
			if err := restored.ApplyUpdate(snapshot); err != nil {
				t.Fatalf("%s: restore: %v", name, err)
			}
			again, err := restored.EncodeStateAsUpdate(nil)
			if err != nil {
				t.Fatalf("%s: re-encode: %v", name, err)
			}
			if !bytes.Equal(snapshot, again) {
				t.Fatalf("%s: iteration %d: snapshot round trip changed the document", name, i)
			}
		}
	}
}
