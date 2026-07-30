package crdt_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/mesutokul/ycollab/internal/crdt"
)

// The paths a busy server spends its time in: integrating an update, answering
// a client's state vector, and writing a snapshot. Everything else in the room
// is bookkeeping around these three.
//
// They exist to make an optimisation an argument with evidence rather than a
// hunch. Run them with:
//
//	go test ./internal/crdt/ -run XXX -bench . -benchmem

// benchUpdates reads a scenario's updates. It does its own reading rather than
// borrowing the test helpers, which take a *testing.T.
func benchUpdates(b *testing.B, scenario string) [][]byte {
	b.Helper()
	dir := filepath.Join(filepath.FromSlash(fixturesDir), scenario)
	entries, err := os.ReadDir(dir)
	if err != nil {
		b.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if n := e.Name(); strings.HasPrefix(n, "update-") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	updates := make([][]byte, 0, len(names))
	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			b.Fatal(err)
		}
		updates = append(updates, raw)
	}
	return updates
}

// benchDoc builds a document from a scenario's updates.
func benchDoc(tb *testing.B, scenario string) (*crdt.Doc, [][]byte) {
	tb.Helper()
	updates := benchUpdates(tb, scenario)
	doc := crdt.NewDoc(1)
	for _, u := range updates {
		if err := doc.ApplyUpdate(u); err != nil {
			tb.Fatal(err)
		}
	}
	return doc, updates
}

func BenchmarkApplyUpdate(b *testing.B) {
	_, updates := benchDoc(b, "text-three-client-interleaved")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		// A fresh document every cycle would measure allocation rather than
		// integration, and a shared one would measure a document growing
		// without bound. Reapplying the same updates measures the path a server
		// actually runs most: an update that is already known, checked and
		// dropped, plus one that is new.
		doc := crdt.NewDoc(1)
		for _, u := range updates {
			if err := doc.ApplyUpdate(u); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// A duplicate update is the common case under load: Redis fanout and reconnects
// both produce them, and the answer has to be cheap.
func BenchmarkApplyDuplicate(b *testing.B) {
	doc, updates := benchDoc(b, "text-three-client-interleaved")
	update := updates[len(updates)-1]
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := doc.ApplyUpdate(update); err != nil {
			b.Fatal(err)
		}
	}
}

// EncodeDiff runs once per client handshake and once per anti-entropy repair.
func BenchmarkEncodeDiff(b *testing.B) {
	doc, _ := benchDoc(b, "varint-boundaries")
	empty, err := crdt.NewDoc(2).EncodeStateVector()
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := doc.EncodeDiff(empty); err != nil {
			b.Fatal(err)
		}
	}
}

// A snapshot is written every 500 updates and once more when a room is evicted.
func BenchmarkEncodeStateAsUpdate(b *testing.B) {
	doc, _ := benchDoc(b, "varint-boundaries")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := doc.EncodeStateAsUpdate(nil); err != nil {
			b.Fatal(err)
		}
	}
}

// The state vector goes out on every handshake and every anti-entropy
// announcement, so it is the most frequent encode of the three.
func BenchmarkEncodeStateVector(b *testing.B) {
	doc, _ := benchDoc(b, "varint-boundaries")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := doc.EncodeStateVector(); err != nil {
			b.Fatal(err)
		}
	}
}
