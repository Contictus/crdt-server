package crdt_test

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"testing"

	"github.com/mesutokul/ycollab/internal/crdt"
	"github.com/mesutokul/ycollab/internal/crdt/lib0"
)

// expectedDoc mirrors the parts of expected.json that describe the document
// Yjs ended up with.
type expectedDoc struct {
	Scenario string `json:"scenario"`
	State    struct {
		Types map[string]struct {
			Kind   string          `json:"kind"`
			String string          `json:"string"`
			JSON   json.RawMessage `json:"json"`
			XML    string          `json:"xml"`
		} `json:"types"`
		StateVector map[string]uint64   `json:"stateVector"`
		DeleteSet   map[string][][2]int `json:"deleteSet"`
	} `json:"state"`
}

func loadExpected(t *testing.T, dir string) expectedDoc {
	t.Helper()
	var exp expectedDoc
	if err := json.Unmarshal(readFixture(t, filepath.Join(dir, "expected.json")), &exp); err != nil {
		t.Fatalf("%s: parse expected.json: %v", dir, err)
	}
	return exp
}

// jsonSafe converts decoded CRDT values into the shape tools/fixturegen/dump.mjs
// produces, so a decoded document can be compared with the fixture directly.
func jsonSafe(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case lib0.Undefined:
		// JSON has no undefined; the generator tags it (see dump.mjs jsonSafe).
		return map[string]any{"$undefined": true}
	case lib0.BigInt:
		return map[string]any{"$bigint": strconv.FormatInt(int64(t), 10)}
	case []byte:
		bytes := make([]any, len(t))
		for i, b := range t {
			bytes[i] = float64(b)
		}
		return map[string]any{"$bytes": bytes}
	case *crdt.ContentDoc:
		return map[string]any{"$subdoc": t.GUID}
	case *lib0.Object:
		out := make(map[string]any, len(t.Fields))
		for _, f := range t.Fields {
			out[f.Key] = jsonSafe(f.Value)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = jsonSafe(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = jsonSafe(val)
		}
		return out
	case int64:
		return float64(t)
	case float32:
		return float64(t)
	case string, bool, float64:
		return t
	default:
		return fmt.Sprintf("<unconvertible %T>", v)
	}
}

// applyState builds a document from a fixture's full state.
func applyState(t *testing.T, dir string) *crdt.Doc {
	t.Helper()
	doc := crdt.NewDoc(1)
	if err := doc.ApplyUpdate(readFixture(t, filepath.Join(dir, "state.bin"))); err != nil {
		t.Fatalf("%s: apply state.bin: %v", filepath.Base(dir), err)
	}
	if n := doc.PendingCount(); n != 0 {
		t.Errorf("%s: %d updates left pending after applying a complete state", filepath.Base(dir), n)
	}
	return doc
}

// checkContent compares a document against the fixture's expected content.
// XML types are checked only for their state, not their shape: the Go API
// deliberately exposes no XML accessors (DECISIONS.md §D8).
func checkContent(t *testing.T, name string, doc *crdt.Doc, exp expectedDoc) {
	t.Helper()
	for typeName, want := range exp.State.Types {
		switch want.Kind {
		case "text":
			if got := doc.Text(typeName).String(); got != want.String {
				t.Errorf("%s: text %q = %q, want %q", name, typeName, got, want.String)
			}
		case "map":
			var wantJSON map[string]any
			if err := json.Unmarshal(want.JSON, &wantJSON); err != nil {
				t.Fatalf("%s: parse expected map: %v", name, err)
			}
			got, ok := jsonSafe(doc.Map(typeName).Type().ToJSON()).(map[string]any)
			if !ok {
				t.Errorf("%s: map %q did not render as an object", name, typeName)
				continue
			}
			if !reflect.DeepEqual(got, wantJSON) {
				t.Errorf("%s: map %q =\n %#v\nwant\n %#v", name, typeName, got, wantJSON)
			}
		}
	}
}

// Applying the full state of every fixture must reproduce the document Yjs had.
func TestApplyStateReproducesDocument(t *testing.T) {
	for _, dir := range scenarioDirs(t) {
		name := filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			exp := loadExpected(t, dir)
			doc := applyState(t, dir)
			checkContent(t, name, doc, exp)
			checkStateVector(t, name, doc, exp)
			checkDeleteSet(t, name, doc, exp)
		})
	}
}

func checkStateVector(t *testing.T, name string, doc *crdt.Doc, exp expectedDoc) {
	t.Helper()
	got := doc.StateVector()
	if len(got) != len(exp.State.StateVector) {
		t.Errorf("%s: state vector covers %d clients, want %d", name, len(got), len(exp.State.StateVector))
	}
	for client, clock := range exp.State.StateVector {
		id, err := strconv.ParseUint(client, 10, 64)
		if err != nil {
			t.Fatalf("bad client id %q", client)
		}
		if g := got[crdt.ClientID(id)]; uint64(g) != clock {
			t.Errorf("%s: clock for client %s = %d, want %d", name, client, g, clock)
		}
	}
}

