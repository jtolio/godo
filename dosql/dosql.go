//go:build js && wasm

// Package dosql is a database/sql driver for the SQLite database of the
// Durable Object hosting the program (see durable.SQL).
//
// Every call is a synchronous call into the Durable Object's SQL API; results
// are fully materialized. Values map as follows:
//
//   - Integers must fit in ±2^53 because JavaScript numbers carry them.
//   - Numbers read back with an integral value are returned as int64, so a
//     REAL 2.0 scans as 2 (scanning into a float64 still works).
//   - bool is stored as 0/1, []byte as BLOB, time.Time as RFC 3339 UTC text
//     (it reads back as a string).
//
// Durable Objects forbid BEGIN/COMMIT statements, so database/sql
// transactions (Begin) are unsupported; use DB.Transaction instead.
package dosql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math"
	"syscall/js"
	"time"
)

// maxSafeInt is the largest integer a JavaScript number represents exactly.
const maxSafeInt = 1<<53 - 1

var errTx = errors.New("dosql: database/sql transactions are unsupported, use dosql.DB.Transaction")

// DB is a *sql.DB for a Durable Object's SQLite database.
type DB struct {
	*sql.DB
	host js.Value
}

// Open returns a DB backed by host, the value returned by durable.SQL.
func Open(host js.Value) *DB {
	return &DB{DB: sql.OpenDB(connector{host}), host: host}
}

// Transaction runs fn inside a SQLite transaction, committing if fn returns
// nil and rolling back otherwise. Every statement executed through db while
// fn runs is part of the transaction.
//
// fn must not block (on channels, sleeps, or JS promises): Durable Object
// transactions have to complete synchronously.
func (db *DB) Transaction(fn func() error) error {
	var fnErr error
	cb := js.FuncOf(func(js.Value, []js.Value) any {
		fnErr = fn()
		return fnErr != nil
	})
	defer cb.Release()
	if msg := db.host.Call("transaction", cb); msg.Truthy() {
		return fmt.Errorf("dosql: %s", msg.String())
	}
	return fnErr
}

type connector struct{ host js.Value }

func (c connector) Connect(context.Context) (driver.Conn, error) { return conn(c), nil }
func (connector) Driver() driver.Driver                          { return drv{} }

type drv struct{}

func (drv) Open(string) (driver.Conn, error) {
	return nil, errors.New("dosql: use dosql.Open instead of sql.Open")
}

type conn struct{ host js.Value }

func (c conn) Prepare(query string) (driver.Stmt, error) { return stmt{c, query}, nil }
func (conn) Close() error                                { return nil }
func (conn) Begin() (driver.Tx, error)                   { return nil, errTx }

func (c conn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	vals, err := values(args)
	if err != nil {
		return nil, err
	}
	return c.exec(query, vals)
}

func (c conn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	vals, err := values(args)
	if err != nil {
		return nil, err
	}
	return c.query(query, vals)
}

func (c conn) exec(query string, args []driver.Value) (driver.Result, error) {
	res, err := c.call("exec", query, args)
	if err != nil {
		return nil, err
	}
	return result{int64(res.Get("lastInsertId").Float()), int64(res.Get("rowsAffected").Float())}, nil
}

func (c conn) query(query string, args []driver.Value) (driver.Rows, error) {
	res, err := c.call("query", query, args)
	if err != nil {
		return nil, err
	}
	names := res.Get("columns")
	cols := make([]string, names.Length())
	for i := range cols {
		cols[i] = names.Index(i).String()
	}
	data := res.Get("rows")
	return &rows{cols: cols, data: data, n: data.Length()}, nil
}

func (c conn) call(method, query string, args []driver.Value) (js.Value, error) {
	params := make([]any, len(args))
	for i, a := range args {
		v, err := toJS(a)
		if err != nil {
			return js.Value{}, err
		}
		params[i] = v
	}
	res := c.host.Call(method, query, params)
	if msg := res.Get("error"); msg.Truthy() {
		return js.Value{}, fmt.Errorf("dosql: %s", msg.String())
	}
	return res, nil
}

func values(args []driver.NamedValue) ([]driver.Value, error) {
	vals := make([]driver.Value, len(args))
	for i, a := range args {
		if a.Name != "" {
			return nil, fmt.Errorf("dosql: named parameter %q unsupported", a.Name)
		}
		vals[i] = a.Value
	}
	return vals, nil
}

func toJS(v driver.Value) (any, error) {
	switch v := v.(type) {
	case int64:
		if v > maxSafeInt || v < -maxSafeInt {
			return nil, fmt.Errorf("dosql: integer %d exceeds ±2^53", v)
		}
		return v, nil
	case bool:
		if v {
			return 1, nil
		}
		return 0, nil
	case []byte:
		b := js.Global().Get("Uint8Array").New(len(v))
		js.CopyBytesToJS(b, v)
		return b.Get("buffer"), nil
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano), nil
	default: // nil, float64, string
		return v, nil
	}
}

func fromJS(v js.Value) driver.Value {
	switch v.Type() {
	case js.TypeNull, js.TypeUndefined:
		return nil
	case js.TypeNumber:
		if f := v.Float(); f == math.Trunc(f) && math.Abs(f) <= maxSafeInt {
			return int64(f)
		} else {
			return f
		}
	case js.TypeString:
		return v.String()
	default: // ArrayBuffer
		u := js.Global().Get("Uint8Array").New(v)
		b := make([]byte, u.Length())
		js.CopyBytesToGo(b, u)
		return b
	}
}

type stmt struct {
	c     conn
	query string
}

func (stmt) Close() error                                      { return nil }
func (stmt) NumInput() int                                     { return -1 }
func (s stmt) Exec(args []driver.Value) (driver.Result, error) { return s.c.exec(s.query, args) }
func (s stmt) Query(args []driver.Value) (driver.Rows, error)  { return s.c.query(s.query, args) }

type result struct{ id, n int64 }

func (r result) LastInsertId() (int64, error) { return r.id, nil }
func (r result) RowsAffected() (int64, error) { return r.n, nil }

type rows struct {
	cols []string
	data js.Value
	n, i int
}

func (r *rows) Columns() []string { return r.cols }
func (r *rows) Close() error      { return nil }

func (r *rows) Next(dest []driver.Value) error {
	if r.i >= r.n {
		return io.EOF
	}
	row := r.data.Index(r.i)
	for j := range dest {
		dest[j] = fromJS(row.Index(j))
	}
	r.i++
	return nil
}
