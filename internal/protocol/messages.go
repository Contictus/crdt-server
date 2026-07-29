// Package protocol implements the message framing that y-websocket clients
// speak: an outer message type byte, then either a y-protocols/sync message, a
// y-protocols/awareness update, or a y-protocols/auth message.
//
// The framing is derived from source, not from a specification:
//
//   - outer types: y-websocket/src/y-websocket.js:20-23
//   - sync sub-types and payloads: y-protocols/sync.js:38-40, :48, :59, :96
//   - awareness update layout: y-protocols/awareness.js:194
//   - auth: y-protocols/auth.js:5
//
// Everything here is pure: bytes in, bytes out, no I/O and no clock except the
// timestamp callers pass to Awareness. That is what lets the whole layer be
// tested against the committed testdata/fixtures/**/msg-*.bin files, which were
// produced by the real libraries.
package protocol

import (
	"errors"
	"fmt"

	"github.com/mesutokul/ycollab/internal/crdt/lib0"
)

// Outer message types (y-websocket.js:20-23).
const (
	MessageSync           uint64 = 0
	MessageAwareness      uint64 = 1
	MessageAuth           uint64 = 2
	MessageQueryAwareness uint64 = 3
)

// Sync sub-types (sync.js:38-40).
const (
	SyncStep1  uint64 = 0
	SyncStep2  uint64 = 1
	SyncUpdate uint64 = 2
)

// Auth sub-types (auth.js:5).
const (
	AuthPermissionDenied uint64 = 0
)

var (
	// ErrUnknownMessageType means the outer type byte is not one of the four
	// y-websocket defines. The reference client logs and drops such a message;
	// we return an error so a mismatch shows up in a test instead of as silence.
	ErrUnknownMessageType = errors.New("protocol: unknown message type")
	// ErrUnknownSyncType means the sync sub-type is not 0, 1 or 2.
	ErrUnknownSyncType = errors.New("protocol: unknown sync message type")
	// ErrUnknownAuthType means the auth sub-type is not permission-denied.
	ErrUnknownAuthType = errors.New("protocol: unknown auth message type")
	// ErrTrailingBytes means a frame carried bytes after a complete message.
	// See DECISIONS D13: we are deliberately stricter than lib0 here, because a
	// frame we cannot account for byte for byte is a frame we cannot relay.
	ErrTrailingBytes = errors.New("protocol: trailing bytes after message")
)

// A Message is one decoded frame.
type Message interface {
	isMessage()
}

// SyncStep1Message carries the sender's state vector and asks for whatever it
// is missing (sync.js:48).
type SyncStep1Message struct{ StateVector []byte }

// SyncStep2Message carries the structs and delete set the peer asked for
// (sync.js:59). It is a reply, not a broadcast.
type SyncStep2Message struct{ Update []byte }

// UpdateMessage carries an incremental update (sync.js:96).
type UpdateMessage struct{ Update []byte }

// AwarenessMessage carries an awareness update payload. The payload is kept
// encoded; use Awareness.ApplyUpdate to interpret it.
type AwarenessMessage struct{ Payload []byte }

// QueryAwarenessMessage asks for every awareness state the receiver knows
// (y-websocket.js:53-67). It has no payload.
type QueryAwarenessMessage struct{}

// PermissionDeniedMessage rejects a connection (auth.js:11).
type PermissionDeniedMessage struct{ Reason string }

func (SyncStep1Message) isMessage()        {}
func (SyncStep2Message) isMessage()        {}
func (UpdateMessage) isMessage()           {}
func (AwarenessMessage) isMessage()        {}
func (QueryAwarenessMessage) isMessage()   {}
func (PermissionDeniedMessage) isMessage() {}

