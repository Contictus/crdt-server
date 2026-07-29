package crdt

import (
	"sort"

	"github.com/mesutokul/ycollab/internal/crdt/lib0"
)

// StateVector maps each client to the clock it is known up to - the *next*
// expected clock, one past the last integrated struct
// (yjs/src/utils/encoding.js:601).
type StateVector map[ClientID]Clock

// NewStateVector returns an empty state vector.
func NewStateVector() StateVector { return make(StateVector) }

// Get returns the known clock for client, or 0 if the client is unknown. An
// absent client and a client at clock 0 are the same thing.
func (sv StateVector) Get(client ClientID) Clock { return sv[client] }

// EncodeStateVector returns the wire form of sv: an empty vector is the single
// byte 0x00. It fails only if a clock is too large for lib0 to represent.
func EncodeStateVector(sv StateVector) ([]byte, error) {
	e := lib0.NewEncoderSize(1 + len(sv)*6)
	writeStateVector(e, sv)
	if err := e.Err(); err != nil {
		return nil, err
	}
	return e.Bytes(), nil
}

func writeStateVector(e *lib0.Encoder, sv StateVector) {
	clients := make([]ClientID, 0, len(sv))
	for client := range sv {
		clients = append(clients, client)
	}
	// Descending, like every other client-keyed section of the format.
	sort.Slice(clients, func(i, j int) bool { return clients[i] > clients[j] })
	e.WriteVarUint(uint64(len(clients)))
	for _, client := range clients {
		e.WriteVarUint(uint64(client))
		e.WriteVarUint(uint64(sv[client]))
	}
}

// DecodeStateVector parses the wire form of a state vector.
func DecodeStateVector(b []byte) (StateVector, error) {
	d := lib0.NewDecoder(b)
	sv, err := readStateVector(d)
	if err != nil {
		return nil, err
	}
	if !d.Done() {
		return nil, ErrCorruptUpdate
	}
	return sv, nil
}

func readStateVector(d *lib0.Decoder) (StateVector, error) {
	numClients, err := d.ReadVarUint()
	if err != nil {
		return nil, err
	}
	if numClients > uint64(d.Remaining()) {
		return nil, lib0.ErrUnexpectedEOF
	}
	sv := make(StateVector, numClients)
	for range numClients {
		client, err := readSafeVarUint(d)
		if err != nil {
			return nil, err
		}
		clock, err := readSafeVarUint(d)
		if err != nil {
			return nil, err
		}
		sv[ClientID(client)] = Clock(clock)
	}
	return sv, nil
}
