package main_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/mesutokul/ycollab/internal/protocol"
)

// The whole reason version history exists: somebody pasted over the document
// and wants what it said before. This is that story end to end, against a real
// server and a real database - take a version, wreck the document, find the
// version, put it back.
func TestAVersionSurvivesSomebodyWreckingTheDocument(t *testing.T) {
	dbURL := os.Getenv(dbEnv)
	if dbURL == "" {
		t.Skipf("%s is not set; start deploy/docker-compose.yml to run this", dbEnv)
	}
	srv := startServer(t, buildServer(t), freePort(t), dbURL)
	doc := fmt.Sprintf("history-%d", time.Now().UnixNano())

	// The document as it should be.
	author := dial(t, srv.addr, doc)
	author.sync()
	// The first half is the document worth keeping; the second half is what
	// somebody does to it afterwards. Two halves of one scenario rather than
	// two scenarios, because an update from an unrelated fixture can carry a
	// client id and clock range this document already has, and then applying it
	// changes nothing - which is how the first version of this test managed to
	// "wreck" a document without altering a byte.
	scenario := scenarioUpdatesFor(t, "text-three-client-interleaved")
	good, damage := scenario[:len(scenario)/2], scenario[len(scenario)/2:]
	for _, u := range good {
		author.send(protocol.WriteUpdate(u))
	}
	author.send(protocol.WriteSyncStep1(emptyStateVector(t)))
	author.recv()
	wanted := textOfUpdate(t, mustGet(t, srv, "/documents/"+doc))

	// Somebody takes a version before doing something risky.
	if code := adminPostQuery(t, srv, "/documents/"+doc+"/versions?label=before+the+migration"); code != http.StatusCreated {
		t.Fatalf("taking a version returned %d, want 201", code)
	}

	// And then wrecks it.
	for _, u := range damage {
		author.send(protocol.WriteUpdate(u))
	}
	author.send(protocol.WriteSyncStep1(emptyStateVector(t)))
	author.recv()
	wrecked := textOfUpdate(t, mustGet(t, srv, "/documents/"+doc))
	if wrecked == wanted {
		t.Fatal("the document did not change, so this test proves nothing")
	}

	// Find the version. The listing is what somebody actually looks at, so it
	// has to carry enough to choose from: when, how big, and the label.
	list := listVersionsOf(t, srv, doc)
	if len(list.Versions) == 0 {
		t.Fatal("no versions were recorded")
	}
	var found *versionRow
	for i, v := range list.Versions {
		if v.Label == "before the migration" {
			found = &list.Versions[i]
		}
	}
	if found == nil {
		t.Fatalf("the labelled version is not in the listing: %+v", list.Versions)
	}
	if found.Bytes == 0 || found.CreatedAt == "" || found.StateVector == "" {
		t.Errorf("the listing entry is missing something: %+v", *found)
	}
	if _, err := time.Parse(time.RFC3339Nano, found.CreatedAt); err != nil {
		t.Errorf("created_at is %q: %v", found.CreatedAt, err)
	}

	// Read it. It is a Yjs update, the same form the document read API returns,
	// so one piece of client code opens both.
	body := mustGet(t, srv, fmt.Sprintf("/documents/%s/versions/%d", doc, found.ID))
	if got := textOfUpdate(t, body); got != wanted {
		t.Fatalf("the version reads %q, want %q", got, wanted)
	}

	// Put it back. DELETE first, because a merge cannot remove what the
	// document has since gained - restoring without it would give the union of
	// the good version and the damage.
	author.disconnect()
	waitForDelete(t, srv, doc)
	if code := post(t, srv, "/documents/"+doc, body); code != http.StatusNoContent {
		t.Fatalf("the restore returned %d", code)
	}
	if got := textOfUpdate(t, mustGet(t, srv, "/documents/"+doc)); got != wanted {
		t.Errorf("after the restore the document reads %q, want %q", got, wanted)
	}
	// And to a client, which is who noticed.
	if got := textOf(t, dial(t, srv.addr, doc).sync()); got != wanted {
		t.Errorf("a reconnecting client sees %q, want %q", got, wanted)
	}
}

