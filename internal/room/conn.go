package room

// Close codes the room uses. They are the WebSocket status codes, repeated here
// so this package does not depend on the WebSocket library: the room is an
// actor over an interface, and its tests use a fake connection.
const (
	// CloseGoingAway (1001) - the room is shutting down.
	CloseGoingAway = 1001
	// CloseProtocolError (1002) - the connection sent something we cannot
	// decode or a document update that does not apply.
	CloseProtocolError = 1002
	// CloseInternalError (1011) - the server could not serve this document, for
	// example because loading it from the database failed.
	CloseInternalError = 1011
	// ClosePolicyViolation (1008) - the connection could not keep up. Per the
	// brief this is the backpressure policy: drop the slow client and let it
	// reconnect, rather than growing a buffer on its behalf. Recovery costs one
	// SyncStep1 and a diff.
	ClosePolicyViolation = 1008
)

// A Conn is the room's view of one client connection.
//
// The room only ever hands complete frames to Send and never blocks on it: an
// implementation must queue into a bounded buffer and report false when that
// buffer is full. That is the whole backpressure contract - the room reacts by
// closing the connection, so one slow client cannot stall the document.
type Conn interface {
	// ID identifies the connection for logging.
	ID() uint64
	// CanWrite reports whether this connection may send document updates. It is
	// decided once, when the connection is authorised, and never changes: a
	// token's permission cannot be revoked mid-connection, and pretending
	// otherwise would be a promise this server cannot keep.
	CanWrite() bool
	// Send queues one frame, reporting false if the outbound buffer is full.
	// It must not block.
	Send(frame []byte) bool
	// Close terminates the connection. It must be safe to call more than once
	// and must not block.
	Close(code int, reason string)
}
