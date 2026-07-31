// Package audit records what was done to the documents on the administrative
// surface, and by which credential.
//
// It exists because that surface can read, overwrite and delete every document
// on the server, and until now the only record was a line in the process log
// next to everything else the server had to say. "Who deleted this document
// last Tuesday" is a question somebody eventually asks, and the answer has to be
// findable without reading a week of debug output.
//
// Three properties are deliberate.
//
// It is a stream of JSON objects, one per line, written to its own destination -
// stdout by default, where the process log is on stderr. That means the audit
// trail is already separated from the noise by the time anything ships it, with
// no configuration, and it lands in whatever the deployment already collects.
//
// It records attempts, not just successes. A request refused with 401 is the
// most interesting line in the file: it is somebody who does not have the
// credential trying to use the surface anyway.
//
// It never records document content. Names, byte counts and status codes only.
// An audit trail that carries what was written is a second copy of the data,
// with none of the protection the first copy has.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"sync"
	"time"
)

// An Event is one thing that happened on the administrative surface.
type Event struct {
	// At is when the request finished.
	At time.Time `json:"time"`
	// Action names what was attempted, in the vocabulary of the server rather
	// than of HTTP: document.delete rather than DELETE. The method and path are
	// carried too, but an operator reading this is looking for a verb.
	Action string `json:"action"`
	// Result is "ok" for a 2xx or 3xx, "denied" for a 401 or 403, "refused" for
	// any other 4xx, and "failed" for a 5xx. It is derived rather than given, so
	// two call sites cannot disagree about what a 409 means.
	Result string `json:"result"`
	Status int    `json:"status"`
	// Document is the name the request was about, empty when it was about none.
	Document string `json:"document,omitempty"`
	// Credential fingerprints the token the request presented: the first eight
	// hex digits of its SHA-256. It is not the token and cannot be turned back
	// into one.
	//
	// With one shared -admin-token this says which credential, not which person,
	// and that is the honest limit of what the server knows. It earns its place
	// when there is more than one token in flight - during a rotation, or
	// between a backup script and an operator - because it is the only way to
	// find out whether the old one is still being used before it is removed.
	Credential string `json:"credential"`
	IP         string `json:"ip,omitempty"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	// Bytes is the size of the response body for a read, or of the request body
	// for a write. It is a size, never content.
	Bytes      int64 `json:"bytes"`
	DurationMS int64 `json:"duration_ms"`
	// Err is the server's own explanation when something failed. It is the text
	// already written to the client, so it says nothing the caller was not told.
	Err string `json:"error,omitempty"`
}

// Credentials that are not a token.
const (
	// CredentialNone is what an unauthenticated administrative surface records.
	// It is a value rather than an empty field so that "this server had no
	// -admin-token" is something the log states rather than something a reader
	// infers from an absence.
	CredentialNone = "none"
	// CredentialBad is recorded for a request whose token did not match. The
	// fingerprint of what was presented is deliberately not written: it would be
	// an oracle, letting somebody who can read the audit log confirm guesses
	// against it.
	CredentialBad = "invalid"
)

// A Recorder writes events. The zero value is not usable; use New or Discard.
//
// It is safe for concurrent use and writes synchronously, under a lock. That is
// the right trade here and not laziness: the administrative surface serves an
// operator, a backup script and a scrape job, so the write rate is low, and an
// audit record dropped to keep a request fast is the one record somebody will
// go looking for.
type Recorder struct {
	mu  sync.Mutex
	enc *json.Encoder
	out io.Writer
	// closer is non-nil when the Recorder opened the destination itself and is
	// therefore the thing that has to close it.
	closer io.Closer
	// now is injectable so a test can assert on a timestamp.
	now func() time.Time
}

// New returns a Recorder writing one JSON object per line to out. If out also
// implements io.Closer and owned is true, Close closes it.
func New(out io.Writer, owned bool) *Recorder {
	r := &Recorder{
		enc: json.NewEncoder(out),
		out: out,
		now: time.Now,
	}
	if c, ok := out.(io.Closer); ok && owned {
		r.closer = c
	}
	return r
}

// Discard returns a Recorder that writes nowhere, for a server started with
// auditing off and for tests that are not about auditing. Recording into it is
// a lock and a return, so no call site has to check for nil.
func Discard() *Recorder { return New(io.Discard, false) }

// Record writes one event. A destination that cannot be written to is not
// reported: there is nowhere left to report it that is not the thing that just
// failed, and an administrative request must not fail because its audit line
// did.
//
// That is a real limitation and worth stating rather than hiding: if the audit
// destination is a file on a full disk, the surface keeps working and the trail
// stops. Sending the trail to stdout - the default - makes this the deployment's
// problem rather than a silent one, because a container's stdout does not fill
// up in the way a path inside it does.
func (r *Recorder) Record(e Event) {
	if e.At.IsZero() {
		e.At = r.now()
	}
	if e.Result == "" {
		e.Result = resultOf(e.Status)
	}
	if e.Credential == "" {
		e.Credential = CredentialNone
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.enc.Encode(e)
}

// Close releases the destination if this Recorder opened it.
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closer == nil {
		return nil
	}
	return r.closer.Close()
}

// Fingerprint names a credential without revealing it: the first eight hex
// digits of its SHA-256.
//
// Four bytes is short on purpose. This goes in a log that is shipped somewhere,
// and it only has to tell a handful of tokens apart during a rotation - not
// resist somebody who has both the log and a guess. A longer prefix would buy no
// more distinguishing power for that job and would offer more to work with for
// the other one.
func Fingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:4])
}

// resultOf turns a status code into the word an operator greps for.
func resultOf(status int) string {
	switch {
	case status >= 500:
		return "failed"
	case status == 401 || status == 403:
		return "denied"
	case status >= 400:
		return "refused"
	default:
		return "ok"
	}
}
