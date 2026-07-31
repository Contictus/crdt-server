// Package store persists documents in PostgreSQL: a snapshot per document plus
// an append-only log of the updates that arrived after it.
//
// Loading a document is the snapshot followed by every remaining log row, in
// seq order. Compaction folds the log into a new snapshot and deletes the rows
// it covered, in one transaction.
package store

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

// ErrStaleSnapshot means a newer snapshot was written while this one was being
// prepared, so it was discarded. See [Store.Compact].
var ErrStaleSnapshot = errors.New("store: a newer snapshot already exists")

// A Store is the database. It is safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects and verifies the connection.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Migrate creates the schema if it is not there. It is idempotent, so it can
// run on every boot.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

// Ensure creates the document row if it is not there, so that a caller which
// writes to the log without reading the document first does not trip the
// foreign key.
//
// Restoring into a document that was just deleted, or into a database that has
// never heard of it, is exactly that case - and it is the disaster-recovery
// path, so it has to work without a read first.
func (s *Store) Ensure(ctx context.Context, id UUID) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO documents (id, owner_id) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`,
		id, NilUUID,
	); err != nil {
		return fmt.Errorf("store: ensure document: %w", err)
	}
	return nil
}

// A Document is what was on disk: a snapshot, possibly empty, and the log rows
// that came after it.
type Document struct {
	// Snapshot is a full Yjs update, or nil for a document that has never been
	// compacted.
	Snapshot []byte
	// SnapshotSeq is the log position the snapshot covers.
	SnapshotSeq int64
	// Updates are the remaining log rows in seq order.
	Updates [][]byte
	// Seqs are those rows' seq values, positionally matching Updates. A room
	// that folds these into a new snapshot passes them back to Compact, which
	// deletes exactly them.
	Seqs []int64
	// LastSeq is the highest seq read, or SnapshotSeq if the log was empty.
	LastSeq int64
}

// Load reads a document, creating its row if this is the first time anyone has
// asked for it.
//
// Every remaining log row is returned, including any whose seq is below
// SnapshotSeq. Compaction deletes exactly the rows it folded in, so a row that
// is still there was not folded in - and if one ever is returned redundantly,
// applying it again is free, because updates are idempotent. Filtering on
// `seq > snapshot_seq` instead would turn a row that committed out of seq order
// into silent data loss, which is possible because identity values are handed
// out before commit. See DECISIONS D35.
func (s *Store) Load(ctx context.Context, id UUID) (*Document, error) {
	doc := &Document{}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: load: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	if _, err := tx.Exec(ctx,
		`INSERT INTO documents (id, owner_id) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`,
		id, NilUUID,
	); err != nil {
		return nil, fmt.Errorf("store: ensure document: %w", err)
	}

	if err := tx.QueryRow(ctx,
		`SELECT snapshot, snapshot_seq FROM documents WHERE id = $1`, id,
	).Scan(&doc.Snapshot, &doc.SnapshotSeq); err != nil {
		return nil, fmt.Errorf("store: read document: %w", err)
	}
	doc.LastSeq = doc.SnapshotSeq

	rows, err := tx.Query(ctx,
		`SELECT seq, payload FROM doc_updates WHERE doc_id = $1 ORDER BY seq`, id)
	if err != nil {
		return nil, fmt.Errorf("store: read updates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var payload []byte
		if err := rows.Scan(&seq, &payload); err != nil {
			return nil, fmt.Errorf("store: scan update: %w", err)
		}
		doc.Updates = append(doc.Updates, payload)
		doc.Seqs = append(doc.Seqs, seq)
		if seq > doc.LastSeq {
			doc.LastSeq = seq
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read updates: %w", err)
	}
	rows.Close()

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: load: %w", err)
	}
	return doc, nil
}

// Append writes updates to the log and returns the seq assigned to each.
//
// The payloads go in one statement: an editing session produces a steady drip
// of small updates, and a round trip each would make the database the thing
// that decides how fast people can type.
//
// The caller keeps the seqs so that a later compaction can delete exactly the
// rows its snapshot covers.
func (s *Store) Append(ctx context.Context, id UUID, payloads [][]byte) ([]int64, error) {
	if len(payloads) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`INSERT INTO doc_updates (doc_id, payload)
		 SELECT $1, payload FROM unnest($2::bytea[]) AS payload
		 RETURNING seq`,
		id, payloads)
	if err != nil {
		return nil, fmt.Errorf("store: append: %w", err)
	}
	defer rows.Close()
	seqs := make([]int64, 0, len(payloads))
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return nil, fmt.Errorf("store: append: %w", err)
		}
		seqs = append(seqs, seq)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: append: %w", err)
	}
	return seqs, nil
}

// Compact replaces the snapshot and deletes exactly the log rows it covers, in
// one transaction. folded lists the seqs the snapshot contains.
//
// The delete names its rows rather than deleting a range. A range would be
// wrong: seq comes from an identity column, and identity values are handed out
// before commit, so a row with a lower seq can become visible after one with a
// higher seq. "seq <= watermark" would then take a row the snapshot never saw.
// Naming the rows makes the operation correct however the commits interleave,
// which matters as soon as more than one replica appends to a document.
//
// Two further guards, aimed at the race the architecture invites by having no
// document ownership:
//
//   - an advisory lock serialises compaction of one document, so two replicas
//     cannot interleave their snapshot write and their delete;
//   - the write only lands if it moves snapshot_seq forward, so an older
//     snapshot cannot overwrite a newer one. That returns ErrStaleSnapshot and
//     changes nothing.
//
// See DECISIONS C2 and C6.
func (s *Store) Compact(ctx context.Context, id UUID, snapshot []byte, folded []int64) error {
	if len(folded) == 0 {
		// Nothing has been written since the last snapshot, so a new one would
		// say nothing the old one does not.
		return nil
	}
	var watermark int64
	for _, seq := range folded {
		if seq > watermark {
			watermark = seq
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: compact: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, id); err != nil {
		return fmt.Errorf("store: compact lock: %w", err)
	}

	tag, err := tx.Exec(ctx,
		`UPDATE documents
		    SET snapshot = $2, snapshot_seq = $3, updated_at = now()
		  WHERE id = $1 AND snapshot_seq < $3`,
		id, snapshot, watermark)
	if err != nil {
		return fmt.Errorf("store: write snapshot: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleSnapshot
	}

	// Anything not named here survives, including updates that arrived while
	// the snapshot was being prepared.
	if _, err := tx.Exec(ctx,
		`DELETE FROM doc_updates WHERE doc_id = $1 AND seq = ANY($2)`, id, folded,
	); err != nil {
		return fmt.Errorf("store: prune log: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: compact: %w", err)
	}
	return nil
}

// Delete removes a document and its log. It reports whether there was one.
//
// The log goes with it through the foreign key's ON DELETE CASCADE, so this is
// one statement and cannot leave orphaned updates behind. Callers are expected
// to have stopped serving the document first: nothing here prevents a resident
// room from writing a snapshot afterwards and bringing it back.
func (s *Store) Delete(ctx context.Context, id UUID) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM documents WHERE id = $1`, id)
	if err != nil {
		return false, fmt.Errorf("store: delete: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// DeleteIdle removes documents that have seen no activity since before, and
// reports how many went.
//
// Activity means either the document row being touched - which compaction does -
// or a log row written since then. Reading the log rather than maintaining a
// last-touched column keeps the append path at one statement: appending is the
// hot path and retention runs a few times a day.
//
// A document that is currently resident in some replica's memory can still be
// caught here, and would be recreated empty by the next snapshot that replica
// writes. That is why the server only runs this against documents nothing has
// touched for days, and why the interval is a deployment's decision rather than
// a default.
func (s *Store) DeleteIdle(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM documents d
		  WHERE d.updated_at < $1
		    AND NOT EXISTS (
		          SELECT 1 FROM doc_updates u
		           WHERE u.doc_id = d.id AND u.created_at >= $1)`,
		before)
	if err != nil {
		return 0, fmt.Errorf("store: delete idle: %w", err)
	}
	return tag.RowsAffected(), nil
}

// UpdateCount reports how many log rows a document currently has. Used by
// tests; the server tracks its own count in memory.
func (s *Store) UpdateCount(ctx context.Context, id UUID) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM doc_updates WHERE doc_id = $1`, id).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count: %w", err)
	}
	return n, nil
}
