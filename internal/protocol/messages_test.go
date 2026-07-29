package protocol_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mesutokul/ycollab/internal/crdt"
	"github.com/mesutokul/ycollab/internal/protocol"
)

const fixturesDir = "../../testdata/fixtures"

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

// scenarioDirs returns every fixture directory that contains framed messages.
func scenarioDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(fixturesDir)
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(fixturesDir, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "msg-sync-step1.bin")); err == nil {
			dirs = append(dirs, dir)
		}
	}
	if len(dirs) == 0 {
		t.Fatal("no framed-message fixtures found")
	}
	return dirs
}

// The framing must survive a decode/encode round trip byte for byte. This is
// the same standard the update codec is held to (D24): what we relay has to be
// what we received, or a client somewhere sees a frame no Yjs produced.
func TestFramedMessagesRoundTrip(t *testing.T) {
	for _, dir := range scenarioDirs(t) {
		name := filepath.Base(dir)
		cases := []struct {
			file   string
			expect func(protocol.Message) ([]byte, error)
		}{
			{"msg-sync-step1.bin", func(m protocol.Message) ([]byte, error) {
				v, ok := m.(protocol.SyncStep1Message)
				if !ok {
					return nil, errors.New("not a SyncStep1")
				}
				return protocol.WriteSyncStep1(v.StateVector), nil
			}},
			{"msg-sync-step2.bin", func(m protocol.Message) ([]byte, error) {
				v, ok := m.(protocol.SyncStep2Message)
				if !ok {
					return nil, errors.New("not a SyncStep2")
				}
				return protocol.WriteSyncStep2(v.Update), nil
			}},
			{"msg-update.bin", func(m protocol.Message) ([]byte, error) {
				v, ok := m.(protocol.UpdateMessage)
				if !ok {
					return nil, errors.New("not an Update")
				}
				return protocol.WriteUpdate(v.Update), nil
			}},
		}
		for _, c := range cases {
			raw := readFixture(t, filepath.Join(dir, c.file))
			msg, err := protocol.Decode(raw)
			if err != nil {
				t.Fatalf("%s/%s: decode: %v", name, c.file, err)
			}
			got, err := c.expect(msg)
			if err != nil {
				t.Fatalf("%s/%s: %v (got %T)", name, c.file, err, msg)
			}
			if !bytes.Equal(got, raw) {
				t.Fatalf("%s/%s: re-encode differs\n got %x\nwant %x", name, c.file, got, raw)
			}
		}
	}
}

// SyncStep1 carries exactly the state vector Yjs wrote to sv.bin, and SyncStep2
// carries an update that reconstructs a document with that same state vector.
// Checking the payloads - not just the envelope - is what proves the framing is
// wrapping the right bytes rather than merely being self-consistent.
func TestSyncPayloadsMatchFixtures(t *testing.T) {
	for _, dir := range scenarioDirs(t) {
		name := filepath.Base(dir)

		step1 := readFixture(t, filepath.Join(dir, "msg-sync-step1.bin"))
		msg, err := protocol.Decode(step1)
		if err != nil {
			t.Fatalf("%s: decode step1: %v", name, err)
		}
		sv := readFixture(t, filepath.Join(dir, "sv.bin"))
		if got := msg.(protocol.SyncStep1Message).StateVector; !bytes.Equal(got, sv) {
			t.Fatalf("%s: step1 state vector\n got %x\nwant %x", name, got, sv)
		}

		step2 := readFixture(t, filepath.Join(dir, "msg-sync-step2.bin"))
		msg, err = protocol.Decode(step2)
		if err != nil {
			t.Fatalf("%s: decode step2: %v", name, err)
		}
		doc := crdt.NewDoc(1)
		if err := doc.ApplyUpdate(msg.(protocol.SyncStep2Message).Update); err != nil {
			t.Fatalf("%s: apply step2 payload: %v", name, err)
		}
		if n := doc.PendingCount(); n != 0 {
			t.Fatalf("%s: step2 payload left %d updates pending", name, n)
		}
		encoded, err := doc.EncodeStateVector()
		if err != nil {
			t.Fatalf("%s: encode sv: %v", name, err)
		}
		if !bytes.Equal(encoded, sv) {
			t.Fatalf("%s: step2 payload rebuilt a different document\n got %x\nwant %x", name, encoded, sv)
		}
	}
}

func TestQueryAwarenessRoundTrip(t *testing.T) {
	raw := readFixture(t, filepath.Join(fixturesDir, "awareness", "msg-query-awareness.bin"))
	msg, err := protocol.Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := msg.(protocol.QueryAwarenessMessage); !ok {
		t.Fatalf("got %T, want QueryAwarenessMessage", msg)
	}
	if got := protocol.WriteQueryAwareness(); !bytes.Equal(got, raw) {
		t.Fatalf("re-encode\n got %x\nwant %x", got, raw)
	}
}

