package main_test

// Multi-tenancy end to end: a real server process, a real database, real
// WebSocket clients, and two tenants who must not be able to reach each other's
// documents by guessing a name.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/mesutokul/ycollab/internal/auth"
	"github.com/mesutokul/ycollab/internal/crdt"
	"github.com/mesutokul/ycollab/internal/protocol"
)

// tenantToken mints a token for one tenant and one document.
func tenantToken(t *testing.T, doc, owner string) string {
	t.Helper()
	token, err := auth.Sign([]byte(testSecret), auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "test",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Doc:  doc,
		Perm: auth.PermissionWrite,
		Own:  owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// The property the whole feature exists for: knowing the name is not enough.
func TestATenantCannotOpenAnotherTenantsDocument(t *testing.T) {
	dbURL := requireDB(t)
	srv := startServer(t, buildServer(t), freePort(t), dbURL, "-jwt-secret", testSecret)
	doc := fmt.Sprintf("shared-name-%d", time.Now().UnixNano())

	update := readFixture(t, "text-insert-single", "update-000.bin")
	want := crdt.NewDoc(9)
	if err := want.ApplyUpdate(update); err != nil {
		t.Fatal(err)
	}

	// acme creates it and writes to it.
	acme := dialRaw(t, srv.addr, doc+"?token="+tenantToken(t, doc, "acme"))
	acme.sync()
	acme.send(protocol.WriteUpdate(update))
	time.Sleep(300 * time.Millisecond)

	// globex knows the name and has a perfectly valid token for it. That is the
	// exact attack: without tenancy, a token naming a document is enough.
	globex := dialRaw(t, srv.addr, doc+"?token="+tenantToken(t, doc, "globex"))
	if reason := globex.expectDenied(t); reason == "" {
		t.Fatal("the refusal carried no reason")
	}

	// And the refusal says nothing that distinguishes "not yours" from "no such
	// document" - otherwise the boundary is a way to enumerate names.
	missing := dialRaw(t, srv.addr, "no-such-document-at-all?token="+
		tenantToken(t, "no-such-document-at-all", "globex"))
	missing.sync() // A name nobody owns is simply created, so this succeeds.

	// acme still has their document, unchanged.
	again := dialRaw(t, srv.addr, doc+"?token="+tenantToken(t, doc, "acme"))
	if got := again.sync(); textOf(t, got) != textOf(t, want) {
		t.Fatalf("the owner sees %q, want %q", textOf(t, got), textOf(t, want))
	}
}

// A token with no owner claim is what every token looked like before tenancy.
// It must not be a skeleton key, and it must not lose access to the documents
// it already had.
func TestATokenWithNoOwnerReachesOnlyUnownedDocuments(t *testing.T) {
	dbURL := requireDB(t)
	srv := startServer(t, buildServer(t), freePort(t), dbURL, "-jwt-secret", testSecret)
	run := time.Now().UnixNano()
	owned := fmt.Sprintf("owned-%d", run)
	unowned := fmt.Sprintf("unowned-%d", run)

	// A document created by a tenant.
	acme := dialRaw(t, srv.addr, owned+"?token="+tenantToken(t, owned, "acme"))
	acme.sync()

	// The old-style token cannot reach it.
	old := dialRaw(t, srv.addr, owned+"?token="+mintToken(t, owned, auth.PermissionWrite, time.Hour))
	if reason := old.expectDenied(t); reason == "" {
		t.Fatal("the refusal carried no reason")
	}

	// But it still has its own documents, which is what a deployment that has
	// not turned tenancy on has.
	plain := dialRaw(t, srv.addr, unowned+"?token="+mintToken(t, unowned, auth.PermissionWrite, time.Hour))
	plain.sync()

	// And a tenant does not inherit those either.
	claiming := dialRaw(t, srv.addr, unowned+"?token="+tenantToken(t, unowned, "acme"))
	if reason := claiming.expectDenied(t); reason == "" {
		t.Fatal("the refusal carried no reason")
	}
}

// The listing, which is what makes an admin UI or an account deletion possible.
func TestListingIsScopedToOneOwner(t *testing.T) {
	dbURL := requireDB(t)
	srv := startServer(t, buildServer(t), freePort(t), dbURL,
		"-jwt-secret", testSecret, "-admin-token", auditToken)
	run := time.Now().UnixNano()

	acmeDocs := []string{
		fmt.Sprintf("t%d-acme-alpha", run),
		fmt.Sprintf("t%d-acme-beta", run),
	}
	globexDoc := fmt.Sprintf("t%d-globex-gamma", run)
	for _, name := range acmeDocs {
		dialRaw(t, srv.addr, name+"?token="+tenantToken(t, name, "acme")).sync()
	}
	dialRaw(t, srv.addr, globexDoc+"?token="+tenantToken(t, globexDoc, "globex")).sync()
	time.Sleep(300 * time.Millisecond)

	list := fetchList(t, srv, "?owner=acme&limit=1000")
	names := map[string]bool{}
	for _, d := range list.Documents {
		names[d.Name] = true
	}
	for _, name := range acmeDocs {
		if !names[name] {
			t.Errorf("%q is missing from acme's listing", name)
		}
	}
	if names[globexDoc] {
		t.Error("another tenant's document is in acme's listing")
	}

	// The operator's view has everything.
	all := fetchList(t, srv, "?limit=1000")
	found := map[string]bool{}
	for _, d := range all.Documents {
		found[d.Name] = true
	}
	for _, name := range append(append([]string{}, acmeDocs...), globexDoc) {
		if !found[name] {
			t.Errorf("%q is missing from the unfiltered listing", name)
		}
	}

	// A listing carries what is needed to act on it: a name to open, an id, and
	// sizes to decide by. Never content.
	for _, d := range list.Documents {
		if d.ID == "" || d.OwnerID == "" || d.UpdatedAt == "" {
			t.Errorf("a listing entry is incomplete: %+v", d)
		}
	}
}

// Paging must cover every document exactly once, because it is what an account
// deletion iterates.
func TestListingPagesWithoutSkippingOrRepeating(t *testing.T) {
	dbURL := requireDB(t)
	srv := startServer(t, buildServer(t), freePort(t), dbURL,
		"-jwt-secret", testSecret, "-admin-token", auditToken)
	run := time.Now().UnixNano()

	const count = 7
	want := map[string]bool{}
	for i := range count {
		name := fmt.Sprintf("p%d-doc-%02d", run, i)
		want[name] = true
		dialRaw(t, srv.addr, name+"?token="+tenantToken(t, name, "pager")).sync()
	}
	time.Sleep(300 * time.Millisecond)

	seen := map[string]int{}
	query := "?owner=pager&limit=2"
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatal("paging did not terminate")
		}
		page := fetchList(t, srv, query)
		for _, d := range page.Documents {
			seen[d.Name]++
		}
		if page.Next == "" {
			break
		}
		query = "?owner=pager&limit=2&after=" + page.Next
	}
	for name := range want {
		if seen[name] != 1 {
			t.Errorf("%q appeared %d times across the pages, want once", name, seen[name])
		}
	}
}

