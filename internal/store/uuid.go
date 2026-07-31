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

// NilUUID is the all-zero UUID. It is the owner of a document that has none:
// what a server running without multi-tenancy writes, and what every row
// written before tenancy existed carries.
//
// It is a real value with real meaning, not a placeholder. A document owned by
// nobody is reachable only by a connection that claims no owner, so turning
// tenancy on does not quietly hand the existing documents to whoever asks first.
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

// ownerNamespace is the namespace tenant names are hashed under. A different
// constant from documentNamespace on purpose: a tenant called "notes" and a
// document called "notes" must not become the same identifier, and one shared
// namespace is how that would happen.
var ownerNamespace = UUID{
	0xb7, 0x3c, 0x41, 0xd8, 0x0a, 0x9f, 0x4e, 0x26,
	0x8c, 0x5d, 0x92, 0xf0, 0x17, 0x6b, 0xe3, 0xa4,
}

// OwnerID maps a tenant name to an owner id, the same way DocumentID maps a
// room name to a document id.
//
// An application's idea of a tenant is very often not a UUID - it is a slug, an
// account number, a subdomain. Requiring one here would mean every deployment
// carrying a lookup table for the sake of a column type. A name that already is
// a UUID is taken as one, so an application that does have them loses nothing.
//
// The empty string is NilUUID: "no tenant" has to survive the round trip, and a
// hash of "" would be an ordinary owner that nothing could ever match by
// accident but that would still read as owned.
func OwnerID(tenant string) UUID {
	if tenant == "" {
		return NilUUID
	}
	if id, err := ParseUUID(tenant); err == nil {
		return id
	}
	return uuidV5(ownerNamespace, tenant)
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
