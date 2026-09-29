package sqldb_test

import (
	"errors"
	"testing"

	"github.com/jtolio/godo/sqldb"
	"github.com/jtolio/godo/sqldb/sqldbtest"
)

var migrations = []string{
	`CREATE TABLE t (n INTEGER NOT NULL)`,
	`INSERT INTO t (n) VALUES (1)`,
}

func count(t *testing.T, q sqldb.Querier, query string) int {
	t.Helper()
	var n int
	if err := q.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMigrate(t *testing.T) {
	d := sqldbtest.Open(t)
	if err := sqldb.Migrate(d, migrations[:1]); err != nil {
		t.Fatal(err)
	}
	for range 2 { // the second run must be a no-op
		if err := sqldb.Migrate(d, migrations); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, d, `SELECT COUNT(*) FROM schema_migrations`); n != 2 {
		t.Errorf("recorded %d versions, want 2", n)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM t`); n != 1 {
		t.Errorf("t has %d rows, want 1", n)
	}
}

func TestMigrateFailureAppliesNothing(t *testing.T) {
	d := sqldbtest.Open(t)
	err := sqldb.Migrate(d, append(migrations, `NOT SQL`))
	if err == nil || err.Error()[:12] != "migration 3:" {
		t.Fatalf("got %v, want a migration 3 error", err)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM sqlite_schema WHERE name IN ('t', 'schema_migrations')`); n != 0 {
		t.Errorf("%d tables survived the failed migration", n)
	}
}

func TestTransactionRollback(t *testing.T) {
	d := sqldbtest.New(t, migrations)
	errBoom := errors.New("boom")
	err := d.Transaction(func(q sqldb.Querier) error {
		if _, err := q.Exec(`INSERT INTO t (n) VALUES (2)`); err != nil {
			return err
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("got %v, want %v", err, errBoom)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM t`); n != 1 {
		t.Errorf("rolled back insert persisted")
	}
}