func TestAwarenessFrameRoundTrip(t *testing.T) {
	raw := readFixture(t, filepath.Join(fixturesDir, "awareness", "msg-awareness.bin"))
	msg, err := protocol.Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	v, ok := msg.(protocol.AwarenessMessage)
	if !ok {
		t.Fatalf("got %T, want AwarenessMessage", msg)
	}
	if got := protocol.WriteAwareness(v.Payload); !bytes.Equal(got, raw) {
		t.Fatalf("re-encode\n got %x\nwant %x", got, raw)
	}
}

// There is no fixture for auth: y-websocket only ever receives this message, so
// the generator cannot produce one through the public API. The layout is from
// auth.js:11-14, and this test pins it.
func TestPermissionDeniedRoundTrip(t *testing.T) {
	raw := protocol.WritePermissionDenied("no")
	want := []byte{0x02, 0x00, 0x02, 'n', 'o'}
	if !bytes.Equal(raw, want) {
		t.Fatalf("encode\n got %x\nwant %x", raw, want)
	}
	msg, err := protocol.Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := msg.(protocol.PermissionDeniedMessage).Reason; got != "no" {
		t.Fatalf("reason %q", got)
	}
}

func TestDecodeRejects(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want error
	}{
		{"empty", nil, nil},
		{"unknown outer type", []byte{0x09}, protocol.ErrUnknownMessageType},
		{"unknown sync type", []byte{0x00, 0x07, 0x00}, protocol.ErrUnknownSyncType},
		{"unknown auth type", []byte{0x02, 0x07}, protocol.ErrUnknownAuthType},
		{"truncated payload", []byte{0x00, 0x02, 0x05, 0x01}, nil},
		{"trailing bytes", []byte{0x03, 0x00}, protocol.ErrTrailingBytes},
		{"trailing after sync", []byte{0x00, 0x02, 0x00, 0xff}, protocol.ErrTrailingBytes},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg, err := protocol.Decode(c.in)
			if err == nil {
				t.Fatalf("accepted %x as %T", c.in, msg)
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

// reencode writes msg back out. Used by the fuzz target and by tests that care
// about what a decoded message serialises to.
func reencode(t *testing.T, msg protocol.Message) []byte {
	t.Helper()
	switch v := msg.(type) {
	case protocol.SyncStep1Message:
		return protocol.WriteSyncStep1(v.StateVector)
	case protocol.SyncStep2Message:
		return protocol.WriteSyncStep2(v.Update)
	case protocol.UpdateMessage:
		return protocol.WriteUpdate(v.Update)
	case protocol.AwarenessMessage:
		return protocol.WriteAwareness(v.Payload)
	case protocol.QueryAwarenessMessage:
		return protocol.WriteQueryAwareness()
	case protocol.PermissionDeniedMessage:
		return protocol.WritePermissionDenied(v.Reason)
	default:
		t.Fatalf("unhandled message type %T", msg)
		return nil
	}
}

// lib0's varUint decoder accepts non-canonical encodings - 0x80 0x00 is a
// perfectly readable zero - so we do too, and normalise on the way out. The
// same choice was made for client ordering in updates (D18).
func TestNonCanonicalVarUintIsAccepted(t *testing.T) {
	msg, err := protocol.Decode([]byte{0x01, 0x80, 0x00})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := reencode(t, msg); !bytes.Equal(got, []byte{0x01, 0x00}) {
		t.Fatalf("got %x, want 0100", got)
	}
}

// A frame arrives from the network. Whatever it contains, decoding it must
// either fail or produce a message whose encoding is stable: re-encoding is
// what the room broadcasts, so encode(decode(encode(x))) has to be a fixed
// point or a relayed frame could drift on every hop.
func FuzzDecodeNeverPanics(f *testing.F) {
	f.Add([]byte{0x03})
	f.Add([]byte{0x00, 0x00, 0x00})
	f.Add([]byte{0x01, 0x01, 0x00})
	f.Add([]byte{0x02, 0x00, 0x00})
	f.Fuzz(func(t *testing.T, data []byte) {
		msg, err := protocol.Decode(data)
		if err != nil {
			return
		}
		once := reencode(t, msg)
		msg2, err := protocol.Decode(once)
		if err != nil {
			t.Fatalf("re-encoded message no longer decodes: %v (%x)", err, once)
		}
		if twice := reencode(t, msg2); !bytes.Equal(once, twice) {
			t.Fatalf("encoding is not stable\n once %x\ntwice %x", once, twice)
		}
	})
}