func checkDeleteSet(t *testing.T, name string, doc *crdt.Doc, exp expectedDoc) {
	t.Helper()
	got := doc.DeleteSet()
	for client, ranges := range exp.State.DeleteSet {
		id, err := strconv.ParseUint(client, 10, 64)
		if err != nil {
			t.Fatalf("bad client id %q", client)
		}
		gotRanges := got.Clients[crdt.ClientID(id)]
		if len(gotRanges) != len(ranges) {
			t.Errorf("%s: client %s has %d deleted ranges, want %d (%v)", name, client, len(gotRanges), len(ranges), gotRanges)
			continue
		}
		for i, r := range ranges {
			if int(gotRanges[i].Clock) != r[0] || gotRanges[i].Len != r[1] {
				t.Errorf("%s: client %s range %d = (%d,%d), want (%d,%d)",
					name, client, i, gotRanges[i].Clock, gotRanges[i].Len, r[0], r[1])
			}
		}
	}
	for client := range got.Clients {
		if _, ok := exp.State.DeleteSet[strconv.FormatUint(uint64(client), 10)]; !ok {
			t.Errorf("%s: client %d has deletions Yjs did not report", name, client)
		}
	}
}

// Feeding the incremental updates one at a time must land on the same document
// as applying the full state at once.
func TestIncrementalUpdatesMatchFullState(t *testing.T) {
	for _, dir := range scenarioDirs(t) {
		name := filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			exp := loadExpected(t, dir)
			doc := crdt.NewDoc(1)
			var files []string
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if len(e.Name()) > 7 && e.Name()[:7] == "update-" {
					files = append(files, e.Name())
				}
			}
			sort.Strings(files) // update-000, update-001, ...
			if len(files) == 0 {
				t.Skip("no incremental updates")
			}
			for _, f := range files {
				if err := doc.ApplyUpdate(readFixture(t, filepath.Join(dir, f))); err != nil {
					t.Fatalf("apply %s: %v", f, err)
				}
			}
			if n := doc.PendingCount(); n != 0 {
				t.Errorf("%s: %d updates still pending", name, n)
			}
			checkContent(t, name+" (incremental)", doc, exp)
			checkStateVector(t, name+" (incremental)", doc, exp)
		})
	}
}

// Updates applied in reverse order must still converge: an update that arrives
// before the one it depends on is held and retried, not dropped.
func TestOutOfOrderUpdatesConverge(t *testing.T) {
	for _, dir := range scenarioDirs(t) {
		name := filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			exp := loadExpected(t, dir)
			var files []string
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if len(e.Name()) > 7 && e.Name()[:7] == "update-" {
					files = append(files, e.Name())
				}
			}
			sort.Sort(sort.Reverse(sort.StringSlice(files)))
			if len(files) < 2 {
				t.Skip("needs at least two updates")
			}
			doc := crdt.NewDoc(1)
			for _, f := range files {
				if err := doc.ApplyUpdate(readFixture(t, filepath.Join(dir, f))); err != nil {
					t.Fatalf("apply %s: %v", f, err)
				}
			}
			if n := doc.PendingCount(); n != 0 {
				t.Errorf("%s: %d updates still pending after all of them arrived", name, n)
			}
			checkContent(t, name+" (reversed)", doc, exp)
			checkStateVector(t, name+" (reversed)", doc, exp)
		})
	}
}

// A document re-encoded and applied into a fresh document must be identical.
// This is the property the server relies on when it serves a snapshot.
func TestEncodeStateAsUpdateRoundTrips(t *testing.T) {
	for _, dir := range scenarioDirs(t) {
		name := filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			exp := loadExpected(t, dir)
			src := applyState(t, dir)
			out, err := src.EncodeStateAsUpdate(nil)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			dst := crdt.NewDoc(2)
			if err := dst.ApplyUpdate(out); err != nil {
				t.Fatalf("apply re-encoded state: %v", err)
			}
			checkContent(t, name+" (re-encoded)", dst, exp)
			checkStateVector(t, name+" (re-encoded)", dst, exp)
			checkDeleteSet(t, name+" (re-encoded)", dst, exp)
		})
	}
}

// coveredRanges reports which clock ranges an update carries, per client, with
// Skip structs excluded - they mark ranges deliberately left out.
func coveredRanges(u *crdt.Update) map[crdt.ClientID][][2]uint64 {
	out := make(map[crdt.ClientID][][2]uint64)
	for _, block := range u.Clients {
		clock := block.StartClock
		for _, s := range block.Structs {
			end := clock + crdt.Clock(s.StructLen())
			if _, isSkip := s.(*crdt.Skip); !isSkip {
				ranges := out[block.Client]
				if n := len(ranges); n > 0 && ranges[n-1][1] == uint64(clock) {
					ranges[n-1][1] = uint64(end) // contiguous: extend
				} else {
					ranges = append(ranges, [2]uint64{uint64(clock), uint64(end)})
				}
				out[block.Client] = ranges
			}
			clock = end
		}
	}
	return out
}