// Decode parses one frame.
//
// The returned payload slices alias buf, which is what the caller wants: the
// room broadcasts an update's bytes unchanged, and copying every frame twice
// per fanout is exactly the kind of cost that shows up under load. Callers must
// therefore not reuse the buffer they passed in - the gateway hands over the
// slice coder/websocket allocated for that read and never touches it again.
func Decode(buf []byte) (Message, error) {
	d := lib0.NewDecoder(buf)
	typ, err := d.ReadVarUint()
	if err != nil {
		return nil, err
	}
	var msg Message
	switch typ {
	case MessageSync:
		msg, err = decodeSync(d)
	case MessageAwareness:
		var payload []byte
		payload, err = d.ReadVarUint8Array()
		msg = AwarenessMessage{Payload: payload}
	case MessageQueryAwareness:
		msg = QueryAwarenessMessage{}
	case MessageAuth:
		msg, err = decodeAuth(d)
	default:
		return nil, fmt.Errorf("%w: %d", ErrUnknownMessageType, typ)
	}
	if err != nil {
		return nil, err
	}
	if !d.Done() {
		return nil, ErrTrailingBytes
	}
	return msg, nil
}

func decodeSync(d *lib0.Decoder) (Message, error) {
	sub, err := d.ReadVarUint()
	if err != nil {
		return nil, err
	}
	payload, err := d.ReadVarUint8Array()
	if err != nil {
		return nil, err
	}
	switch sub {
	case SyncStep1:
		return SyncStep1Message{StateVector: payload}, nil
	case SyncStep2:
		return SyncStep2Message{Update: payload}, nil
	case SyncUpdate:
		return UpdateMessage{Update: payload}, nil
	default:
		return nil, fmt.Errorf("%w: %d", ErrUnknownSyncType, sub)
	}
}

func decodeAuth(d *lib0.Decoder) (Message, error) {
	sub, err := d.ReadVarUint()
	if err != nil {
		return nil, err
	}
	if sub != AuthPermissionDenied {
		return nil, fmt.Errorf("%w: %d", ErrUnknownAuthType, sub)
	}
	reason, err := d.ReadVarString()
	if err != nil {
		return nil, err
	}
	return PermissionDeniedMessage{Reason: reason}, nil
}

// WriteSyncStep1 frames a state vector request (sync.js:47-49).
func WriteSyncStep1(stateVector []byte) []byte {
	return writeSync(SyncStep1, stateVector)
}

// WriteSyncStep2 frames the reply to a step 1 (sync.js:57-60).
func WriteSyncStep2(update []byte) []byte {
	return writeSync(SyncStep2, update)
}

// WriteUpdate frames an incremental update (sync.js:95-97).
func WriteUpdate(update []byte) []byte {
	return writeSync(SyncUpdate, update)
}

func writeSync(sub uint64, payload []byte) []byte {
	e := lib0.NewEncoderSize(len(payload) + 8)
	e.WriteVarUint(MessageSync)
	e.WriteVarUint(sub)
	e.WriteVarUint8Array(payload)
	return e.Bytes()
}

// WriteAwareness frames an awareness update payload (y-websocket.js:369-372).
func WriteAwareness(payload []byte) []byte {
	e := lib0.NewEncoderSize(len(payload) + 8)
	e.WriteVarUint(MessageAwareness)
	e.WriteVarUint8Array(payload)
	return e.Bytes()
}

// WriteQueryAwareness frames a request for every known awareness state
// (y-websocket.js:460).
func WriteQueryAwareness() []byte {
	e := lib0.NewEncoderSize(1)
	e.WriteVarUint(MessageQueryAwareness)
	return e.Bytes()
}

// WritePermissionDenied frames a rejection (auth.js:11-14). The client logs the
// reason and stops reconnecting to that room.
func WritePermissionDenied(reason string) []byte {
	e := lib0.NewEncoderSize(len(reason) + 8)
	e.WriteVarUint(MessageAuth)
	e.WriteVarUint(AuthPermissionDenied)
	e.WriteVarString(reason)
	return e.Bytes()
}
