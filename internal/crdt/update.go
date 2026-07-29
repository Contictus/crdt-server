package crdt

import (
	"sort"

	"github.com/mesutokul/ycollab/internal/crdt/lib0"
)

// ClientBlock is one client's contiguous run of structs inside an update.
//
// StartClock is the clock the block begins at, which need not be the clock the
// client began at: a diff starts where the receiver left off, and the first
// struct may be a slice of a larger one.
type ClientBlock struct {
	Client     ClientID
	StartClock Clock
	Structs    []Struct
}

// Update is a decoded v1 update: structs grouped per client, then a delete set.
//
// It is a faithful representation of the bytes, not of a document. Decoding and
// re-encoding an update reproduces it byte for byte, which is what lets the
// server relay and store updates it has not integrated.
type Update struct {
	Clients []ClientBlock
	Deletes *DeleteSet
}

// DecodeUpdate parses a v1 update.
//
// Trailing bytes are rejected. Yjs would ignore them; here they can only mean a
// framing bug or a corrupt record, and silently accepting a prefix is how a
// server ends up persisting garbage.
func DecodeUpdate(b []byte) (*Update, error) {
	d := lib0.NewDecoder(b)
	u, err := readUpdate(d)
	if err != nil {
		return nil, err
	}
	if !d.Done() {
		return nil, ErrCorruptUpdate
	}
	return u, nil
}

func readUpdate(d *lib0.Decoder) (*Update, error) {
	numClients, err := d.ReadVarUint()
	if err != nil {
		return nil, err
	}
	// Each client block costs at least four bytes.
	if numClients > uint64(d.Remaining()) {
		return nil, lib0.ErrUnexpectedEOF
	}
	u := &Update{Clients: make([]ClientBlock, 0, numClients)}
	for range numClients {
		numStructs, err := d.ReadVarUint()
		if err != nil {
			return nil, err
		}
		// Every struct is at least one info byte plus one length byte.
		if numStructs > uint64(d.Remaining()) {
			return nil, lib0.ErrUnexpectedEOF
		}
		client, err := readSafeVarUint(d)
		if err != nil {
			return nil, err
		}
		startClock, err := readSafeVarUint(d)
		if err != nil {
			return nil, err
		}
		block := ClientBlock{
			Client:     ClientID(client),
			StartClock: Clock(startClock),
			Structs:    make([]Struct, 0, numStructs),
		}
		clock := Clock(startClock)
		for range numStructs {
			s, err := readStruct(d, block.Client, clock)
			if err != nil {
				return nil, err
			}
			block.Structs = append(block.Structs, s)
			clock += Clock(s.StructLen())
		}
		u.Clients = append(u.Clients, block)
	}
	ds, err := readDeleteSet(d)
	if err != nil {
		return nil, err
	}
	u.Deletes = ds
	return u, nil
}

// Encode returns the wire form of the update.
//
// It returns an error rather than a short buffer: the encoder swallows writes
// after its first failure, so ignoring it would hand the caller a silently
// truncated update.
func (u *Update) Encode() ([]byte, error) {
	e := lib0.NewEncoder()
	u.write(e)
	if err := e.Err(); err != nil {
		return nil, err
	}
	return e.Bytes(), nil
}

func (u *Update) write(e *lib0.Encoder) {
	blocks := u.Clients
	if !isDescendingByClient(blocks) {
		// Client blocks go out with the highest client id first - the comment
		// in yjs/src/utils/encoding.js:99 says this "heavily improves the
		// conflict algorithm". Sort a copy so the caller's slice is untouched.
		sorted := make([]ClientBlock, len(blocks))
		copy(sorted, blocks)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Client > sorted[j].Client })
		blocks = sorted
	}
	e.WriteVarUint(uint64(len(blocks)))
	for _, block := range blocks {
		e.WriteVarUint(uint64(len(block.Structs)))
		e.WriteVarUint(uint64(block.Client))
		e.WriteVarUint(uint64(block.StartClock))
		for i, s := range block.Structs {
			offset := 0
			if i == 0 {
				// Only the first struct of a block can start mid-struct.
				offset = int(block.StartClock - s.StructID().Clock)
			}
			s.write(e, offset)
		}
	}
	if u.Deletes == nil {
		e.WriteVarUint(0)
		return
	}
	u.Deletes.write(e)
}

func isDescendingByClient(blocks []ClientBlock) bool {
	for i := 1; i < len(blocks); i++ {
		if blocks[i-1].Client <= blocks[i].Client {
			return false
		}
	}
	return true
}

// StateVector returns the clocks this update carries, i.e. one past the last
// clock of each client block. It is not the sender's state vector: a diff only
// covers what the receiver was missing.
func (u *Update) StateVector() StateVector {
	sv := NewStateVector()
	for _, block := range u.Clients {
		clock := block.StartClock
		for _, s := range block.Structs {
			clock += Clock(s.StructLen())
		}
		if clock > sv[block.Client] {
			sv[block.Client] = clock
		}
	}
	return sv
}
