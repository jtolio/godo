// Package sqldb defines database interfaces that let the same code run
// against a Durable Object's SQLite database in production and a regular
// *sql.DB (such as an in-memory SQLite, see package sqldbtest) in native
// tests.
//
// Two implementations exist: dosql.DB, adapted with Implicit, and *sql.DB,
// adapted with Native.
package sqldb

import (
	"database/sql"
	"fmt"
)

// MaxInt is the largest integer a Durable Object database stores, because
// JavaScript numbers carry integers exactly only up to 2^53. Integers from
// user input must be checked against it (and -MaxInt) before use in queries.
const MaxInt = 1<<53 - 1

// Querier runs SQL statements. *sql.DB, *sql.Tx and dosql.DB implement it.
type Querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// DB is a Querier that can also run transactions.
type DB interface {
	Querier
	// Transaction runs fn inside a transaction, committing if fn returns nil
	// and rolling back otherwise. fn must run its statements through the
	// Querier it is given and must not block (on channels, sleeps, or JS
	// promises), because Durable Object transactions complete synchronously.
	Transaction(fn func(Querier) error) error
}

// Native adapts a *sql.DB, running transactions with BeginTx.
func Native(d *sql.DB) DB { return native{d} }

type native struct{ *sql.DB }

func (d native) Transaction(fn func(Querier) error) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ImplicitTxDB is a database whose Transaction method includes every statement
// executed while fn runs, such as dosql.DB.
type ImplicitTxDB interface {
	Querier
	Transaction(fn func() error) error
}

// Implicit adapts an ImplicitTxDB.
func Implicit(d ImplicitTxDB) DB { return implicit{d} }

type implicit struct{ ImplicitTxDB }

func (d implicit) Transaction(fn func(Querier) error) error {
	return d.ImplicitTxDB.Transaction(func() error { return fn(d.ImplicitTxDB) })
}

// Migrate applies, in one transaction, every migration not yet recorded in
// the schema_migrations table. migrations[i] is schema version i+1, so the
// list must only ever be appended to.
func Migrate(d DB, migrations []string) error {
	return d.Transaction(func(q Querier) error {
		if _, err := q.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
			return err
		}
		var applied int
		if err := q.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&applied); err != nil {
			return err
		}
		for i := applied; i < len(migrations); i++ {
			version := i + 1
			if _, err := q.Exec(migrations[i]); err != nil {
				return fmt.Errorf("migration %d: %w", version, err)
			}
			if _, err := q.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, version); err != nil {
				return err
			}
		}
		return nil
	})
}
