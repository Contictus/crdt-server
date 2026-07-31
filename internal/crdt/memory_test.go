package crdt_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/mesutokul/ycollab/internal/crdt"
)

// corpusOrFixtures returns the largest real updates available: the measurement
// corpora when they have been generated, and the committed fixtures otherwise.
func corpusOrFixtures(t testing.TB) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, pattern := range []string{"/tmp/corpus-*.bin", "../../testdata/fixtures/*/state.bin"} {
		files, _ := filepath.Glob(pattern)
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil || len(b) == 0 {
				continue
			}
			out[filepath.Base(filepath.Dir(f))+"/"+filepath.Base(f)] = b
		}
	}
	if len(out) == 0 {
		t.Skip("no updates to measure")
	}
	return out
}

// The estimate has to be usable as a memory budget, which means it has to be in
// the same order as what the process actually allocates. It is a floor by
// construction - it does not model size classes, map buckets or the collector -
// so this measures how far under, and fails if the gap is large enough to make a
// budget meaningless.
func TestTheEstimateIsWithinReachOfRealMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates and forces the collector")
	}
	for name, update := range corpusOrFixtures(t) {
		if len(update) < 20000 {
			// Small documents are dominated by fixed overhead the estimate does
			// not claim to model, and the ratio there says nothing useful.
			continue
		}
		t.Run(name, func(t *testing.T) {
			// Hold the documents so nothing is collected mid-measurement, and
			// build several so per-document noise averages out.
			const copies = 8
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)

			docs := make([]*crdt.Doc, 0, copies)
			var estimate int64
			for range copies {
				d := crdt.NewDoc(1)
				if err := d.ApplyUpdate(update); err != nil {
					t.Fatal(err)
				}
				estimate += d.Usage().Bytes
				docs = append(docs, d)
			}

			runtime.GC()
			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			actual := int64(after.HeapAlloc) - int64(before.HeapAlloc)
			runtime.KeepAlive(docs)

			if actual <= 0 {
				t.Skipf("could not measure: heap moved by %d", actual)
			}
			ratio := float64(actual) / float64(estimate)
			t.Logf("%d docs: estimate %d, heap %d, actual/estimate %.2f (%d structs each)",
				copies, estimate, actual, ratio, docs[0].Usage().Structs)

			if ratio < 0.5 {
				t.Errorf("the estimate is %.2fx the real cost; it is meant to be a floor, not an overstatement", 1/ratio)
			}
			// Two and a half is generous, and deliberately so: this test guards
			// against the estimate becoming decorative, not against the allocator
			// having overhead.
			if ratio > 2.5 {
				t.Errorf("real memory is %.2fx the estimate; a budget written against it would be badly wrong", ratio)
			}
		})
	}
}

// The parts that must be exactly right, because a budget that drifts is a budget
// that stops meaning anything.
func TestUsageCountsWhatIsThere(t *testing.T) {
	d := crdt.NewDoc(1)
	if u := d.Usage(); u.Structs != 0 || u.Clients != 0 {
		t.Fatalf("an empty document reports %+v", u)
	}

	for name, update := range corpusOrFixtures(t) {
		d := crdt.NewDoc(1)
		if err := d.ApplyUpdate(update); err != nil {
			continue // Some fixtures are deliberately partial.
		}
		u := d.Usage()
		if u.Structs == 0 {
			t.Errorf("%s: applied %d bytes and the document holds no structs", name, len(update))
		}
		if u.Bytes <= 0 {
			t.Errorf("%s: %d structs cost nothing", name, u.Structs)
		}
		// A document cannot cost less than the bytes that were applied to it:
		// the wire form is the compressed one.
		if len(update) > 1000 && u.Bytes < int64(len(update)) {
			t.Errorf("%s: %d bytes of update became an estimate of %d", name, len(update), u.Bytes)
		}
	}
}

// Applying more makes it cost more. Obvious, and the thing that breaks first if
// somebody caches the walk in the wrong place.
func TestUsageGrowsWithTheDocument(t *testing.T) {
	updates := corpusOrFixtures(t)
	var biggest []byte
	for _, u := range updates {
		if len(u) > len(biggest) {
			biggest = u
		}
	}
	d := crdt.NewDoc(1)
	empty := d.Usage()
	if err := d.ApplyUpdate(biggest); err != nil {
		t.Fatal(err)
	}
	full := d.Usage()
	if full.Bytes <= empty.Bytes || full.Structs <= empty.Structs {
		t.Fatalf("empty %+v, after applying %d bytes %+v", empty, len(biggest), full)
	}
	// Applying the same update again changes nothing: it is idempotent, so the
	// cost must not double.
	if err := d.ApplyUpdate(biggest); err != nil {
		t.Fatal(err)
	}
	if again := d.Usage(); again.Structs != full.Structs {
		t.Errorf("re-applying an update took the document from %d structs to %d", full.Structs, again.Structs)
	}
}

// The walk is O(structs) and the room runs it on a timer, so its cost has to be
// known rather than assumed.
func BenchmarkUsage(b *testing.B) {
	for name, update := range corpusOrFixtures(b) {
		if len(update) < 20000 {
			continue
		}
		b.Run(name, func(b *testing.B) {
			d := crdt.NewDoc(1)
			if err := d.ApplyUpdate(update); err != nil {
				b.Fatal(err)
			}
			u := d.Usage()
			b.ReportMetric(float64(u.Structs), "structs")
			b.ReportMetric(float64(u.Bytes)/(1<<20), "MiB")
			b.ResetTimer()
			for b.Loop() {
				_ = d.Usage()
			}
		})
	}
}