// Turning tenancy on leaves the existing documents owned by nobody. Moving one
// to a tenant is an operator action, and it takes effect at once - including on
// the replica that had the document open.
func TestAnOperatorCanMoveADocumentToATenant(t *testing.T) {
	dbURL := requireDB(t)
	srv := startServer(t, buildServer(t), freePort(t), dbURL,
		"-jwt-secret", testSecret, "-admin-token", auditToken)
	doc := fmt.Sprintf("legacy-%d", time.Now().UnixNano())

	// A document from before tenancy: created by a token with no owner.
	before := dialRaw(t, srv.addr, doc+"?token="+mintToken(t, doc, auth.PermissionWrite, time.Hour))
	before.sync()
	before.send(protocol.WriteUpdate(readFixture(t, "text-insert-single", "update-000.bin")))
	time.Sleep(300 * time.Millisecond)

	// acme cannot reach it yet.
	blocked := dialRaw(t, srv.addr, doc+"?token="+tenantToken(t, doc, "acme"))
	blocked.expectDenied(t)

	// Moving a document somebody is editing is refused rather than done under
	// them, so the writer goes first. That refusal is worth pinning here: it is
	// what stops a connection ending up attached to a room whose owner no longer
	// matches its grant.
	if resp := adminDo(t, srv, http.MethodPut, "/documents/"+doc+"/owner", auditToken,
		strings.NewReader(`{"owner":"acme"}`)); resp.StatusCode != http.StatusConflict {
		t.Fatalf("moving a document that is in use returned %d, want 409", resp.StatusCode)
	}
	_ = before.ws.CloseNow()
	// The room lingers for -idle-timeout after the last client leaves, and Evict
	// is what ends it - so the retry below is the operation, not a workaround.
	time.Sleep(300 * time.Millisecond)

	// The operator says whose it is.
	resp := adminDo(t, srv, http.MethodPut, "/documents/"+doc+"/owner", auditToken,
		strings.NewReader(`{"owner":"acme"}`))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT owner returned %d\n%s", resp.StatusCode, srv.logs)
	}

	// Now acme can, and the content is still there.
	after := dialRaw(t, srv.addr, doc+"?token="+tenantToken(t, doc, "acme"))
	want := crdt.NewDoc(9)
	if err := want.ApplyUpdate(readFixture(t, "text-insert-single", "update-000.bin")); err != nil {
		t.Fatal(err)
	}
	if got := after.sync(); textOf(t, got) != textOf(t, want) {
		t.Fatalf("after the move the document reads %q, want %q", textOf(t, got), textOf(t, want))
	}

	// And the token that used to work does not any more.
	stale := dialRaw(t, srv.addr, doc+"?token="+mintToken(t, doc, auth.PermissionWrite, time.Hour))
	if reason := stale.expectDenied(t); reason == "" {
		t.Fatal("the refusal carried no reason")
	}

	// The move shows up in the listing under its new owner, with its name - a
	// document that was moved but stayed nameless would be invisible forever.
	list := fetchList(t, srv, "?owner=acme&limit=1000")
	for _, d := range list.Documents {
		if d.Name == doc {
			return
		}
	}
	t.Errorf("the moved document is not in its new owner's listing")
}

