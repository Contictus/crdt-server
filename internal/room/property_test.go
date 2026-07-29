package room

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"

	"github.com/mesutokul/ycollab/internal/crdt"
	"github.com/mesutokul/ycollab/internal/protocol"
)

const propertyIterations = 1000

// scenarioUpdates returns a fixture scenario's incremental updates, in the
// order Yjs produced them.
func scenarioUpdates(t *testing.T, name string) [][]byte {
	t.Helper()
	dir := filepath.Join(fixturesDir, name)
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
		updates = append(updates, readFixture(t, name, n))
	}
	return updates
}

func scenarioNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(fixturesDir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(fixturesDir, e.Name(), "state.bin")); err == nil {
			out = append(out, e.Name())
		}
	}
	return out
}

// docPrint is a document identity that does not depend on struct boundaries:
// the same content split differently is the same document, and which split a
// replica ends up with depends only on how updates were batched.
func docPrint(t *testing.T, doc *crdt.Doc) string {
	t.Helper()
	sv, err := doc.EncodeStateVector()
	if err != nil {
		t.Fatalf("encode state vector: %v", err)
	}
	out := hex.EncodeToString(sv)
	names := doc.Roots()
	sort.Strings(names)
	for _, name := range names {
		out += "|" + name + "=" + crdt.AsText(doc.Get(name)).String()
	}
	return out
}

// Property: however two clients' traffic is interleaved through a room, the
// room and both clients end up with the same document.
//
// This is the convergence property from internal/crdt lifted to the layer that
// actually decides who hears what. A fanout bug - relaying to the wrong set, or
// dropping an update that failed to integrate on arrival - would show up here
// and nowhere in the CRDT tests.
func TestPropertyRoomFanoutConverges(t *testing.T) {
	now := time.Unix(1700000000, 0)
	for _, name := range scenarioNames(t) {
		updates := scenarioUpdates(t, name)
		if len(updates) < 2 {
			continue
		}
		// The reference: one document with everything applied.
		reference := crdt.NewDoc(9)
		for _, u := range updates {
			if err := reference.ApplyUpdate(u); err != nil {
				t.Fatalf("%s: reference: %v", name, err)
			}
		}
		want := docPrint(t, reference)

		t.Run(name, func(t *testing.T) {
			params := gopter.DefaultTestParametersWithSeed(20260729)
			params.MinSuccessfulTests = propertyIterations
			properties := gopter.NewProperties(params)
			properties.Property("room and clients converge", prop.ForAll(
				func(senders []int) (bool, error) {
					r := New(Config{Name: name, Now: func() time.Time { return now }})
					conns := []*fakeConn{{id: 1}, {id: 2}}
					for _, c := range conns {
						r.handle(joinCmd{c})
					}

					// Each update is authored by one of the two clients and
					// sent to the room, which relays it to the other.
					sent := make([][][]byte, len(conns))
					for i, u := range updates {
						from := senders[i]
						sent[from] = append(sent[from], u)
						r.handle(frameCmd{conns[from], protocol.WriteUpdate(u)})
					}

					if got := docPrint(t, r.doc); got != want {
						return false, fmt.Errorf("room diverged\n got %s\nwant %s", got, want)
					}
					if n := r.doc.PendingCount(); n != 0 {
						return false, fmt.Errorf("room left %d updates pending", n)
					}

					// A client's document is what it wrote plus what the room
					// sent it - exactly what a real editor would hold.
					for i, c := range conns {
						doc := crdt.NewDoc(crdt.ClientID(100 + i))
						for _, u := range sent[i] {
							if err := doc.ApplyUpdate(u); err != nil {
								return false, err
							}
						}
						for _, frame := range c.sent {
							msg, err := protocol.Decode(frame)
							if err != nil {
								return false, err
							}
							update, ok := msg.(protocol.UpdateMessage)
							if !ok {
								return false, fmt.Errorf("client %d got a %T, want an update", c.id, msg)
							}
							if err := doc.ApplyUpdate(update.Update); err != nil {
								return false, err
							}
						}
						if got := docPrint(t, doc); got != want {
							return false, fmt.Errorf("client %d diverged\n got %s\nwant %s", c.id, got, want)
						}
					}
					return true, nil
				},
				gen.SliceOfN(len(updates), gen.IntRange(0, 1)),
			))
			properties.TestingRun(t)
		})
	}
}
