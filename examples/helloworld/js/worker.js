import "../build/wasm_exec.js"; // defines globalThis.Go for whichever compiler built app.wasm
import wasm from "../build/app.wasm";
import { GoDurableObject } from "../../../js/durable.js";

// Requests to /u/<name>/<path> are routed to the Durable Object named <name>,
// which sees the request as /<path>. Each object owns a private SQLite database.
export default {
	fetch(request, env) {
		const url = new URL(request.url);
		const m = url.pathname.match(/^\/u\/([^/]+)(\/.*)?$/);
		if (!m) return new Response("not found\n", { status: 404 });
		url.pathname = m[2] ?? "/";
		return env.HELLO.getByName(decodeURIComponent(m[1])).fetch(new Request(url, request));
	},
};

// Hello runs one instance of the Go program in cmd/hello per Durable Object.
export class Hello extends GoDurableObject {
	constructor(ctx, env) {
		super(ctx, env, wasm);
	}
}
