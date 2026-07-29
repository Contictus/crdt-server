package store

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
)

// A UUID is a document identifier, matching the schema's `documents.id`.
//
// This is a hand-rolled 16 bytes rather than a dependency: the only things
// needed are parsing, formatting and version 5 derivation, and none of them are
// worth another module in a project whose whole point is that the interesting
// parts are written out.
type UUID [16]byte

// NilUUID is the all-zero UUID. It stands in for `owner_id` until Phase 5
// introduces real users; the column is NOT NULL, so something has to.
var NilUUID UUID

// documentNamespace is the UUID namespace room names are hashed under. It is an
// arbitrary constant, generated once; it only has to be stable, since changing
// it would rename every document in an existing database.
var documentNamespace = UUID{
	0x5f, 0x2b, 0x9a, 0x1e, 0x7c, 0x64, 0x4d, 0x8f,
	0xa3, 0x11, 0x0e, 0x6d, 0xc2, 0x84, 0xb7, 0x59,
}

// ErrBadUUID means a string is not a UUID in the canonical 8-4-4-4-12 form.
var ErrBadUUID = errors.New("store: malformed UUID")

// DocumentID maps a room name to a document id.
//
// The schema keys documents by UUID, but the wire protocol keys rooms by the
// URL path, which people want to be readable. A name that already is a UUID is
// taken as one; anything else is hashed into a version 5 UUID under a fixed
// namespace. So `/notes-2026` and `/f81d4fae-7dec-11d0-a765-00a0c91e6bf6` both
// work, the mapping is deterministic and needs no lookup table, and the schema
// stays exactly as the brief specifies.
func DocumentID(name string) UUID {
	if id, err := ParseUUID(name); err == nil {
		return id
	}
	return uuidV5(documentNamespace, name)
}

// uuidV5 is RFC 4122 §4.3: SHA-1 over the namespace and the name, with the
// version and variant bits overwritten.
func uuidV5(namespace UUID, name string) UUID {
	h := sha1.New()
	h.Write(namespace[:])
	h.Write([]byte(name))
	sum := h.Sum(nil)

	var out UUID
	copy(out[:], sum[:16])
	out[6] = (out[6] & 0x0f) | 0x50 // version 5
	out[8] = (out[8] & 0x3f) | 0x80 // RFC 4122 variant
	return out
}

// String formats the UUID canonically.
func (u UUID) String() string {
	var buf [36]byte
	hex.Encode(buf[0:8], u[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], u[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], u[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], u[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], u[10:16])
	return string(buf[:])
}

// ParseUUID reads the canonical 8-4-4-4-12 form.
func ParseUUID(s string) (UUID, error) {
	var u UUID
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return u, fmt.Errorf("%w: %q", ErrBadUUID, s)
	}
	groups := [][2]int{{0, 8}, {9, 13}, {14, 18}, {19, 23}, {24, 36}}
	pos := 0
	for _, g := range groups {
		n, err := hex.Decode(u[pos:pos+(g[1]-g[0])/2], []byte(s[g[0]:g[1]]))
		if err != nil {
			return UUID{}, fmt.Errorf("%w: %q", ErrBadUUID, s)
		}
		pos += n
	}
	return u, nil
}
