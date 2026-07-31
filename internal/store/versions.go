package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mesutokul/ycollab/internal/pack"
)

// Version history answers the question the rest of this store cannot: not "what
// does the document say" but "what did it say yesterday, before somebody
// pasted over it". Nothing else in the server can answer it - a CRDT update log
// is a record of what was added, and compaction folds it away by design.
//
// A version is a complete document rather than a diff from the version before
// it. A chain would be smaller, and would make reading one version a walk
// through every version before it, with no way to drop an old one - which is
// the only operation retention consists of. What keeps the size honest is that
// a version is written only when it differs from the newest one, so a document
// nobody edited produces one row however long the timer runs.

// ErrNoVersion means the version asked for is not there, or belongs to another
// document.
var ErrNoVersion = errors.New("store: no such version")

// A Version is one stored state of a document.
type Version struct {
	// ID orders versions within a document; it is what a caller asks for.
	ID int64
	// CreatedAt is when it was taken.
	CreatedAt time.Time
	// StateVector is the document's version at that moment. It is the same
	// bytes the read API returns as an ETag, so a caller can tell whether a
	// version matches what it already has without downloading it.
	StateVector []byte
	// Label is empty for the versions the timer took, and whatever the caller
	// said for the ones taken by hand.
	Label string
	// Bytes is the size of Payload. On a listing, where Payload is not read,
	// it is the *stored* size, which is what the version costs and may be
	// smaller than the document it holds; on a load it is the document.
	Bytes int
	// Payload is the whole document as a Yjs update. Nil on listings: a list of
	// twenty versions of a megabyte document is not a listing.
	Payload []byte
}

// SaveVersion stores a version, unless one would say nothing new.
//
// Two conditions, both there because every replica holding a document runs its
// own timer:
//
//   - nothing is written when a version newer than minAge already exists, so
//     three replicas on the same document produce one version per interval
//     rather than three;
//   - nothing is written when the newest version has the same state vector, so
//     a document nobody is editing does not accumulate identical copies.
//
// The advisory lock is what makes those hold between replicas, and it is not
// decoration. `INSERT ... WHERE NOT EXISTS` looks atomic and is not: under READ
// COMMITTED two concurrent statements each evaluate the subquery against their
// own snapshot, neither sees the other's uncommitted row, and both insert.
// Demonstrated against this schema with two sessions, which produced two rows.
//
// The damage was not corruption but retention: duplicates eat the budget, so
// "keep the last 24" on a three-replica cluster silently becomes "keep the last
// eight moments". Compact takes the same lock, for the same reason (C2, C6).
//
// It reports whether a row was written.
func (s *Store) SaveVersion(ctx context.Context, id UUID, v Version, minAge time.Duration) (bool, error) {
	if len(v.Payload) == 0 {
		return false, errors.New("store: a version needs a payload")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: save version: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	// Per document, released at commit. Two replicas versioning two different
	// documents never wait for each other.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, id); err != nil {
		return false, fmt.Errorf("store: save version lock: %w", err)
	}

	// History is the largest thing in this database - a whole document per
	// version, twenty-four per document by default - so it is the blob most
	// worth compressing.
	packed, codec := pack.Pack(v.Payload)
	s.observePack(len(v.Payload), len(packed))
	tag, err := tx.Exec(ctx,
		`INSERT INTO doc_versions (doc_id, state_vector, payload, label, codec)
		 SELECT $1, $2, $3, $4, $6
		  WHERE EXISTS (SELECT 1 FROM documents WHERE id = $1)
		    AND NOT EXISTS (
		          SELECT 1 FROM doc_versions v
		           WHERE v.doc_id = $1
		             AND (v.created_at > now() - $5::interval OR v.state_vector = $2)
		             AND v.id = (SELECT max(id) FROM doc_versions WHERE doc_id = $1))`,
		id, v.StateVector, packed, v.Label, minAge, codec)
	if err != nil {
		return false, fmt.Errorf("store: save version: %w", err)
	}
	written := tag.RowsAffected() > 0
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: save version: %w", err)
	}
	return written, nil
}

// ListVersions returns a document's versions, newest first, without their
// payloads. limit caps how many; zero or less means fifty.
func (s *Store) ListVersions(ctx context.Context, id UUID, limit int) ([]Version, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, created_at, state_vector, label, length(payload)
		   FROM doc_versions WHERE doc_id = $1 ORDER BY id DESC LIMIT $2`,
		id, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list versions: %w", err)
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.ID, &v.CreatedAt, &v.StateVector, &v.Label, &v.Bytes); err != nil {
			return nil, fmt.Errorf("store: scan version: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list versions: %w", err)
	}
	return out, nil
}

// LoadVersion reads one version, payload included.
//
// The document id is part of the lookup, not just the version id: a caller that
// guesses a number must not be handed somebody else's document.
func (s *Store) LoadVersion(ctx context.Context, id UUID, version int64) (*Version, error) {
	v := &Version{ID: version}
	var codec pack.Codec
	err := s.pool.QueryRow(ctx,
		`SELECT created_at, state_vector, label, payload, codec
		   FROM doc_versions WHERE doc_id = $1 AND id = $2`,
		id, version,
	).Scan(&v.CreatedAt, &v.StateVector, &v.Label, &v.Payload, &codec)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoVersion
		}
		return nil, fmt.Errorf("store: load version: %w", err)
	}
	if v.Payload, err = pack.Unpack(v.Payload, codec); err != nil {
		return nil, fmt.Errorf("store: load version: %w", err)
	}
	v.Bytes = len(v.Payload)
	return v, nil
}

// PruneVersions keeps the newest keep versions of a document and deletes the
// rest, reporting how many went. keep of zero or less deletes nothing, because
// "keep none" is what DELETE on the document is for.
//
// Unbounded history is unbounded storage - each row is a whole document - so
// something has to say when to stop, and a count is the form an operator can
// reason about: "the last twenty-four" is a promise, "a gigabyte" is not.
func (s *Store) PruneVersions(ctx context.Context, id UUID, keep int) (int64, error) {
	if keep <= 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM doc_versions
		  WHERE doc_id = $1
		    AND id NOT IN (
		          SELECT id FROM doc_versions WHERE doc_id = $1 ORDER BY id DESC LIMIT $2)`,
		id, keep)
	if err != nil {
		return 0, fmt.Errorf("store: prune versions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// VersionCount reports how many versions a document has.
func (s *Store) VersionCount(ctx context.Context, id UUID) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM doc_versions WHERE doc_id = $1`, id).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count versions: %w", err)
	}
	return n, nil
}
