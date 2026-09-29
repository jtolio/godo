//go:build js && wasm

// Package durable connects a Go program to the GoDurableObject class in
// js/durable.js, which runs one instance of the program per Durable Object.
//
// Outgoing HTTP works through net/http: http.DefaultTransport on js/wasm
// uses the runtime's fetch under both Go and TinyGo, and importing this
// package makes http.DefaultClient (so http.Get, http.Post, ...) use it too.
// Other http.Clients need Transport: http.DefaultTransport, because TinyGo's
// Client ignores DefaultTransport and dials TCP, which Workers cannot do.
// Under TinyGo such a Client also leaves redirects to fetch, which follows
// them, and ignores Client.Timeout; use the request's context instead. Like
// Await, requests block the calling goroutine and must not be made inside a
// dosql.DB.Transaction callback.
package durable

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"syscall/js"
	"time"
)

// host is handed over by GoDurableObject through a global that only exists
// while the program starts, so it must be captured during package
// initialization.
var host = js.Global().Get("__goHost")

func init() {
	// See the package documentation. Standard Go already behaves this way.
	if http.DefaultClient.Transport == nil {
		http.DefaultClient.Transport = http.DefaultTransport
	}
}

func mustHost() js.Value {
	if host.IsUndefined() {
		panic("durable: not running inside a GoDurableObject")
	}
	return host
}

// SQL returns the object's SQLite host, for use with dosql.Open.
func SQL() js.Value { return mustHost() }

// Env returns the Worker environment (bindings, vars, secrets).
func Env() js.Value { return mustHost().Get("env") }

// ID returns the object's id as hex, as accepted by idFromString.
func ID() string { return mustHost().Get("id").String() }

// Await blocks the calling goroutine until promise settles, returning its
// value, or an error carrying the rejection's message. Other goroutines,
// including other requests, keep being served meanwhile.
//
// Await must not be called inside a dosql.DB.Transaction callback: Durable
// Object transactions have to complete synchronously.
func Await(promise js.Value) (js.Value, error) {
	type result struct {
		v   js.Value
		err error
	}
	done := make(chan result, 1) // buffered: the callback must not block
	cb := js.FuncOf(func(_ js.Value, args []js.Value) any {
		r := result{v: args[0]}
		if msg := args[1]; !msg.IsNull() {
			r.err = errors.New(msg.String())
		}
		done <- r
		return nil
	})
	defer cb.Release()
	mustHost().Call("settle", promise, cb)
	r := <-done
	return r.v, r.err
}

// OnAlarm registers fn to run, on its own goroutine, whenever the object's
// alarm fires. It must be called before Serve. If fn fails or panics, the
// runtime retries the alarm with backoff. Under TinyGo, which cannot recover
// on wasm, a panic also resets the object.
func OnAlarm(fn func() error) {
	mustHost().Call("onAlarm", js.FuncOf(func(_ js.Value, args []js.Value) any {
		done := args[0]
		go func() { done.Invoke(runAlarm(fn)) }()
		return nil
	}))
}

// runAlarm runs fn, returning its error's or panic's message, or nil if it
// succeeded.
func runAlarm(fn func() error) (msg any) {
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprintf("panic: %v", r)
		}
	}()
	if err := fn(); err != nil {
		return err.Error()
	}
	return nil
}

// Call calls obj's method with args and awaits the result. Unlike
// js.Value.Call, it returns an error rather than panicking if the method
// throws. Like Await, it must not be called inside a dosql.DB.Transaction
// callback.
func Call(obj js.Value, method string, args ...any) (js.Value, error) {
	return Await(mustHost().Call("call", append([]any{obj, method}, args...)...))
}

// SetAlarm schedules the object's alarm for t, replacing any pending one.
// Like Await, it must not be called inside a dosql.DB.Transaction callback.
func SetAlarm(t time.Time) error {
	if _, err := Call(mustHost().Get("storage"), "setAlarm", t.UnixMilli()); err != nil {
		return fmt.Errorf("setting alarm: %w", err)
	}
	return nil
}

// Serve handles the object's fetch requests with h. It never returns.
// Each request is served on its own goroutine.
//
// A panic in h is answered with a 500 under standard Go only. TinyGo cannot
// recover on wasm: the panic traps, failing every in-flight request, and the
// runtime resets the object. Handlers must therefore not panic on any input.
func Serve(h http.Handler) {
	mustHost().Call("serve", js.FuncOf(func(_ js.Value, args []js.Value) any {
		req, resolve := args[0], args[1]
		go func() { resolve.Invoke(serveJS(h, req)) }()
		return nil
	}))
	select {}
}

// serveJS runs h for a request of the form {method, url, headers: [[k, v]],
// body: Uint8Array | null} and returns {status, headers, body: Uint8Array}.
func serveJS(h http.Handler, jsReq js.Value) (res map[string]any) {
	w := &responseWriter{header: http.Header{}, status: http.StatusOK}
	defer func() {
		if r := recover(); r != nil {
			w = &responseWriter{header: http.Header{}, status: http.StatusInternalServerError}
			fmt.Fprintf(&w.body, "panic: %v\n", r)
			res = w.toJS()
		}
	}()

	var body io.Reader = http.NoBody
	if b := jsReq.Get("body"); !b.IsNull() {
		buf := make([]byte, b.Length())
		js.CopyBytesToGo(buf, b)
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(jsReq.Get("method").String(), jsReq.Get("url").String(), body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return w.toJS()
	}
	headers := jsReq.Get("headers")
	for i := range headers.Length() {
		kv := headers.Index(i)
		req.Header.Add(kv.Index(0).String(), kv.Index(1).String())
	}
	h.ServeHTTP(w, req)
	return w.toJS()
}

type responseWriter struct {
	header      http.Header
	status      int
	wroteHeader bool
	body        bytes.Buffer
}

func (w *responseWriter) Header() http.Header { return w.header }

func (w *responseWriter) WriteHeader(status int) {
	if !w.wroteHeader {
		w.status, w.wroteHeader = status, true
	}
}

func (w *responseWriter) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.body.Write(b)
}

func (w *responseWriter) toJS() map[string]any {
	var headers []any
	for k, vs := range w.header {
		for _, v := range vs {
			headers = append(headers, []any{k, v})
		}
	}
	body := js.Global().Get("Uint8Array").New(w.body.Len())
	js.CopyBytesToJS(body, w.body.Bytes())
	return map[string]any{"status": w.status, "headers": headers, "body": body}
}