// A restore may name the owner, and is refused when it names the wrong one:
// writing one tenant's bytes into another's document should take more than a
// typo in a URL.
func TestARestoreWillNotCrossTenants(t *testing.T) {
	dbURL := requireDB(t)
	srv := startServer(t, buildServer(t), freePort(t), dbURL,
		"-jwt-secret", testSecret, "-admin-token", auditToken)
	doc := fmt.Sprintf("restore-%d", time.Now().UnixNano())

	dialRaw(t, srv.addr, doc+"?token="+tenantToken(t, doc, "acme")).sync()
	time.Sleep(300 * time.Millisecond)

	update := readFixture(t, "text-insert-single", "update-000.bin")
	resp := adminDo(t, srv, http.MethodPost, "/documents/"+doc+"?owner=globex", auditToken,
		bytesReader(update))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("a cross-tenant restore returned %d, want 409", resp.StatusCode)
	}
	// The right owner works.
	resp = adminDo(t, srv, http.MethodPost, "/documents/"+doc+"?owner=acme", auditToken, bytesReader(update))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("a restore by the owner returned %d\n%s", resp.StatusCode, srv.logs)
	}
}

// requireDB skips a test that needs PostgreSQL, rather than passing without
// having checked anything.
func requireDB(t *testing.T) string {
	t.Helper()
	dbURL := os.Getenv(dbEnv)
	if dbURL == "" {
		t.Skipf("%s is not set; start deploy/docker-compose.yml to run this", dbEnv)
	}
	return dbURL
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// fetchList reads GET /documents.
func fetchList(t *testing.T, srv *server, query string) documentListView {
	t.Helper()
	resp := adminDo(t, srv, http.MethodGet, "/documents"+query, auditToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /documents%s returned %d\n%s", query, resp.StatusCode, srv.logs)
	}
	var out documentListView
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding the listing: %v", err)
	}
	return out
}

type documentListView struct {
	Documents []struct {
		Name          string `json:"name"`
		ID            string `json:"id"`
		OwnerID       string `json:"owner_id"`
		Resident      bool   `json:"resident"`
		UpdatedAt     string `json:"updated_at"`
		SnapshotBytes int64  `json:"snapshot_bytes"`
		Updates       int64  `json:"updates"`
	} `json:"documents"`
	Next string `json:"next"`
}
