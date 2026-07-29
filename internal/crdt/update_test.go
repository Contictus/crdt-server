package crdt_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mesutokul/ycollab/internal/crdt"
)

const fixturesDir = "../../testdata/fixtures"

// expectedState is the part of a fixture's expected.json this file checks.
type expectedState struct {
	Scenario string `json:"scenario"`
	State    struct {
		StateVector map[string]uint64   `json:"stateVector"`
		DeleteSet   map[string][][2]int `json:"deleteSet"`
		StructCount int                 `json:"structCount"`
	} `json:"state"`
}

func scenarioDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.FromSlash(fixturesDir))
	if err != nil {
		t.Fatalf("read fixtures: %v (run `npm run generate` in tools/fixturegen)", err)
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(filepath.FromSlash(fixturesDir), e.Name())
		if _, err := os.Stat(filepath.Join(dir, "state.bin")); err != nil {
			continue // lib0/awareness fixtures hold no document state
		}
		dirs = append(dirs, dir)
	}
	if len(dirs) == 0 {
		t.Fatal("no document fixtures found")
	}
	return dirs
}

// updateFiles returns every file in dir that holds a bare v1 update.
func updateFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		switch {
		case name == "state.bin", name == "diff.bin", strings.HasPrefix(name, "update-"):
			files = append(files, filepath.Join(dir, name))
		}
	}
	return files
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// Every update Yjs produced must decode, and re-encoding it must reproduce the
// original bytes exactly. This is the whole wire-compatibility claim in one
// test: any misread field shifts the stream and shows up as a byte diff.
func TestUpdateRoundTripIsByteIdentical(t *testing.T) {
	checked := 0
	for _, dir := range scenarioDirs(t) {
		for _, path := range updateFiles(t, dir) {
			name := filepath.Base(dir) + "/" + filepath.Base(path)
			in := readFixture(t, path)
			u, err := crdt.DecodeUpdate(in)
			if err != nil {
				t.Errorf("%s: decode: %v", name, err)
				continue
			}
			got, err := u.Encode()
			if err != nil {
				t.Errorf("%s: encode: %v", name, err)
				continue
			}
			if !bytes.Equal(got, in) {
				t.Errorf("%s: re-encode differs\n got %x\nwant %x", name, got, in)
				continue
			}
			checked++
		}
	}
	if checked < 50 {
		t.Errorf("only %d updates checked; fixtures look incomplete", checked)
	}
	t.Logf("round-tripped %d updates", checked)
}

func TestStateVectorRoundTrip(t *testing.T) {
	for _, dir := range scenarioDirs(t) {
		for _, base := range []string{"sv.bin", "diff-sv.bin"} {
			path := filepath.Join(dir, base)
			if _, err := os.Stat(path); err != nil {
				continue
			}
			name := filepath.Base(dir) + "/" + base
			in := readFixture(t, path)
			sv, err := crdt.DecodeStateVector(in)
			if err != nil {
				t.Errorf("%s: decode: %v", name, err)
				continue
			}
			got, err := crdt.EncodeStateVector(sv)
			if err != nil {
				t.Errorf("%s: encode: %v", name, err)
				continue
			}
			if !bytes.Equal(got, in) {
				t.Errorf("%s: re-encode = %x, want %x", name, got, in)
			}
		}
	}
}

// The state vector Yjs reports for a document must equal the one implied by its
// full state update: every client's clock is one past its last struct.
func TestUpdateStateVectorMatchesFixture(t *testing.T) {
	for _, dir := range scenarioDirs(t) {
		name := filepath.Base(dir)
		var exp expectedState
		raw := readFixture(t, filepath.Join(dir, "expected.json"))
		if err := json.Unmarshal(raw, &exp); err != nil {
			t.Fatalf("%s: parse expected.json: %v", name, err)
		}
		u, err := crdt.DecodeUpdate(readFixture(t, filepath.Join(dir, "state.bin")))
		if err != nil {
			t.Errorf("%s: decode state.bin: %v", name, err)
			continue
		}
		got := u.StateVector()
		if len(got) != len(exp.State.StateVector) {
			t.Errorf("%s: state vector has %d clients, want %d", name, len(got), len(exp.State.StateVector))
		}
		for client, clock := range exp.State.StateVector {
			id, err := strconv.ParseUint(client, 10, 64)
			if err != nil {
				t.Fatalf("%s: bad client id %q: %v", name, client, err)
			}
			if g := got[crdt.ClientID(id)]; uint64(g) != clock {
				t.Errorf("%s: clock for client %s = %d, want %d", name, client, g, clock)
			}
		}

		structs := 0
		for _, block := range u.Clients {
			structs += len(block.Structs)
		}
		if exp.State.StructCount != 0 && structs != exp.State.StructCount {
			t.Errorf("%s: decoded %d structs, want %d", name, structs, exp.State.StructCount)
		}
	}
}

