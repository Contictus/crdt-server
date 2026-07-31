package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Handles for the external tests to look at what is actually in the table.
//
// This file is _test.go, so none of it is in the built binary. It exists
// because the interesting assertions about compression are about the stored
// bytes rather than the returned ones - "it round trips" would pass just as well
// with compression switched off.

func (s *Store) QueryRowForTest(ctx context.Context, sql string, args ...any) pgx.Row {
	return s.pool.QueryRow(ctx, sql, args...)
}

func (s *Store) ExecForTest(ctx context.Context, sql string, args ...any) error {
	_, err := s.pool.Exec(ctx, sql, args...)
	return err
}

func (s *Store) QueryForTest(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return s.pool.Query(ctx, sql, args...)
}
