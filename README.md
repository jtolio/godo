# GoDO

GoDO is a support library for running Go programs in Cloudflare Durable Objects.
GoDO also provides a small Javascript Worker shim for routing requests to the appropriate Durable Object.
In this model, each object runs its own instance of your program, compiled to
WebAssembly with TinyGo or standard Go, and serves requests with an ordinary
`http.Handler`. The program reaches the object's SQLite database through `database/sql`.

This initial release is Opus 5.5 slop. Expect the API to be in flux.

A larger example app is here: https://github.com/jtolio/kcalvin

| Package | What it does |
| --- | --- |
| `durable` | Serves the object's requests (`Serve`), exposes its environment and id (`Env`, `ID`), awaits JS promises (`Await`, `Call`), and handles alarms (`OnAlarm`, `SetAlarm`). Makes `http.DefaultClient` use the runtime's `fetch`. |
| `dosql` | A `database/sql` driver for the object's SQLite database, with `Transaction` built on `transactionSync`. |
| `sqldb` | `Querier` and `DB` interfaces that `dosql` (via `Implicit`) and `*sql.DB` (via `Native`) both satisfy, a `Migrate` runner, and `MaxInt`. Lets the same code run in production and in native tests. |
| `sqldb/sqldbtest` | Fresh in-memory SQLite databases (ncruces/go-sqlite3) for native tests. |
| `js/durable.js` | `GoDurableObject`, the Durable Object class that hosts the program. |

`durable` and `dosql` build only for `js && wasm`. `sqldb` builds everywhere.

## Getting started

[`examples/helloworld`](examples/helloworld) is a complete project to copy. It has the
Makefile that builds with either compiler, the Wrangler config, and a Worker that routes
requests to one object per name. Its program keeps a visit counter in SQLite, shows a
transaction rolling back, and makes an outgoing HTTP request.

```sh
cd examples/helloworld
npm ci
npm run dev        # or: GO=go npm run dev
curl localhost:8787/u/alice/           # Hello from Go! Visit #1.
curl localhost:8787/u/alice/rollback   # counts #2 inside a transaction, then rolls it back
curl localhost:8787/u/alice/fetch      # GET https://example.com/ from Go
```

You need Go 1.25 or newer, TinyGo 0.42 or newer (unless you build with `GO=go`), Node.js and
make.

A project has three parts:

1. **A Go program** (`cmd/hello`), which opens the database, runs any migrations, registers
   handlers, and calls `durable.Serve`:

   ```go
   db := dosql.Open(durable.SQL())
   // ... create or migrate tables; storage is usable synchronously at startup ...
   durable.Serve(handler)
   ```

2. **A Worker** (`js/worker.js`), which picks the object for each request and exports your
   object class as a subclass of `GoDurableObject`:

   ```js
   import "../build/wasm_exec.js"; // must match the compiler that built app.wasm
   import wasm from "../build/app.wasm";
   import { GoDurableObject } from "path/to/godo/js/durable.js";

   export class Hello extends GoDurableObject {
   	constructor(ctx, env) {
   		super(ctx, env, wasm);
   	}
   }
   ```

3. **Build glue**: the example's `Makefile`, which writes `build/app.wasm` and the matching
   `build/wasm_exec.js`, and `wrangler.jsonc`, which runs `make` and declares the class with
   `new_sqlite_classes`.

The example uses the library through `replace github.com/jtolio/godo => ../..` in its
`go.mod`, and imports `durable.js` by relative path. A project that keeps godo elsewhere changes
both paths.

## Rules for programs

- **Don't block inside a transaction.** `dosql.DB.Transaction` wraps `transactionSync`, which
  must complete synchronously. Inside the callback, don't await promises, make HTTP requests,
  use channels or sleep. Do those before or after the transaction.
- **Don't use BEGIN, COMMIT or SAVEPOINT.** Durable Objects forbid them, so `database/sql`'s
  `Begin` returns an error. Use `Transaction`, which includes every statement run through the
  `DB` while the callback runs.
- **Integers must fit in ±2^53** (`sqldb.MaxInt`), because values pass through JavaScript
  numbers. Check integers from user input before using them in queries. Numbers with an integral
  value read back as `int64`. `time.Time` is stored as RFC 3339 text.
- **Handlers must not panic under TinyGo.** TinyGo can't `recover` on wasm, so a panic traps
  (`RuntimeError: unreachable`). That fails every in-flight request, and the runtime resets the
  object. Standard Go turns a handler panic into a 500 instead. `durable.Call` runs a JS method
  and returns an error instead of panicking when it throws.
- **Read the environment while starting.** `durable.Env()` returns the Worker's bindings, vars
  and secrets. A var or secret that isn't set is `undefined`, and calling `String()` on that
  returns `"<undefined>"`, so check `Type()` first.

## Outgoing HTTP

Use `net/http`. On js/wasm, `http.DefaultTransport` calls the runtime's `fetch` under both
compilers. Importing `durable` also makes `http.DefaultClient` use it, so `http.Get`,
`http.Post` and `http.DefaultClient.Do` work unchanged.

Under TinyGo, any other `http.Client` needs `Transport: http.DefaultTransport`. Without it,
TinyGo's client tries to dial TCP, which Workers can't do. Such a client also leaves redirects
to `fetch`, which follows them, and ignores `Client.Timeout`, so use the request's context for
deadlines. TinyGo's errors aren't wrapped in `*url.Error`.

## Testing natively

Write code that touches the database against `sqldb.Querier` and `sqldb.DB`. In production,
wrap the Durable Object database:

```go
d := sqldb.Implicit(dosql.Open(durable.SQL()))
if err := sqldb.Migrate(d, migrations); err != nil {
	panic(err)
}
```

In tests, `sqldbtest.New(t, migrations)` gives each test a fresh, migrated in-memory SQLite. It
has a single connection, so running a statement on the `DB` instead of on the `Querier` passed
to `Transaction` deadlocks. That turns a mistake Durable Object SQLite would silently tolerate
into a failing test. `migrations[i]` is schema version i+1. Only ever append to the list.

The two SQLites differ. For example, Durable Objects disallow some functions, such as
`sqlite_version()`. Also test every query against `wrangler dev`.

## Platform notes

These were verified with TinyGo 0.42, standard Go 1.26 and 1.27, and `wrangler dev`:

- The same database works with either compiler, so a project can switch freely.
- Under TinyGo, `html/template` doesn't work. `encoding/json`, `time/tzdata`, `crypto/sha256`,
  `crypto/rand` and [gomponents](https://www.gomponents.com/) do.
- TinyGo's `net/http` has no `ServeMux` method or wildcard patterns.
- Sizes, gzipped: the example is about 0.44 MB under TinyGo and 2.8 MB under standard Go. The
  Workers Free plan allows 3 MB per Worker, and larger standard Go programs exceed that, so
  deploy with TinyGo on Free.
- `durable.Await` blocks only its own goroutine. Other requests keep being served while a
  promise is pending, and rejections, including non-`Error` values, become Go errors.
- An alarm handler that returns an error or panics makes the runtime retry the alarm with
  backoff.
