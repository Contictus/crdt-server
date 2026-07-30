package main

import (
	"fmt"

	"github.com/mesutokul/ycollab/internal/crdt"
	"github.com/mesutokul/ycollab/internal/crdt/lib0"
)

// The load bot has to author updates, and internal/crdt deliberately cannot:
// it is a server engine, it reads and integrates documents and never edits one.
// So the bytes are built here, by hand, in the same shape a Yjs client produces
// when somebody types at the end of a line.
//
// One struct per update, appended to this client's own chain:
//
//	first update:  no origin, so the struct carries parent info instead - a
//	               root type and its name (struct.go:253)
//	later updates: origin is the last unit of the previous struct, and no
//	               parent info is written (struct.go:249-253)
//
// The content is ref 4, ContentString, which is a varString (content.go:151).
// selfTest below applies a chain of these to a real Doc, so a mistake here fails
// at startup rather than as a stream of 1002 closes from the server.

// contentString is the content ref for a plain string (content.go:17).
const contentString = 4

// bitOrigin marks an origin as present (struct.go:11).
const bitOrigin = 0x80

// buildUpdate returns one update: client appending text at the end of its own
// chain, whose next struct starts at clock.
func buildUpdate(client, clock uint64, root, text string) []byte {
	e := lib0.NewEncoderSize(len(text) + 32)
	e.WriteVarUint(1)      // one client block
	e.WriteVarUint(1)      // carrying one struct
	e.WriteVarUint(client) // whose author is
	e.WriteVarUint(clock)  // starting at this clock

	info := byte(contentString)
	if clock > 0 {
		info |= bitOrigin
	}
	e.WriteUint8(info)
	if clock > 0 {
		// The item immediately to the left is the last unit of what we wrote
		// before, which is one clock back.
		e.WriteVarUint(client)
		e.WriteVarUint(clock - 1)
	} else {
		e.WriteVarUint(1) // parent is a root type
		e.WriteVarString(root)
	}
	e.WriteVarString(text)

	e.WriteVarUint(0) // empty delete set
	return e.Bytes()
}

// selfTest builds a short chain and integrates it, so a malformed builder is
// caught here rather than by the server.
func selfTest(root string) error {
	doc := crdt.NewDoc(1)
	const client = 424242
	var clock uint64
	for _, s := range []string{"hello ", "load ", "bot"} {
		if err := doc.ApplyUpdate(buildUpdate(client, clock, root, s)); err != nil {
			return fmt.Errorf("update at clock %d: %w", clock, err)
		}
		clock += uint64(len(s))
	}
	if n := doc.PendingCount(); n != 0 {
		return fmt.Errorf("%d updates would not integrate", n)
	}
	if got, want := crdt.AsText(doc.Get(root)).String(), "hello load bot"; got != want {
		return fmt.Errorf("the chain reads %q, want %q", got, want)
	}
	return nil
}
