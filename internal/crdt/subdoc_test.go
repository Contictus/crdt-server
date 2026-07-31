package crdt_test

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/mesutokul/ycollab/internal/crdt"
)

// The fixture is a real Yjs document with two subdocuments, one of them removed
// again. A parent document is the only thing that names its subdocuments, so
// reading them out is what connects "delete this document" to "and these".
func TestSubdocsNamesTheLiveReferences(t *testing.T) {
	doc := crdt.NewDoc(1)
	if err := doc.ApplyUpdate(readFixture(t, fixture("subdocument", "state.bin"))); err != nil {
		t.Fatalf("apply: %v", err)
	}

	got := doc.Subdocs()
	want := []string{"chapter-one"}
	if !slices.Equal(got, want) {
		// "appendix" was set and then removed, so it is not part of this
		// document any more - and an operator deciding what to delete must not
		// be told otherwise.
		t.Errorf("Subdocs() = %v, want %v", got, want)
	}
}

// A document with no subdocuments has none, rather than a nil that a caller has
// to special-case.
func TestSubdocsOfAnOrdinaryDocument(t *testing.T) {
	doc := crdt.NewDoc(1)
	if err := doc.ApplyUpdate(readFixture(t, fixture("text-insert-single", "state.bin"))); err != nil {
		t.Fatal(err)
	}
	if got := doc.Subdocs(); len(got) != 0 {
		t.Errorf("Subdocs() = %v, want none", got)
	}
}

// The reference has to survive a round trip through this engine untouched, or a
// client's subdocument would come back pointing at nothing. That is the part
// the brief always required; naming the guids is what is new.
func TestSubdocumentsSurviveARoundTrip(t *testing.T) {
	state := readFixture(t, fixture("subdocument", "state.bin"))
	doc := crdt.NewDoc(1)
	if err := doc.ApplyUpdate(state); err != nil {
		t.Fatal(err)
	}
	out, err := doc.EncodeStateAsUpdate(nil)
	if err != nil {
		t.Fatal(err)
	}
	again := crdt.NewDoc(2)
	if err := again.ApplyUpdate(out); err != nil {
		t.Fatalf("the re-encoded document would not apply: %v", err)
	}
	if !slices.Equal(again.Subdocs(), doc.Subdocs()) {
		t.Errorf("after a round trip: %v, want %v", again.Subdocs(), doc.Subdocs())
	}
}

func fixture(parts ...string) string {
	return filepath.Join(append([]string{filepath.FromSlash(fixturesDir)}, parts...)...)
}
