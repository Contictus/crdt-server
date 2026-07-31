// Package cluster carries one document's traffic between server replicas.
//
// The unit is an Envelope: who sent it, what kind of thing it is, and the
// payload, which is always bytes the rest of the system already understands - a
// Yjs update, an awareness update, or an encoded state vector. Nothing in here
// interprets a payload.
//
// The encoding is lib0's, the same one the client protocol uses. That is not
// because anything outside this project reads these bytes - nothing does - but
// because the alternative is a second serialisation format in a codebase that
// already has one that is tested against real Yjs.
package cluster

import (
	"errors"
	"fmt"

	"github.com/mesutokul/ycollab/internal/crdt/lib0"
)

// Kind says what an envelope's payload is.
type Kind uint64

const (
	// KindUpdate carries a Yjs update: either one a client produced, or a diff
	// computed in answer to a KindStateVector.
	KindUpdate Kind = 0
	// KindAwareness carries a y-protocols awareness update payload.
	KindAwareness Kind = 1
	// KindStateVector carries an encoded state vector, published periodically so
	// a replica that missed a message finds out. Redis Pub/Sub is at-most-once,
	// so this is the only thing standing between a dropped message and a
	// permanent divergence; see DECISIONS C1.
	KindStateVector Kind = 2
)

// envelopeVersion prefixes every envelope. Two replicas running different
// builds is normal during a rolling restart, and a version byte turns "the
// bytes stopped making sense" into a message we can log and skip.
const envelopeVersion uint64 = 0

var (
	// ErrVersion means the envelope was written by a build that does not agree
	// with this one about the format.
	ErrVersion = errors.New("cluster: unknown envelope version")
	// ErrKind means the envelope kind is not one this build knows.
	ErrKind = errors.New("cluster: unknown envelope kind")
	// ErrTrailingBytes means an envelope carried bytes after a complete message,
	// the same stance internal/protocol takes on client frames.
	ErrTrailingBytes = errors.New("cluster: trailing bytes after envelope")
	// ErrOriginOutOfRange means the origin is larger than lib0 can encode. The
	// format cannot carry it, so accepting it would mean holding a value that
	// cannot be written back out - see Decode.
	ErrOriginOutOfRange = errors.New("cluster: origin out of range")
)

// An Envelope is one message between replicas.
type Envelope struct {
	// Origin identifies the replica that published this. A subscriber sees its
	// own messages - Redis Pub/Sub delivers to every subscriber of a channel,
	// publisher included - and drops them by comparing this against its own node
	// id. That comparison is the whole loop prevention.
	Origin uint64
	Kind   Kind
	// Payload aliases the buffer Decode was given.
	Payload []byte
}

// Encode returns the wire form of an envelope.
//
// It returns an error rather than bytes-and-hope. lib0 refuses an integer above
// 2^53-1, and the encoder records that instead of raising it - so an origin out
// of range used to come back as a one-byte buffer that every replica logged as
// undecodable, with nothing anywhere saying why. Found by the fuzzer: it
// produced an envelope that decoded and then would not survive being written
// back out.
func (e Envelope) Encode() ([]byte, error) {
	enc := lib0.NewEncoderSize(len(e.Payload) + 16)
	enc.WriteVarUint(envelopeVersion)
	enc.WriteVarUint(e.Origin)
	enc.WriteVarUint(uint64(e.Kind))
	enc.WriteVarUint8Array(e.Payload)
	if err := enc.Err(); err != nil {
		return nil, fmt.Errorf("cluster: encode envelope: %w", err)
	}
	return enc.Bytes(), nil
}

// Decode parses one envelope. The payload aliases buf.
func Decode(buf []byte) (Envelope, error) {
	d := lib0.NewDecoder(buf)
	version, err := d.ReadVarUint()
	if err != nil {
		return Envelope{}, err
	}
	if version != envelopeVersion {
		return Envelope{}, fmt.Errorf("%w: %d", ErrVersion, version)
	}
	origin, err := d.ReadVarUint()
	if err != nil {
		return Envelope{}, err
	}
	if origin > lib0.MaxSafeInteger {
		// lib0's decoder is more permissive than its encoder. Accepting a value
		// here that Encode cannot write would leave the rest of the system
		// holding an envelope it can never relay, so it is refused at the door.
		// NewNodeID masks its ids for the same reason.
		return Envelope{}, fmt.Errorf("%w: %d", ErrOriginOutOfRange, origin)
	}
	kind, err := d.ReadVarUint()
	if err != nil {
		return Envelope{}, err
	}
	switch Kind(kind) {
	case KindUpdate, KindAwareness, KindStateVector:
	default:
		return Envelope{}, fmt.Errorf("%w: %d", ErrKind, kind)
	}
	payload, err := d.ReadVarUint8Array()
	if err != nil {
		return Envelope{}, err
	}
	if !d.Done() {
		return Envelope{}, ErrTrailingBytes
	}
	return Envelope{Origin: origin, Kind: Kind(kind), Payload: payload}, nil
}

func (k Kind) String() string {
	switch k {
	case KindUpdate:
		return "update"
	case KindAwareness:
		return "awareness"
	case KindStateVector:
		return "state_vector"
	default:
		return fmt.Sprintf("kind(%d)", uint64(k))
	}
}