// The delete set carried by state.bin must match what Yjs reports.
func TestDeleteSetMatchesFixture(t *testing.T) {
	for _, dir := range scenarioDirs(t) {
		name := filepath.Base(dir)
		var exp expectedState
		if err := json.Unmarshal(readFixture(t, filepath.Join(dir, "expected.json")), &exp); err != nil {
			t.Fatalf("%s: parse expected.json: %v", name, err)
		}
		u, err := crdt.DecodeUpdate(readFixture(t, filepath.Join(dir, "state.bin")))
		if err != nil {
			t.Errorf("%s: decode state.bin: %v", name, err)
			continue
		}
		if len(u.Deletes.Clients) != len(exp.State.DeleteSet) {
			t.Errorf("%s: delete set has %d clients, want %d", name, len(u.Deletes.Clients), len(exp.State.DeleteSet))
		}
		for client, ranges := range exp.State.DeleteSet {
			id, err := strconv.ParseUint(client, 10, 64)
			if err != nil {
				t.Fatalf("%s: bad client id %q: %v", name, client, err)
			}
			got := u.Deletes.Clients[crdt.ClientID(id)]
			if len(got) != len(ranges) {
				t.Errorf("%s: client %s has %d delete ranges, want %d", name, client, len(got), len(ranges))
				continue
			}
			for i, r := range ranges {
				if int(got[i].Clock) != r[0] || got[i].Len != r[1] {
					t.Errorf("%s: client %s range %d = (%d,%d), want (%d,%d)",
						name, client, i, got[i].Clock, got[i].Len, r[0], r[1])
				}
				if !u.Deletes.IsDeleted(crdt.NewID(crdt.ClientID(id), crdt.Clock(r[0]))) {
					t.Errorf("%s: IsDeleted(%s:%d) = false", name, client, r[0])
				}
			}
		}
	}
}

// Truncating a valid update at any point must produce an error, never a
// silently short read or a panic.
func TestTruncatedUpdatesRejected(t *testing.T) {
	full := readFixture(t, filepath.Join(filepath.FromSlash(fixturesDir), "map-set-overwrite", "state.bin"))
	for i := range len(full) {
		if _, err := crdt.DecodeUpdate(full[:i]); err == nil {
			t.Errorf("DecodeUpdate(first %d of %d bytes) succeeded, want error", i, len(full))
		}
	}
	if _, err := crdt.DecodeUpdate(append(append([]byte(nil), full...), 0x00)); err == nil {
		t.Error("DecodeUpdate with a trailing byte succeeded, want error")
	}
}

// Yjs accepts client blocks and delete-set clients in any order; it only
// *writes* them descending. Decoding such an update must work, and re-encoding
// it puts it in canonical order rather than preserving the odd one.
func TestNonCanonicalOrderIsCanonicalised(t *testing.T) {
	canonical := readFixture(t, filepath.Join(filepath.FromSlash(fixturesDir), "text-three-client-interleaved", "state.bin"))
	u, err := crdt.DecodeUpdate(canonical)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(u.Clients) < 2 {
		t.Fatalf("fixture has %d client blocks, need at least 2", len(u.Clients))
	}
	// Reverse the blocks, which for a canonical update means ascending order.
	for i, j := 0, len(u.Clients)-1; i < j; i, j = i+1, j-1 {
		u.Clients[i], u.Clients[j] = u.Clients[j], u.Clients[i]
	}
	got, err := u.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !bytes.Equal(got, canonical) {
		t.Errorf("re-encode of reordered update = %x, want %x", got, canonical)
	}
}

func FuzzDecodeUpdateNeverPanics(f *testing.F) {
	for _, dir := range []string{"text-insert-single", "map-set-overwrite", "gc-and-skip"} {
		b, err := os.ReadFile(filepath.Join(filepath.FromSlash(fixturesDir), dir, "state.bin"))
		if err == nil {
			f.Add(b)
		}
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		u, err := crdt.DecodeUpdate(b)
		if err != nil {
			return
		}
		// Encoding canonicalises client order, so re-encoding arbitrary input
		// need not reproduce it byte for byte (see TestNonCanonicalOrderIs
		// Canonicalised). What must hold is that encoding is a fixed point:
		// a canonical update never changes again, so nothing drifts as updates
		// are stored, merged and re-sent.
		once, err := u.Encode()
		if err != nil {
			// Everything that decodes is in range, so encoding cannot fail.
			t.Fatalf("decoded %x but could not encode it: %v", b, err)
		}
		u2, err := crdt.DecodeUpdate(once)
		if err != nil {
			t.Fatalf("re-encoded update does not decode: %v\nbytes %x\nfrom  %x", err, once, b)
		}
		twice, err := u2.Encode()
		if err != nil {
			t.Fatalf("re-encode failed: %v", err)
		}
		if !bytes.Equal(once, twice) {
			t.Fatalf("encoding is not idempotent\nfirst  %x\nsecond %x\nfrom   %x", once, twice, b)
		}
	})
}