// A diff against a peer's state vector must carry exactly what the peer lacks.
func TestEncodeDiffMatchesFixture(t *testing.T) {
	for _, dir := range scenarioDirs(t) {
		diffPath := filepath.Join(dir, "diff.bin")
		svPath := filepath.Join(dir, "diff-sv.bin")
		if _, err := os.Stat(diffPath); err != nil {
			continue
		}
		name := filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			doc := applyState(t, dir)
			got, err := doc.EncodeDiff(readFixture(t, svPath))
			if err != nil {
				t.Fatalf("encode diff: %v", err)
			}
			// The bytes need not match Yjs's diff byte for byte - where an item
			// gets split is an implementation detail - but the diff must cover
			// exactly the same clock ranges, no more and no less. Sending less
			// loses an edit; sending more wastes bandwidth on every sync.
			ours, err := crdt.DecodeUpdate(got)
			if err != nil {
				t.Fatalf("decode our diff: %v", err)
			}
			theirs, err := crdt.DecodeUpdate(readFixture(t, diffPath))
			if err != nil {
				t.Fatalf("decode fixture diff: %v", err)
			}
			gotRanges := coveredRanges(ours)
			wantRanges := coveredRanges(theirs)
			if !reflect.DeepEqual(gotRanges, wantRanges) {
				t.Errorf("%s: diff covers %v, want %v", name, gotRanges, wantRanges)
			}
		})
	}
}

// Applying the same update twice must change nothing: the second copy is
// entirely below the document's state vector.
func TestApplyIsIdempotent(t *testing.T) {
	for _, dir := range scenarioDirs(t) {
		name := filepath.Base(dir)
		state := readFixture(t, filepath.Join(dir, "state.bin"))
		doc := crdt.NewDoc(1)
		if err := doc.ApplyUpdate(state); err != nil {
			t.Fatalf("%s: first apply: %v", name, err)
		}
		first, err := doc.EncodeStateAsUpdate(nil)
		if err != nil {
			t.Fatalf("%s: encode: %v", name, err)
		}
		if err := doc.ApplyUpdate(state); err != nil {
			t.Fatalf("%s: second apply: %v", name, err)
		}
		second, err := doc.EncodeStateAsUpdate(nil)
		if err != nil {
			t.Fatalf("%s: encode: %v", name, err)
		}
		if string(first) != string(second) {
			t.Errorf("%s: applying the same update twice changed the document", name)
		}
	}
}

// Concurrent inserts at the same position converge to the same order on every
// replica, and that order is decided by client id. Applying the two updates in
// either order must give the same text.
func TestConcurrentInsertsConvergeEitherWay(t *testing.T) {
	for _, name := range []string{"text-concurrent-same-index", "text-concurrent-after-shared-origin"} {
		dir := filepath.Join(filepath.FromSlash(fixturesDir), name)
		exp := loadExpected(t, dir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var files []string
		for _, e := range entries {
			if len(e.Name()) > 7 && e.Name()[:7] == "update-" {
				files = append(files, e.Name())
			}
		}
		sort.Strings(files)

		forward := crdt.NewDoc(1)
		for _, f := range files {
			if err := forward.ApplyUpdate(readFixture(t, filepath.Join(dir, f))); err != nil {
				t.Fatal(err)
			}
		}
		backward := crdt.NewDoc(1)
		for i := len(files) - 1; i >= 0; i-- {
			if err := backward.ApplyUpdate(readFixture(t, filepath.Join(dir, files[i]))); err != nil {
				t.Fatal(err)
			}
		}
		want := exp.State.Types["text"].String
		if got := forward.Text("text").String(); got != want {
			t.Errorf("%s: forward order = %q, want %q", name, got, want)
		}
		if got := backward.Text("text").String(); got != want {
			t.Errorf("%s: reverse order = %q, want %q", name, got, want)
		}
	}
}

func TestUTF16LengthsSurviveIntegration(t *testing.T) {
	dir := filepath.Join(filepath.FromSlash(fixturesDir), "varint-boundaries")
	if _, err := os.Stat(dir); err != nil {
		t.Skip("fixture missing")
	}
	doc := applyState(t, dir)
	// Whatever the scenario holds, every client's clock must land exactly where
	// Yjs said it does - which only happens if string lengths are counted in
	// UTF-16 code units.
	exp := loadExpected(t, dir)
	checkStateVector(t, "varint-boundaries", doc, exp)
	_ = math.MaxInt32
}
