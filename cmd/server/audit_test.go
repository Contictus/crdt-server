package main_test

// The audit trail, against a real server process.
//
// Every assertion here reads the process's actual stdout, because the point of
// the feature is what a deployment collects - not what a function returned.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mesutokul/ycollab/internal/protocol"
)

// record is one line of the trail.
type record struct {
	Time       string `json:"time"`
	Action     string `json:"action"`
	Result     string `json:"result"`
	Status     int    `json:"status"`
	Document   string `json:"document"`
	Credential string `json:"credential"`
	IP         string `json:"ip"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Bytes      int64  `json:"bytes"`
	DurationMS int64  `json:"duration_ms"`
	Err        string `json:"error"`
}

// trail parses whatever the server has written to stdout so far.
func trail(t *testing.T, s *server) []record {
	t.Helper()
	return parseTrail(t, s.audit.String())
}

func parseTrail(t *testing.T, out string) []record {
	t.Helper()
	var records []record
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var r record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("a line of the audit trail is not JSON: %v\n%s", err, line)
		}
		records = append(records, r)
	}
	return records
}

// only returns the records matching an action, which is how an operator reads
// this file.
func only(records []record, action string) []record {
	var kept []record
	for _, r := range records {
		if r.Action == action {
			kept = append(kept, r)
		}
	}
	return kept
}

// waitForTrail polls until the trail has at least n records, because the
// process writes them and the test reads them across a pipe.
func waitForTrail(t *testing.T, s *server, n int) []record {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		records := trail(t, s)
		if len(records) >= n {
			return records
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d audit records after 10s, want %d\n%s", len(records), n, s.audit.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// adminDo makes an administrative request with an optional token.
func adminDo(t *testing.T, s *server, method, path, token string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, "http://"+s.admin+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _, _ = io.Copy(io.Discard, resp.Body); resp.Body.Close() })
	return resp
}

const auditToken = "an-admin-token-for-the-audit-tests"

// The whole point: read, overwrite and delete a document and be able to say
// afterwards that it happened, to which document, and with which credential.
func TestTheTrailNamesWhatWasDoneToWhichDocument(t *testing.T) {
	srv := startServer(t, buildServer(t), freePort(t), "", "-admin-token", auditToken)
	doc := fmt.Sprintf("audited-%d", time.Now().UnixNano())

	// Put something there over the WebSocket, which is deliberately not audited:
	// the trail is about the administrative surface, not about editing.
	c := dial(t, srv.addr, doc)
	c.sync()
	c.send(protocol.WriteUpdate(readFixture(t, "text-insert-single", "update-000.bin")))
	time.Sleep(200 * time.Millisecond)

	adminDo(t, srv, http.MethodGet, "/documents/"+doc, auditToken, nil)
	adminDo(t, srv, http.MethodPost, "/documents/"+doc, auditToken,
		strings.NewReader(string(readFixture(t, "text-three-client-interleaved", "update-001.bin"))))

	records := waitForTrail(t, srv, 2)

	reads := only(records, "document.read")
	if len(reads) != 1 {
		t.Fatalf("%d document.read records, want 1\n%s", len(reads), srv.audit.String())
	}
	r := reads[0]
	if r.Document != doc {
		t.Errorf("document = %q, want %q", r.Document, doc)
	}
	if r.Result != "ok" || r.Status != http.StatusOK {
		t.Errorf("result = %q status = %d", r.Result, r.Status)
	}
	if r.Method != http.MethodGet || r.Path != "/documents/"+doc {
		t.Errorf("method = %q path = %q", r.Method, r.Path)
	}
	if r.Bytes <= 0 {
		t.Errorf("bytes = %d; a read that returned a document recorded no size", r.Bytes)
	}
	if r.IP == "" {
		t.Error("no client address")
	}
	if r.Time == "" {
		t.Error("no timestamp")
	}

	writes := only(records, "document.write")
	if len(writes) != 1 {
		t.Fatalf("%d document.write records, want 1", len(writes))
	}
	if writes[0].Document != doc || writes[0].Bytes <= 0 {
		t.Errorf("write recorded as %+v", writes[0])
	}
}

// A refused request is the most interesting line in the file, so it has to be
// there - and it must not record the token that was tried, which would make the
// trail an oracle for guessing it.
func TestARefusedRequestIsRecordedWithoutTheTokenItTried(t *testing.T) {
	srv := startServer(t, buildServer(t), freePort(t), "", "-admin-token", auditToken)

	const guess = "a-token-somebody-guessed"
	resp := adminDo(t, srv, http.MethodDelete, "/documents/anything", guess, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
	// A request with no credential at all, and a probe of the metrics endpoint,
	// which is the one route not audited when it succeeds.
	adminDo(t, srv, http.MethodGet, "/documents/anything", "", nil)
	adminDo(t, srv, http.MethodGet, "/metrics", guess, nil)

	records := waitForTrail(t, srv, 3)
	denied := 0
	for _, r := range records {
		if r.Result != "denied" {
			continue
		}
		denied++
		if r.Status != http.StatusUnauthorized {
			t.Errorf("a denial recorded status %d", r.Status)
		}
		if r.Credential != "invalid" {
			t.Errorf("credential = %q, want the refusal to name no credential", r.Credential)
		}
	}
	if denied != 3 {
		t.Fatalf("%d denials recorded, want 3\n%s", denied, srv.audit.String())
	}

	// And nothing anywhere in the trail carries the token or any prefix of it.
	out := srv.audit.String()
	if strings.Contains(out, guess) {
		t.Error("the trail carries the token that was tried")
	}
	for n := 6; n <= len(guess); n++ {
		if strings.Contains(out, guess[:n]) {
			t.Errorf("the trail carries a %d-character prefix of the token that was tried", n)
		}
	}
}

// With more than one token in flight - which is what a rotation is - the trail
// has to say which one was used, or there is no safe moment to remove the old
// one.
func TestTheTrailTellsTwoCredentialsApart(t *testing.T) {
	const oldToken, newToken = "the-old-admin-token-being-retired", "the-new-admin-token-taking-over"
	srv := startServer(t, buildServer(t), freePort(t), "", "-admin-token", oldToken+","+newToken)
	doc := fmt.Sprintf("rotating-%d", time.Now().UnixNano())

	// Both work, which is the point of accepting two at once. The document does
	// not exist on a server with no database, so the interesting part of the
	// answer is that neither request was the 401 a wrong token gets.
	for _, token := range []string{oldToken, newToken} {
		if resp := adminDo(t, srv, http.MethodGet, "/documents/"+doc, token, nil); resp.StatusCode == http.StatusUnauthorized {
			t.Fatalf("a token that should work was refused with %d", resp.StatusCode)
		}
	}

	records := waitForTrail(t, srv, 2)
	seen := map[string]bool{}
	for _, r := range only(records, "document.read") {
		if r.Credential == "" || r.Credential == "none" || r.Credential == "invalid" {
			t.Errorf("an authorised read recorded credential %q", r.Credential)
		}
		seen[r.Credential] = true
	}
	if len(seen) != 2 {
		t.Fatalf("%d distinct credentials in the trail, want 2: %v", len(seen), seen)
	}

	out := srv.audit.String()
	for _, token := range []string{oldToken, newToken} {
		if strings.Contains(out, token) {
			t.Error("the trail carries a token verbatim")
		}
	}
}

// A heap profile is a copy of every document this process is holding, which
// makes pprof the least obvious way to read the documents off this surface and
// the one most worth having a record of.
func TestReadingAProfileIsAudited(t *testing.T) {
	srv := startServer(t, buildServer(t), freePort(t), "", "-admin-token", auditToken)

	adminDo(t, srv, http.MethodGet, "/debug/pprof/heap?debug=1", auditToken, nil)
	records := waitForTrail(t, srv, 1)
	if len(only(records, "profile.read")) == 0 {
		t.Fatalf("reading a heap profile left no record\n%s", srv.audit.String())
	}
}

// A failure says what went wrong, in the words the caller was already given.
func TestAFailureRecordsItsReason(t *testing.T) {
	srv := startServer(t, buildServer(t), freePort(t), "", "-admin-token", auditToken)

	// An update that says nothing is refused with a sentence, and that sentence
	// is what turns "status 400" in the trail into something readable.
	// Two zero varints: a structurally valid Yjs update that adds nothing.
	empty := []byte{0x00, 0x00}
	resp := adminDo(t, srv, http.MethodPost, "/documents/nothing", auditToken, bytes.NewReader(empty))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
	records := waitForTrail(t, srv, 1)
	last := records[len(records)-1]
	if last.Result != "refused" {
		t.Fatalf("a %d was recorded as %q", last.Status, last.Result)
	}
	if last.Err == "" {
		t.Errorf("a failure recorded no reason: %+v", last)
	}
}

// A path this server does not route is still worth a record: an authorised
// caller looking around in here is the thing a trail exists to show.
func TestAnUnroutedPathIsStillRecorded(t *testing.T) {
	srv := startServer(t, buildServer(t), freePort(t), "", "-admin-token", auditToken)

	resp := adminDo(t, srv, http.MethodGet, "/there-is-no-such-endpoint", auditToken, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404", resp.StatusCode)
	}
	records := waitForTrail(t, srv, 1)
	last := records[len(records)-1]
	if last.Action != "unknown" || last.Path != "/there-is-no-such-endpoint" {
		t.Errorf("recorded as %+v", last)
	}
	if last.Credential == "none" || last.Credential == "invalid" {
		t.Errorf("credential = %q; the caller was authorised", last.Credential)
	}
}

// The trail is a separate stream from the process log. That separation is what
// makes it usable with no configuration, so it is worth pinning: nothing the
// server says about itself may land in the trail, and no record may land in the
// log.
func TestTheTrailIsNotMixedIntoTheProcessLog(t *testing.T) {
	srv := startServer(t, buildServer(t), freePort(t), "", "-admin-token", auditToken)
	adminDo(t, srv, http.MethodGet, "/documents/anything", auditToken, nil)
	waitForTrail(t, srv, 1)

	// Every line of stdout parses as a record - parseTrail fails otherwise.
	if len(parseTrail(t, srv.audit.String())) == 0 {
		t.Fatal("nothing on stdout")
	}
	// And the process log, which is full of the server's own lines, has none.
	if strings.Contains(srv.logs.String(), `"action":`) {
		t.Errorf("an audit record went to the process log:\n%s", srv.logs)
	}
}

// A path instead of stdout, appended rather than truncated: a restart must not
// be a way to lose the trail.
func TestAFileDestinationIsAppendedTo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	binary := buildServer(t)

	for range 2 {
		srv := startServer(t, binary, freePort(t), "", "-admin-token", auditToken, "-audit-log", path)
		adminDo(t, srv, http.MethodGet, "/documents/anything", auditToken, nil)
		// The record is written before the response is, so the file has it by
		// the time the request has returned.
		srv.kill()
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	records := parseTrail(t, string(body))
	if len(records) != 2 {
		t.Fatalf("%d records after two runs, want 2 - the second run truncated the first\n%s", len(records), body)
	}
	// Nothing on stdout, because the trail went to the file.
	if strings.Contains(string(body), "level=INFO") {
		t.Error("the process log went into the audit file")
	}
}

// Auditing can be turned off, and the server says so rather than going quiet.
func TestAuditingCanBeTurnedOff(t *testing.T) {
	srv := startServer(t, buildServer(t), freePort(t), "", "-admin-token", auditToken, "-audit-log", "")
	adminDo(t, srv, http.MethodGet, "/documents/anything", auditToken, nil)
	time.Sleep(300 * time.Millisecond)

	if out := strings.TrimSpace(srv.audit.String()); out != "" {
		t.Errorf("something was recorded with -audit-log empty:\n%s", out)
	}
	if !strings.Contains(srv.logs.String(), "no -audit-log") {
		t.Error("the server did not say the trail is off")
	}
}
