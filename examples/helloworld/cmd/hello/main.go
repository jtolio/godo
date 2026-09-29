//go:build js && wasm

// Command hello is a minimal Go program hosted in a Durable Object. Each
// object keeps its own visit counter in its private SQLite database.
package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/jtolio/godo/dosql"
	"github.com/jtolio/godo/durable"
)

var errRollback = errors.New("rolled back on purpose")

func main() {
	db := dosql.Open(durable.SQL())
	// Durable Object storage is usable synchronously while the object starts.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS visits (n INTEGER NOT NULL);
		INSERT INTO visits SELECT 0 WHERE NOT EXISTS (SELECT 1 FROM visits)`); err != nil {
		panic(err)
	}
	fetchURL := durable.Env().Get("FETCH_URL").String()

	// Routes are matched by hand because TinyGo's net/http fork predates
	// ServeMux method and wildcard patterns.
	durable.Serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/rollback":
			count(w, db, r.URL.Path == "/rollback")
		case "/fetch":
			fetch(w, fetchURL)
		default:
			http.NotFound(w, r)
		}
	}))
}

// count increments the visit counter in a transaction, rolling it back if
// rollback is set.
func count(w http.ResponseWriter, db *dosql.DB, rollback bool) {
	var n int64
	err := db.Transaction(func() error {
		if _, err := db.Exec("UPDATE visits SET n = n + 1"); err != nil {
			return err
		}
		if err := db.QueryRow("SELECT n FROM visits").Scan(&n); err != nil || !rollback {
			return err
		}
		return errRollback
	})
	switch {
	case rollback && errors.Is(err, errRollback):
		fmt.Fprintf(w, "Counted visit #%d inside the transaction, then rolled it back.\n", n)
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		fmt.Fprintf(w, "Hello from Go! Visit #%d.\n", n)
	}
}

// fetch reports the result of a GET of url. net/http's default transport
// uses the runtime's fetch, so no Workers-specific client is needed.
func fetch(w http.ResponseWriter, url string) {
	res, err := http.Get(url)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer res.Body.Close()
	n, err := io.Copy(io.Discard, res.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	fmt.Fprintf(w, "GET %s: %s, %d bytes.\n", url, res.Status, n)
}
