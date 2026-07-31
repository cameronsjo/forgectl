package sessions

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// likeEscape must neutralize the LIKE/ILIKE metacharacters so a `why` fallback
// query matches as a literal substring, not a pattern. A DB-free unit test:
// the integration tests that exercise the query itself skip without a concordance DSN.
func TestLikeEscape(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"50%", `50\%`},
		{"foo_bar", `foo\_bar`},
		{`a\b`, `a\\b`},
		{"%_\\", `\%\_\\`},
		{"", ""},
	}
	for _, c := range cases {
		if got := likeEscape(c.in); got != c.want {
			t.Errorf("likeEscape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// schemaHint turns the two schema-drift SQLSTATEs into operator guidance. A
// DB-free unit test: it drives synthetic *pgconn.PgError values, so it needs
// no concordance DSN.
//
// Test plan
//
//	[x] 42P01 (undefined_table)  → "schema missing" + the DDL pointer
//	[x] 42703 (undefined_column) → "out of date" + the column + the DDL pointer
//	[x] 42703 with an empty ColumnName falls back to the server message
//	[x] An unrelated SQLSTATE passes through undecorated
//	[x] A non-pg error passes through undecorated
//	[x] Both decorated forms still unwrap to the original error
func TestSchemaHint(t *testing.T) {
	const ddl = "scripts/concordance/schema.sql"

	cases := []struct {
		name     string
		err      error
		want     []string // substrings the decorated error must contain
		notWant  []string
		decorate bool
	}{
		{
			name:     "undefined_table points at the canonical DDL",
			err:      &pgconn.PgError{Code: "42P01", Message: `relation "session" does not exist`},
			want:     []string{"schema missing", ddl, "does not exist"},
			decorate: true,
		},
		{
			name:     "undefined_column names the column and the DDL",
			err:      &pgconn.PgError{Code: "42703", Message: `column "harness" of relation "session" does not exist`, ColumnName: "harness"},
			want:     []string{"out of date", "column harness", ddl},
			decorate: true,
		},
		{
			name:     "undefined_column without ColumnName falls back to the message",
			err:      &pgconn.PgError{Code: "42703", Message: `column "tokens_total" of relation "session" does not exist`},
			want:     []string{"out of date", "tokens_total", ddl},
			decorate: true,
		},
		{
			name:    "an unrelated SQLSTATE is not decorated",
			err:     &pgconn.PgError{Code: "23505", Message: "duplicate key value"},
			notWant: []string{ddl},
		},
		{
			name:    "a non-pg error is not decorated",
			err:     errors.New("dial tcp: connection refused"),
			notWant: []string{ddl},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := schemaHint(c.err)
			for _, want := range c.want {
				if !strings.Contains(got.Error(), want) {
					t.Errorf("schemaHint(%v) = %q, want it to contain %q", c.err, got, want)
				}
			}
			for _, notWant := range c.notWant {
				if strings.Contains(got.Error(), notWant) {
					t.Errorf("schemaHint(%v) = %q, want it NOT to contain %q", c.err, got, notWant)
				}
			}
			// Decoration must wrap, never replace: callers still need errors.As.
			if c.decorate && !errors.Is(got, c.err) {
				t.Errorf("decorated error does not unwrap to the original: %v", got)
			}
		})
	}
}

// TestSchemaHint_WrapsThroughAnIntermediateWrap mirrors the real call sites,
// which hand schemaHint an already-wrapped error (fmt.Errorf("upsert …: %w")).
// A plain Code check on the top-level error would miss those.
func TestSchemaHint_WrapsThroughAnIntermediateWrap(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "42703", Message: `column "pricing_source" does not exist`, ColumnName: "pricing_source"}
	wrapped := fmt.Errorf("upsert 12 session rows: %w", pgErr)

	got := schemaHint(wrapped).Error()
	for _, want := range []string{"upsert 12 session rows", "out of date", "column pricing_source", "scripts/concordance/schema.sql"} {
		if !strings.Contains(got, want) {
			t.Errorf("schemaHint(wrapped) = %q, want it to contain %q", got, want)
		}
	}
}
