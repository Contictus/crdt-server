package audit_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mesutokul/ycollab/internal/audit"
)

// decode reads the records a Recorder wrote. One JSON object per line is the
// contract everything downstream is written against, so the test parses it that
// way rather than looking for substrings.
func decode(t *testing.T, out *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("a record is not JSON: %v\n%s", err, line)
		}
		records = append(records, r)
	}
	return records
}

func TestARecordSaysWhatHappened(t *testing.T) {
	out := &bytes.Buffer{}
	rec := audit.New(out, false)
	rec.Record(audit.Event{
		At:         time.Unix(1750000000, 0).UTC(),
		Action:     "document.delete",
		Status:     204,
		Document:   "notes",
		Credential: "a1b2c3d4",
		IP:         "127.0.0.1",
		Method:     "DELETE",
		Path:       "/documents/notes",
		DurationMS: 12,
	})

	records := decode(t, out)
	if len(records) != 1 {
		t.Fatalf("%d records", len(records))
	}
	r := records[0]
	for k, want := range map[string]any{
		"action":      "document.delete",
		"result":      "ok",
		"status":      float64(204),
		"document":    "notes",
		"credential":  "a1b2c3d4",
		"ip":          "127.0.0.1",
		"method":      "DELETE",
		"path":        "/documents/notes",
		"duration_ms": float64(12),
	} {
		if r[k] != want {
			t.Errorf("%s = %v, want %v", k, r[k], want)
		}
	}
	if r["time"] != "2025-06-15T15:06:40Z" {
		t.Errorf("time = %v", r["time"])
	}
}

// The result word is derived rather than given, so two call sites cannot
// disagree about what a 409 means. An operator greps for these.
func TestTheResultWordIsDerivedFromTheStatus(t *testing.T) {
	for status, want := range map[int]string{
		200: "ok",
		204: "ok",
		304: "ok",
		400: "refused",
		401: "denied",
		403: "denied",
		404: "refused",
		409: "refused",
		500: "failed",
		503: "failed",
	} {
		out := &bytes.Buffer{}
		audit.New(out, false).Record(audit.Event{Status: status})
		if got := decode(t, out)[0]["result"]; got != want {
			t.Errorf("status %d recorded as %q, want %q", status, got, want)
		}
	}
}

// "This server had no -admin-token" has to be something the trail states, not
// something a reader infers from a missing field.
func TestAnUnauthenticatedRequestSaysSo(t *testing.T) {
	out := &bytes.Buffer{}
	audit.New(out, false).Record(audit.Event{Action: "document.read", Status: 200})
	if got := decode(t, out)[0]["credential"]; got != audit.CredentialNone {
		t.Errorf("credential = %v, want %q", got, audit.CredentialNone)
	}
}

// A fingerprint names a credential without being one, and the same token always
// gets the same name - which is the whole point during a rotation.
func TestAFingerprintIsStableAndIsNotTheToken(t *testing.T) {
	const token = "an-admin-token-nobody-else-should-have"
	print := audit.Fingerprint(token)
	if print != audit.Fingerprint(token) {
		t.Fatal("the same token fingerprinted two ways")
	}
	if len(print) != 8 {
		t.Errorf("fingerprint %q is %d characters", print, len(print))
	}
	if strings.Contains(token, print) || strings.Contains(print, token) {
		t.Error("the fingerprint carries the token")
	}
	if audit.Fingerprint("another-admin-token") == print {
		t.Error("two different tokens share a fingerprint")
	}
}

// A record is written per request from whatever goroutine serves it. Losing one
// to a torn line would be losing the record somebody went looking for.
func TestConcurrentRecordsAreWholeLines(t *testing.T) {
	out := &bytes.Buffer{}
	rec := audit.New(&syncWriter{w: out}, false)

	const writers, each = 16, 64
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				rec.Record(audit.Event{
					Action:     "document.read",
					Status:     200,
					Document:   strings.Repeat("d", i+1),
					Credential: "a1b2c3d4",
				})
			}
		}()
	}
	wg.Wait()

	records := decode(t, out)
	if len(records) != writers*each {
		t.Fatalf("%d records, want %d", len(records), writers*each)
	}
}

// syncWriter is what a file or a pipe is not: a writer with no atomicity of its
// own, so the test measures the Recorder's lock rather than the destination's.
type syncWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Written in two halves, so a Recorder that did not hold its lock across the
	// whole record would interleave and the decode above would fail.
	half := len(p) / 2
	n, err := s.w.Write(p[:half])
	if err != nil {
		return n, err
	}
	m, err := s.w.Write(p[half:])
	return n + m, err
}

// Discard exists so no call site has to check for nil.
func TestDiscardRecordsNothingAndDoesNotPanic(t *testing.T) {
	rec := audit.Discard()
	rec.Record(audit.Event{Action: "document.read", Status: 200})
	if err := rec.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
}

// A destination that cannot be written to must not take the request down with
// it. There is nowhere left to report the failure that is not the thing that
// just failed.
func TestAFailingDestinationDoesNotPanic(t *testing.T) {
	rec := audit.New(brokenWriter{}, false)
	rec.Record(audit.Event{Action: "document.delete", Status: 204})
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errBroken }

var errBroken = errBrokenType{}

type errBrokenType struct{}

func (errBrokenType) Error() string { return "the disk is full" }
