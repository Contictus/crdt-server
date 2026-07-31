package room

// The tenancy boundary, at the layer that enforces it.
//
// Every test here is about one property: knowing a document's name is not
// enough to open it. Without that, "multi-tenancy" is a column.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mesutokul/ycollab/internal/store"
)

func tenantManager(t *testing.T, s Persistence) *Manager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		// The manager's rooms stop with the context; waiting keeps one test's
		// goroutines out of the next one's.
		done := make(chan struct{})
		go func() { close(done) }()
		<-done
	})
	return NewManager(ctx, ManagerConfig{Room: Config{IdleTimeout: time.Hour, Store: s}})
}

// The whole point. Two tenants, one document name.
func TestAnotherTenantCannotOpenTheSameName(t *testing.T) {
	m := tenantManager(t, &fakeStore{})

	if _, err := m.Join("shared-name", &fakeConn{id: 1}, "acme"); err != nil {
		t.Fatalf("the owner was refused their own document: %v", err)
	}
	_, err := m.Join("shared-name", &fakeConn{id: 2}, "globex")
	if !errors.Is(err, ErrWrongOwner) {
		t.Fatalf("a second tenant opened the same name: err=%v", err)
	}
	// And the first tenant still works, from the resident room.
	if _, err := m.Join("shared-name", &fakeConn{id: 3}, "acme"); err != nil {
		t.Fatalf("the owner was refused after somebody else was: %v", err)
	}
}

// A connection claiming no owner is not a skeleton key. That direction matters
// more than the other: it is what a token that predates tenancy looks like.
func TestNoOwnerIsAnOwner(t *testing.T) {
	m := tenantManager(t, &fakeStore{})

	if _, err := m.Join("owned", &fakeConn{id: 1}, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Join("owned", &fakeConn{id: 2}, ""); !errors.Is(err, ErrWrongOwner) {
		t.Fatalf("a connection with no owner opened an owned document: err=%v", err)
	}

	// The mirror: a tenant does not inherit the documents nobody owns, which is
	// every document in a database that predates tenancy.
	if _, err := m.Join("unowned", &fakeConn{id: 3}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Join("unowned", &fakeConn{id: 4}, "acme"); !errors.Is(err, ErrWrongOwner) {
		t.Fatalf("a tenant claimed a document owned by nobody: err=%v", err)
	}
}

// A tenant may be named by its slug in one token and by its UUID in another.
// The comparison is over the derived id, so both are the same owner.
func TestATenantNamedTwoWaysIsOneOwner(t *testing.T) {
	m := tenantManager(t, &fakeStore{})

	const tenant = "acme"
	id := store.OwnerID(tenant).String()
	if store.OwnerID(id) != store.OwnerID(tenant) {
		t.Fatalf("a tenant's own id does not resolve to itself")
	}
	if _, err := m.Join("doc", &fakeConn{id: 1}, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Join("doc", &fakeConn{id: 2}, id); err != nil {
		t.Fatalf("the same tenant was refused under its id: %v", err)
	}
}

// Without a database there is nothing durable to consult, and the room's own
// memory has to hold the line. It is what a server run without -database-url
// does, and a hole there would be a hole in the demo everybody tries first.
func TestTheBoundaryHoldsWithoutADatabase(t *testing.T) {
	m := tenantManager(t, nil)

	if _, err := m.Join("doc", &fakeConn{id: 1}, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Join("doc", &fakeConn{id: 2}, "globex"); !errors.Is(err, ErrWrongOwner) {
		t.Fatalf("with no database a second tenant opened the document: err=%v", err)
	}
}

// Two tenants reaching for a document that does not exist yet, at the same
// moment. Exactly one may end up owning it, and the other must be refused -
// they must not both be told yes and then quietly share it.
func TestARaceToCreateHasOneWinner(t *testing.T) {
	m := tenantManager(t, &fakeStore{})

	const racers = 16
	var wg sync.WaitGroup
	results := make(chan error, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Half claim one tenant and half the other, so whichever wins there
			// is somebody on the losing side to be refused.
			owner := "acme"
			if i%2 == 1 {
				owner = "globex"
			}
			_, err := m.Join("contested", &fakeConn{id: uint64(i + 1)}, owner)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	allowed, refused := 0, 0
	for err := range results {
		switch {
		case err == nil:
			allowed++
		case errors.Is(err, ErrWrongOwner):
			refused++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if allowed != racers/2 || refused != racers/2 {
		t.Fatalf("%d allowed and %d refused; exactly one tenant's half should have got in", allowed, refused)
	}
}

// Import is the restore path. Naming an owner checks it; omitting one leaves
// whatever is there alone.
func TestImportChecksTheOwnerItIsGiven(t *testing.T) {
	s := &fakeStore{}
	update := oneUpdate(t)

	// Create it as acme's.
	if err := Import(context.Background(), MergeConfig{Store: s, Owner: "acme"}, "doc", update); err != nil {
		t.Fatal(err)
	}
	// Another tenant may not write into it.
	err := Import(context.Background(), MergeConfig{Store: s, Owner: "globex"}, "doc", update)
	if !errors.Is(err, store.ErrWrongOwner) {
		t.Fatalf("a restore wrote into another tenant's document: err=%v", err)
	}
	// The operator who does not say whose it is gets the old behaviour, because
	// a restore is an operator action on a surface that is above tenancy.
	if err := Import(context.Background(), MergeConfig{Store: s}, "doc", update); err != nil {
		t.Fatalf("a restore with no owner was refused: %v", err)
	}
}

// A document name that hashes into an owner id, or an owner name that hashes
// into a document id, must not collide: they are hashed under different
// namespaces for exactly this reason.
func TestDocumentAndOwnerNamespacesDoNotCollide(t *testing.T) {
	for _, name := range []string{"acme", "notes", "", "a"} {
		if name == "" {
			// The empty tenant is NilUUID by definition; the empty document
			// name is not a name the gateway accepts.
			if store.OwnerID(name) != store.NilUUID {
				t.Error("the empty tenant is not NilUUID")
			}
			continue
		}
		if store.OwnerID(name) == store.DocumentID(name) {
			t.Errorf("%q is the same identifier as a document and as an owner", name)
		}
	}
}

// oneUpdate is a real update a real client produced, so Import's decode step
// is exercised rather than sidestepped.
func oneUpdate(t *testing.T) []byte {
	t.Helper()
	return readFixture(t, "text-insert-single", "update-000.bin")
}
