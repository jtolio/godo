// Package sqldbtest provides fresh in-memory SQLite databases for native
// tests of code written against package sqldb.
package sqldbtest

import (
	"database/sql"
	"testing"

	_ "github.com/ncruces/go-sqlite3/driver"

	"github.com/jtolio/godo/sqldb"
)

// Open returns an empty in-memory database, closed when t ends.
//
// It holds a single connection, because every connection to ":memory:" is a
// separate database. As a consequence, running a statement on the DB rather
// than on the Querier passed to Transaction deadlocks, which surfaces misuse
// that the Durable Object database would silently tolerate.
func Open(t testing.TB) sqldb.DB {
	t.Helper()
	d, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	d.SetMaxOpenConns(1)
	t.Cleanup(func() { d.Close() })
	return sqldb.Native(d)
}

// New returns a fresh in-memory database with migrations applied (see
// sqldb.Migrate).
func New(t testing.TB, migrations []string) sqldb.DB {
	t.Helper()
	d := Open(t)
	if err := sqldb.Migrate(d, migrations); err != nil {
		t.Fatal(err)
	}
	return d
}