// Each version is a whole document, so a timer with no bound is unbounded
// storage. This drives the timer for real and checks both halves: versions
// appear while the document changes, and the count stops where it was told to.
func TestTheTimerTakesVersionsAndKeepsOnlyWhatItShould(t *testing.T) {
	dbURL := os.Getenv(dbEnv)
	if dbURL == "" {
		t.Skipf("%s is not set; start deploy/docker-compose.yml to run this", dbEnv)
	}
	srv := startServer(t, buildServer(t), freePort(t), dbURL,
		"-version-interval", "50ms", "-version-keep", "3", "-tick", "50ms")
	doc := fmt.Sprintf("history-timer-%d", time.Now().UnixNano())

	c := dial(t, srv.addr, doc)
	c.sync()
	// Six distinct edits, spread out enough that the interval elapses between
	// them, so each one is a version the store has a reason to keep.
	for _, u := range scenarioUpdatesFor(t, "text-three-client-interleaved")[:6] {
		c.send(protocol.WriteUpdate(u))
		time.Sleep(120 * time.Millisecond)
	}

	deadline := time.Now().Add(20 * time.Second)
	var list versionListBody
	for {
		list = listVersionsOf(t, srv, doc)
		if len(list.Versions) >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d versions after 20s\n%s", len(list.Versions), srv.logs)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// -version-keep 3 means three, however many were taken.
	if len(list.Versions) != 3 {
		t.Errorf("%d versions are kept, want 3", len(list.Versions))
	}
	// Newest first, which is the order somebody looking for "an hour ago" reads.
	for i := 1; i < len(list.Versions); i++ {
		if list.Versions[i-1].ID <= list.Versions[i].ID {
			t.Errorf("the listing is not newest-first: %d then %d",
				list.Versions[i-1].ID, list.Versions[i].ID)
		}
	}
	// The metric exists, because a history nobody can see stopping is a history
	// nobody notices stopping.
	if v := scrape(t, srv)["ycollab_versions_total"]; v < 3 {
		t.Errorf("ycollab_versions_total is %v", v)
	}
}

// Asking for a version of an unchanged document twice must not fill the history
// with copies of the same thing.
func TestTakingTheSameVersionTwiceStoresOne(t *testing.T) {
	dbURL := os.Getenv(dbEnv)
	if dbURL == "" {
		t.Skipf("%s is not set; start deploy/docker-compose.yml to run this", dbEnv)
	}
	srv := startServer(t, buildServer(t), freePort(t), dbURL)
	doc := fmt.Sprintf("history-dup-%d", time.Now().UnixNano())

	c := dial(t, srv.addr, doc)
	c.sync()
	c.send(protocol.WriteUpdate(readFixture(t, "text-insert-single", "update-000.bin")))
	c.send(protocol.WriteSyncStep1(emptyStateVector(t)))
	c.recv()

	if code := adminPostQuery(t, srv, "/documents/"+doc+"/versions"); code != http.StatusCreated {
		t.Fatalf("the first version returned %d, want 201", code)
	}
	// 200 rather than 201: it succeeded, and nothing new exists.
	if code := adminPostQuery(t, srv, "/documents/"+doc+"/versions"); code != http.StatusOK {
		t.Fatalf("the second version returned %d, want 200", code)
	}
	if list := listVersionsOf(t, srv, doc); len(list.Versions) != 1 {
		t.Errorf("%d versions stored, want 1", len(list.Versions))
	}
}

// A version id that belongs to another document, or to nothing, is a 404 - not
// somebody else's document.
func TestAskingForAVersionThatIsNotThere(t *testing.T) {
	dbURL := os.Getenv(dbEnv)
	if dbURL == "" {
		t.Skipf("%s is not set; start deploy/docker-compose.yml to run this", dbEnv)
	}
	srv := startServer(t, buildServer(t), freePort(t), dbURL)
	doc := fmt.Sprintf("history-404-%d", time.Now().UnixNano())
	other := fmt.Sprintf("history-other-%d", time.Now().UnixNano())

	c := dial(t, srv.addr, other)
	c.sync()
	c.send(protocol.WriteUpdate(readFixture(t, "text-insert-single", "update-000.bin")))
	c.send(protocol.WriteSyncStep1(emptyStateVector(t)))
	c.recv()
	if code := adminPostQuery(t, srv, "/documents/"+other+"/versions"); code != http.StatusCreated {
		t.Fatalf("setup: %d", code)
	}
	mine := listVersionsOf(t, srv, other)
	if len(mine.Versions) != 1 {
		t.Fatalf("setup: %d versions", len(mine.Versions))
	}

	// Another document's version id.
	if resp, _ := get(t, srv, fmt.Sprintf("/documents/%s/versions/%d", doc, mine.Versions[0].ID), nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("reading another document's version returned %d, want 404", resp.StatusCode)
	}
	// A number that is not a version at all.
	if resp, _ := get(t, srv, "/documents/"+doc+"/versions/999999999", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a missing version returned %d, want 404", resp.StatusCode)
	}
	// Not a number.
	if resp, _ := get(t, srv, "/documents/"+doc+"/versions/latest", nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a non-numeric version returned %d, want 400", resp.StatusCode)
	}
	// A document with no history lists an empty history rather than failing.
	if list := listVersionsOf(t, srv, doc); len(list.Versions) != 0 {
		t.Errorf("a document with no history listed %d versions", len(list.Versions))
	}
}

type versionListBody struct {
	Document string       `json:"document"`
	Versions []versionRow `json:"versions"`
}

type versionRow struct {
	ID          int64  `json:"id"`
	CreatedAt   string `json:"created_at"`
	StateVector string `json:"state_vector"`
	Label       string `json:"label"`
	Bytes       int    `json:"bytes"`
}

func listVersionsOf(t *testing.T, s *server, doc string) versionListBody {
	t.Helper()
	resp, body := get(t, s, "/documents/"+doc+"/versions", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("listing versions returned %d: %s", resp.StatusCode, body)
	}
	var out versionListBody
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("the listing is not JSON: %v\n%s", err, body)
	}
	return out
}

func mustGet(t *testing.T, s *server, path string) []byte {
	t.Helper()
	resp, body := get(t, s, path, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s returned %d: %s", path, resp.StatusCode, body)
	}
	return body
}

// adminPostQuery posts with no body, which is what taking a version is.
func adminPostQuery(t *testing.T, s *server, path string) int {
	t.Helper()
	resp, err := http.Post("http://"+s.admin+path, "application/json", nil)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
