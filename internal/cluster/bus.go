package cluster

import (
	"context"
	"crypto/rand"
	"encoding/binary"

	"github.com/mesutokul/ycollab/internal/crdt/lib0"
)

// A Bus moves envelopes between the replicas that hold the same document.
//
// The interface is deliberately narrow: publish to a document's channel,
// subscribe to it. There is no request/response and no membership - a replica
// never learns which other replicas exist, which is what keeps the whole thing
// stateless enough to scale by adding processes.
type Bus interface {
	// Publish sends one envelope to everybody subscribed to room, including the
	// caller. It must not be called from a goroutine that a document depends on:
	// it talks to the network.
	Publish(ctx context.Context, room string, env Envelope) error
	// Subscribe delivers envelopes for room to deliver until the returned
	// Subscription is closed. deliver runs on the bus's own goroutine, so it
	// must not block: one document must not be able to stall every other
	// document on the node.
	Subscribe(ctx context.Context, room string, deliver func(Envelope)) (Subscription, error)
}

// A Subscription is closed when a room stops caring about its channel.
type Subscription interface {
	Close() error
}

// NewNodeID returns a random id for this process.
//
// Random rather than configured: the id's only job is to be different from every
// other replica's, and anything derived from the environment - hostname, pod
// name, index - is one misconfiguration away from two replicas sharing an id,
// at which point they would each ignore the other's traffic and diverge in a way
// that looks like a CRDT bug. 64 random bits make a collision a non-event.
func NewNodeID() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any platform we run on; if it ever does,
		// there is no sane fallback, and a silently duplicated node id is worse
		// than a crash at startup.
		panic("cluster: could not read random bytes: " + err.Error())
	}
	// The id travels as a lib0 varUint, which mirrors JavaScript and therefore
	// tops out at 2^53-1. Masking to 53 bits keeps every id encodable; a
	// collision among 2^53 values is still not a thing that happens.
	id := binary.BigEndian.Uint64(b[:]) & lib0.MaxSafeInteger
	if id == 0 {
		// Zero is what an uninitialised Config has, so it is reserved: a node
		// that ended up with it would filter out its own traffic correctly but
		// would also silently agree with a misconfigured peer.
		id = 1
	}
	return id
}
