// GoDurableObject hosts a Go program compiled to WebAssembly, one instance per
// Durable Object, for the Go packages github.com/jtolio/godo/durable and
// github.com/jtolio/godo/dosql. Export a subclass that passes the compiled
// module, after importing the wasm_exec.js that matches the compiler (the
// path to this file depends on where the library is checked out):
//
//	import "../build/wasm_exec.js";
//	import wasm from "../build/app.wasm";
//	import { GoDurableObject } from "../godo/js/durable.js";
//
//	export class MyObject extends GoDurableObject {
//		constructor(ctx, env) {
//			super(ctx, env, wasm);
//		}
//	}
import { DurableObject } from "cloudflare:workers";

// Rolls back a transactionSync callback without being reported as a failure.
const ROLLBACK = Symbol("rollback");

const errorMessage = (e) => {
	try {
		return String(e?.message ?? e);
	} catch {
		return "unprintable error"; // e.g. Object.create(null)
	}
};

// The Go program reads globalThis.__goHost during startup (go.run executes
// Go's init and main synchronously until main blocks), so each instance binds
// to its own storage. Host methods never throw: failures are returned as
// values because Go cannot reliably recover from JS exceptions under TinyGo.
export class GoDurableObject extends DurableObject {
	constructor(ctx, env, wasm) {
		super(ctx, env);
		const sql = ctx.storage.sql;
		let handler, alarmHandler;
		globalThis.__goHost = {
			env,
			id: ctx.id.toString(),
			serve(fn) {
				handler = fn;
			},
			onAlarm(fn) {
				alarmHandler = fn;
			},
			storage: ctx.storage,
			// Returns a promise of obj[method](...args), which rejects instead
			// of throwing on failure.
			call(obj, method, ...args) {
				return Promise.resolve().then(() => obj[method](...args));
			},
			// Calls cb(value, null) or cb(undefined, errorMessage) once value
			// (a promise or any other value) settles.
			settle(value, cb) {
				Promise.resolve(value).then(
					(v) => cb(v, null),
					(e) => cb(undefined, errorMessage(e)),
				);
			},
			query(query, params) {
				try {
					const cursor = sql.exec(query, ...params);
					const rows = cursor.raw().toArray();
					return { columns: cursor.columnNames, rows };
				} catch (e) {
					return { error: errorMessage(e) };
				}
			},
			exec(query, params) {
				try {
					sql.exec(query, ...params).toArray();
					return sql.exec("SELECT last_insert_rowid() AS lastInsertId, changes() AS rowsAffected").one();
				} catch (e) {
					return { error: errorMessage(e) };
				}
			},
			// fn returns true to request a rollback. Returns an error message if
			// the transaction failed for any other reason, otherwise null.
			transaction(fn) {
				try {
					ctx.storage.transactionSync(() => {
						if (fn()) throw ROLLBACK;
					});
					return null;
				} catch (e) {
					return e === ROLLBACK ? null : errorMessage(e);
				}
			},
		};
		try {
			const go = new Go();
			go.run(new WebAssembly.Instance(wasm, go.importObject));
		} finally {
			delete globalThis.__goHost;
		}
		if (!handler) throw new Error("Go program exited without calling durable.Serve");
		this.handler = handler;
		this.alarmHandler = alarmHandler;
	}

	// Throwing makes the runtime retry the alarm with backoff.
	async alarm() {
		if (!this.alarmHandler) return;
		const err = await new Promise((resolve) => this.alarmHandler(resolve));
		if (err) throw new Error(err);
	}

	async fetch(request) {
		const body = request.body ? new Uint8Array(await request.arrayBuffer()) : null;
		const req = { method: request.method, url: request.url, headers: [...request.headers], body };
		const res = await new Promise((resolve) => this.handler(req, resolve));
		return new Response(res.body.length ? res.body : null, { status: res.status, headers: res.headers });
	}
}
