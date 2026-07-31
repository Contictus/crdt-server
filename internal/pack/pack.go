// Package pack compresses the blobs this server stores, and says which way it
// did it.
//
// Two things are worth compressing and one is not. A snapshot is the whole
// document folded into one update, and a version is a copy of that kept for
// history - twenty-four of them per document by default, which makes history
// the largest thing in the database. Both are written rarely and read rarely,
// so spending CPU to make them smaller is a good trade.
//
// The individual rows in the update log are not worth it. A keystroke is around
// twenty bytes and deflate's own header is about that, so compressing them would
// reliably make them bigger; they are also deleted at the next compaction. They
// are stored exactly as they arrived.
//
// The codec is recorded in a column beside the bytes rather than guessed from
// them. A leading magic byte was the obvious alternative and does not work here:
// every row written before this existed is a bare Yjs update, and a Yjs update
// begins with a varUint that can be any value including whatever marker we
// picked. A column cannot be ambiguous, and it makes "how much of this database
// is still uncompressed" a question SQL can answer during a migration.
package pack

import (
	"bytes"
	"compress/flate"
	"fmt"
	"io"
)

// A Codec says how a stored blob was encoded. The values are written to the
// database, so they are fixed forever: a number that is reused means old rows
// decoded the wrong way.
type Codec int16

const (
	// Raw is the bytes exactly as they were given. It is zero so that every row
	// written before this existed - and every column default - already says the
	// truth about itself.
	Raw Codec = 0
	// Deflate is RFC 1951, from compress/flate. Chosen over gzip because the
	// codec is already named in a column, so gzip's header would be ten bytes
	// per blob restating what the column says. Chosen over zstd because zstd is
	// a dependency and this is not the compression ratio that decides anything.
	Deflate Codec = 1
)

func (c Codec) String() string {
	switch c {
	case Raw:
		return "raw"
	case Deflate:
		return "deflate"
	default:
		return fmt.Sprintf("codec(%d)", int16(c))
	}
}

// minSize is the payload below which compression is not attempted. Deflate's
// overhead on a short input is larger than anything it can save, and an empty
// document's snapshot is a handful of bytes.
const minSize = 256

// Pack compresses b, returning the bytes to store and how they were encoded.
//
// It falls back to Raw whenever compression did not actually help. That is not
// belt and braces: a Yjs update of a document full of random ids or of already
// compressed content can come out larger, and a storage layer that can make a
// document bigger by storing it is a storage layer with a surprising bill.
//
// The returned slice may alias b when the codec is Raw. Callers hand it straight
// to the database, which copies it.
func Pack(b []byte) ([]byte, Codec) {
	if len(b) < minSize {
		return b, Raw
	}
	var buf bytes.Buffer
	// Grown to the input size so the common case does one allocation and the
	// "it got bigger" case stops reallocating early.
	buf.Grow(len(b))
	w, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		// flate.NewWriter only fails on an out-of-range level, and the level
		// here is a constant. Storing raw is still correct, so this is reported
		// by the codec rather than by an error nobody could act on.
		return b, Raw
	}
	if _, err := w.Write(b); err != nil {
		return b, Raw
	}
	if err := w.Close(); err != nil {
		return b, Raw
	}
	if buf.Len() >= len(b) {
		return b, Raw
	}
	return buf.Bytes(), Deflate
}

// Unpack reverses Pack.
//
// An unknown codec is an error rather than a guess. It is what a newer server
// writing a codec this binary has never heard of looks like - during a rollback,
// say - and reading those bytes as something else would produce a document that
// is wrong rather than a document that is missing.
func Unpack(b []byte, c Codec) ([]byte, error) {
	switch c {
	case Raw:
		return b, nil
	case Deflate:
		r := flate.NewReader(bytes.NewReader(b))
		defer r.Close()
		out, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("pack: decompress: %w", err)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("pack: unknown codec %d; this binary may be older than the one that wrote it", int16(c))
	}
}
