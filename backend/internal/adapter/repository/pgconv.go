package repository

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// pgconv.go holds the tiny conversion helpers that bridge sqlc-generated
// pgtype.* params/results and the plain Go types (time.Time, int) used
// by the port layer. Keeping them in one file means the per-table
// wrappers stay focused on the schema mapping.

// pgts wraps t for sqlc parameter binding. A zero time.Time becomes a
// NULL timestamptz (pgtype.Timestamptz.Valid=false).
func pgts(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// pgtsTime unwraps a sqlc timestamptz result; a non-Valid value returns
// the zero time.Time so the caller can keep using IsZero() checks.
func pgtsTime(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time
}

// pgtsTimePtr returns a pointer to the unwrapped time, or nil when the
// sqlc value is not valid. Used by callers that want to keep the
// existence-vs-zero distinction (ActivatedAt etc.).
func pgtsTimePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

// nullableString returns &s when s is non-empty, else nil. Wires the
// usecase-layer convention of "empty string means absent" into sqlc's
// *string nullable parameter convention.
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}
