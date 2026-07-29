package store_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/mesutokul/ycollab/internal/store"
)

// The brief asks for an integration test against a real Postgres in Docker,
// not a mock, and this package has nothing worth testing against a mock: every
// interesting property here is a property of the database - identity columns,
// transaction boundaries, advisory locks.
//
//	docker compose -f deploy/docker-compose.yml up -d
//	YCOLLAB_TEST_DATABASE_URL=postgres://ycollab:ycollab@127.0.0.1:5433/ycollab go test ./internal/store/
const dbEnv = "YCOLLAB_TEST_DATABASE_URL"

func testStore(t *testing.T) *store.Store {
	t.Helper()
	url := os.Getenv(dbEnv)
	if url == "" {
		t.Skipf("%s is not set; start deploy/docker-compose.yml to run these", dbEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s, err := store.Open(ctx, url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// Each test gets its own document, so tests do not interfere and none of them
// has to truncate a table somebody else might be using.
func testDoc(t *testing.T) store.UUID {
	t.Helper()
	return store.DocumentID("test-" + t.Name() + "-" + time.Now().Format(time.RFC3339Nano))
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestLoadCreatesAnEmptyDocument(t *testing.T) {
	s := testStore(t)
	c := ctx(t)
	id := testDoc(t)

	doc, err := s.Load(c, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if doc.Snapshot != nil || doc.SnapshotSeq != 0 || len(doc.Updates) != 0 || doc.LastSeq != 0 {
		t.Fatalf("fresh document is not empty: %+v", doc)
	}

	// Loading twice must not fail on the primary key, because every connection
	// to a document does it.
	if _, err := s.Load(c, id); err != nil {
		t.Fatalf("second load: %v", err)
	}
}

func TestAppendAndLoadPreservesOrder(t *testing.T) {
	s := testStore(t)
	c := ctx(t)
	id := testDoc(t)
	if _, err := s.Load(c, id); err != nil {
		t.Fatal(err)
	}

	want := [][]byte{[]byte("one"), []byte("two"), []byte("three")}
	last, err := s.Append(c, id, want[:2])
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	next, err := s.Append(c, id, want[2:])
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if next <= last {
		t.Fatalf("seq did not advance: %d then %d", last, next)
	}

	doc, err := s.Load(c, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(doc.Updates) != len(want) {
		t.Fatalf("got %d updates, want %d", len(doc.Updates), len(want))
	}
	for i := range want {
		if !bytes.Equal(doc.Updates[i], want[i]) {
			t.Fatalf("update %d is %q, want %q", i, doc.Updates[i], want[i])
		}
	}
	if doc.LastSeq != next {
		t.Fatalf("LastSeq %d, want %d", doc.LastSeq, next)
	}
}

func TestCompactFoldsTheLog(t *testing.T) {
	s := testStore(t)
	c := ctx(t)
	id := testDoc(t)
	if _, err := s.Load(c, id); err != nil {
		t.Fatal(err)
	}

	watermark, err := s.Append(c, id, [][]byte{[]byte("a"), []byte("b"), []byte("c")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Compact(c, id, []byte("snapshot-1"), watermark); err != nil {
		t.Fatalf("compact: %v", err)
	}

	doc, err := s.Load(c, id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(doc.Snapshot, []byte("snapshot-1")) {
		t.Fatalf("snapshot %q", doc.Snapshot)
	}
	if doc.SnapshotSeq != watermark {
		t.Fatalf("snapshot_seq %d, want %d", doc.SnapshotSeq, watermark)
	}
	if len(doc.Updates) != 0 {
		t.Fatalf("compaction left %d rows behind", len(doc.Updates))
	}
}

// An update that arrives while a snapshot is being prepared has a higher seq
// than the watermark, so it must survive the delete. Losing it would be exactly
// the data loss the single-transaction requirement exists to prevent.
func TestCompactKeepsUpdatesAboveTheWatermark(t *testing.T) {
	s := testStore(t)
	c := ctx(t)
	id := testDoc(t)
	if _, err := s.Load(c, id); err != nil {
		t.Fatal(err)
	}

	watermark, err := s.Append(c, id, [][]byte{[]byte("folded")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(c, id, [][]byte{[]byte("in flight")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Compact(c, id, []byte("snapshot"), watermark); err != nil {
		t.Fatalf("compact: %v", err)
	}

	doc, err := s.Load(c, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Updates) != 1 || !bytes.Equal(doc.Updates[0], []byte("in flight")) {
		t.Fatalf("remaining log is %q", doc.Updates)
	}
}

// C2: any replica may compact any document. An older snapshot must not be able
// to overwrite a newer one and then delete rows the newer one does not contain.
func TestCompactRefusesToGoBackwards(t *testing.T) {
	s := testStore(t)
	c := ctx(t)
	id := testDoc(t)
	if _, err := s.Load(c, id); err != nil {
		t.Fatal(err)
	}

	first, err := s.Append(c, id, [][]byte{[]byte("a")})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Append(c, id, [][]byte{[]byte("b")})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Compact(c, id, []byte("newer"), second); err != nil {
		t.Fatalf("compact: %v", err)
	}
	err = s.Compact(c, id, []byte("older"), first)
	if !errors.Is(err, store.ErrStaleSnapshot) {
		t.Fatalf("got %v, want ErrStaleSnapshot", err)
	}

	doc, err := s.Load(c, id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(doc.Snapshot, []byte("newer")) {
		t.Fatalf("the losing snapshot won: %q", doc.Snapshot)
	}
	if doc.SnapshotSeq != second {
		t.Fatalf("snapshot_seq went backwards: %d", doc.SnapshotSeq)
	}
}

// Compaction happens in one transaction, so a reader either sees the old
// snapshot with the whole log, or the new snapshot with the log pruned - never
// a document missing both.
func TestCompactIsAtomicForReaders(t *testing.T) {
	s := testStore(t)
	c := ctx(t)
	id := testDoc(t)
	if _, err := s.Load(c, id); err != nil {
		t.Fatal(err)
	}
	watermark, err := s.Append(c, id, [][]byte{[]byte("a"), []byte("b")})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- s.Compact(c, id, []byte("snapshot"), watermark) }()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		doc, err := s.Load(c, id)
		if err != nil {
			t.Errorf("load during compaction: %v", err)
			break
		}
		if doc.Snapshot == nil && len(doc.Updates) != 2 {
			t.Fatalf("reader saw a document with no snapshot and %d updates", len(doc.Updates))
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("compact: %v", err)
			}
			return
		default:
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("compact: %v", err)
	}
}

func TestUpdateCount(t *testing.T) {
	s := testStore(t)
	c := ctx(t)
	id := testDoc(t)
	if _, err := s.Load(c, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(c, id, [][]byte{[]byte("a"), []byte("b")}); err != nil {
		t.Fatal(err)
	}
	n, err := s.UpdateCount(c, id)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("count %d, want 2", n)
	}
}

// Documents are keyed by UUID but rooms are named in a URL, so the mapping has
// to be deterministic and has to leave real UUIDs alone.
func TestDocumentID(t *testing.T) {
	a := store.DocumentID("notes-2026")
	if a != store.DocumentID("notes-2026") {
		t.Fatal("the same name produced two different ids")
	}
	if a == store.DocumentID("notes-2027") {
		t.Fatal("two names collided")
	}
	if got := a.String()[14]; got != '5' {
		t.Fatalf("version nibble is %q, want '5'", got)
	}
	if v := a[8] & 0xc0; v != 0x80 {
		t.Fatalf("variant bits are %#x, want 0x80", v)
	}

	const canonical = "f81d4fae-7dec-11d0-a765-00a0c91e6bf6"
	id := store.DocumentID(canonical)
	if id.String() != canonical {
		t.Fatalf("a name that is already a UUID was rehashed: %s", id)
	}
	if _, err := store.ParseUUID("not-a-uuid"); !errors.Is(err, store.ErrBadUUID) {
		t.Fatalf("got %v, want ErrBadUUID", err)
	}
	if _, err := store.ParseUUID("f81d4fae-7dec-11d0-a765-00a0c91e6bfZ"); !errors.Is(err, store.ErrBadUUID) {
		t.Fatal("accepted a non-hex UUID")
	}
}
