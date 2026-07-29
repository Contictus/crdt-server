package crdt_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"

	"github.com/mesutokul/ycollab/internal/crdt"
)

// propertyIterations is the number of random cases each property runs. The
// brief asks for at least a thousand; the scenarios are small enough that this
// still finishes in a second or two.
const propertyIterations = 1000

// propertySeed fixes the generator seed, so a failure reported by CI can be
// reproduced exactly. gopter also shrinks the failing case before reporting it,
// which is the reason these are written with generators rather than loops: the
// counterexample you get is the smallest one, not the first one.
const propertySeed = 20260729

func propertyParameters() *gopter.TestParameters {
	params := gopter.DefaultTestParametersWithSeed(propertySeed)
	params.MinSuccessfulTests = propertyIterations
	return params
}

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

// genPermutation produces permutations of 0..n-1.
//
// gopter has no permutation generator, so this sorts n random keys and takes
// the resulting index order. Shrinking the keys shrinks towards the identity
// order, which is the case that is easiest to read in a failure report.
func genPermutation(n int) gopter.Gen {
	return gen.SliceOfN(n, gen.Int()).Map(func(keys []int) []int {
		perm := make([]int, n)
		for i := range perm {
			perm[i] = i
		}
		sort.SliceStable(perm, func(i, j int) bool { return keys[perm[i]] < keys[perm[j]] })
		return perm
	})
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
		typ := doc.Get(name)
		// Render each root both ways rather than guessing its kind. The text
		// rendering is what makes this split-insensitive: "wxyz" and "w","x",
		// "y","z" are the same text but different sequence elements, and which
		// one a document ends up with depends only on how the updates were
		// batched.
		entries := make(map[string]any)
		for _, key := range crdt.AsMap(typ).Keys() {
			if v, ok := crdt.AsMap(typ).Get(key); ok {
				entries[key] = jsonSafe(v)
			}
		}
		roots[name] = map[string]any{
			"text": crdt.AsText(typ).String(),
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

// eachScenario runs f for every fixture scenario with at least min updates.
func eachScenario(t *testing.T, min int, f func(t *testing.T, name string, updates [][]byte, want string)) {
	t.Helper()
	for _, dir := range scenarioDirs(t) {
		name := filepath.Base(dir)
		updates := scenarioUpdates(t, dir)
		if len(updates) < min {
			continue
		}
		t.Run(name, func(t *testing.T) {
			f(t, name, updates, fingerprint(t, canonicalDoc(t, updates)))
		})
	}
}

// Property: the order updates arrive in does not affect the document.
//
// This is the whole point of a CRDT, and the one thing a wire-compatible engine
// cannot get subtly wrong without diverging from clients in production.
func TestPropertyOrderDoesNotMatter(t *testing.T) {
	eachScenario(t, 2, func(t *testing.T, name string, updates [][]byte, want string) {
		properties := gopter.NewProperties(propertyParameters())
		properties.Property("any delivery order reaches the same document", prop.ForAll(
			func(perm []int) (bool, error) {
				doc := crdt.NewDoc(1)
				for _, j := range perm {
					if err := doc.ApplyUpdate(updates[j]); err != nil {
						return false, err
					}
				}
				if n := doc.PendingCount(); n != 0 {
					return false, fmt.Errorf("%d updates still pending", n)
				}
				if got := fingerprint(t, doc); got != want {
					return false, fmt.Errorf("diverged\n got %s\nwant %s", got, want)
				}
				return true, nil
			},
			genPermutation(len(updates)),
		))
		properties.TestingRun(t)
	})
}

// Property: delivering an update more than once changes nothing. Redis fanout
// and websocket reconnects both make duplicates routine.
func TestPropertyDuplicatesAreHarmless(t *testing.T) {
	eachScenario(t, 1, func(t *testing.T, name string, updates [][]byte, want string) {
		properties := gopter.NewProperties(propertyParameters())
		properties.Property("repeated delivery changes nothing", prop.ForAll(
			func(repeats []int) (bool, error) {
				doc := crdt.NewDoc(1)
				for i, u := range updates {
					for range repeats[i] {
						if err := doc.ApplyUpdate(u); err != nil {
							return false, err
						}
					}
				}
				if got := fingerprint(t, doc); got != want {
					return false, fmt.Errorf("diverged\n got %s\nwant %s", got, want)
				}
				return true, nil
			},
			gen.SliceOfN(len(updates), gen.IntRange(1, 3)),
		))
		properties.TestingRun(t)
	})
}

// Property: two replicas that saw disjoint halves of the traffic converge once
// they exchange diffs against each other's state vectors. This is the sync
// handshake internal/room performs, run against random splits.
func TestPropertyReplicasConvergeAfterExchange(t *testing.T) {
	eachScenario(t, 2, func(t *testing.T, name string, updates [][]byte, want string) {
		properties := gopter.NewProperties(propertyParameters())
		properties.Property("replicas converge after exchanging diffs", prop.ForAll(
			func(assignment []int) (bool, error) {
				a := crdt.NewDoc(1)
				b := crdt.NewDoc(2)
				for i, u := range updates {
					// Every update goes to at least one replica, sometimes both.
					switch assignment[i] {
					case 0:
						if err := a.ApplyUpdate(u); err != nil {
							return false, err
						}
					case 1:
						if err := b.ApplyUpdate(u); err != nil {
							return false, err
						}
					default:
						if err := a.ApplyUpdate(u); err != nil {
							return false, err
						}
						if err := b.ApplyUpdate(u); err != nil {
							return false, err
						}
					}
				}

				// Exchange until nothing changes. More than two rounds are
				// sometimes needed: a delete for structs a replica has not
				// received yet is held pending and is therefore invisible to
				// its peers until those structs arrive, so the deletion
				// propagates one round behind the structs it refers to. Yjs
				// behaves the same way (pendingDs). Phase 4's anti-entropy has
				// to keep running for this reason - one exchange is not a
				// guarantee of convergence.
				const maxRounds = 6
				for range maxRounds {
					if fingerprint(t, a) == fingerprint(t, b) {
						break
					}
					if err := exchange(a, b); err != nil {
						return false, err
					}
				}

				gotA := fingerprint(t, a)
				gotB := fingerprint(t, b)
				if gotA != gotB {
					return false, fmt.Errorf("replicas diverged\n a %s\n b %s", gotA, gotB)
				}
				if gotA != want {
					return false, fmt.Errorf("replicas agree but differ from the reference\n got %s\nwant %s", gotA, want)
				}
				return true, nil
			},
			gen.SliceOfN(len(updates), gen.IntRange(0, 2)),
		))
		properties.TestingRun(t)
	})
}

// exchange runs one round of the sync handshake in both directions.
func exchange(a, b *crdt.Doc) error {
	svA, err := a.EncodeStateVector()
	if err != nil {
		return err
	}
	svB, err := b.EncodeStateVector()
	if err != nil {
		return err
	}
	forA, err := b.EncodeDiff(svA)
	if err != nil {
		return err
	}
	forB, err := a.EncodeDiff(svB)
	if err != nil {
		return err
	}
	if err := a.ApplyUpdate(forA); err != nil {
		return err
	}
	return b.ApplyUpdate(forB)
}

// Property: a document rebuilt from its own serialised state is the same
// document, no matter which random subset of updates built it. This is what
// makes snapshot compaction safe.
func TestPropertySnapshotRoundTrip(t *testing.T) {
	eachScenario(t, 1, func(t *testing.T, name string, updates [][]byte, _ string) {
		properties := gopter.NewProperties(propertyParameters())
		properties.Property("a snapshot rebuilds the same document", prop.ForAll(
			func(perm []int) (bool, error) {
				doc := crdt.NewDoc(1)
				for _, j := range perm {
					if err := doc.ApplyUpdate(updates[j]); err != nil {
						return false, err
					}
				}
				snapshot, err := doc.EncodeStateAsUpdate(nil)
				if err != nil {
					return false, err
				}
				restored := crdt.NewDoc(2)
				if err := restored.ApplyUpdate(snapshot); err != nil {
					return false, err
				}
				again, err := restored.EncodeStateAsUpdate(nil)
				if err != nil {
					return false, err
				}
				if !bytes.Equal(snapshot, again) {
					return false, fmt.Errorf("snapshot round trip changed the document")
				}
				return true, nil
			},
			genPermutation(len(updates)),
		))
		properties.TestingRun(t)
	})
}
