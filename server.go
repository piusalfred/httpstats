// MIT License
//
// Copyright (c) 2026 Pius Alfred
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package httpstats

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type ServerOption func(*serverConfig)

type serverConfig struct {
	handler  Handler
	nameFunc func(*RequestTagInfo)
}

// WithServerHandler sets the stats handler for the middleware. Multiple
// handlers are composed via MultiHandler so each receives every event.
func WithServerHandler(h ...Handler) ServerOption {
	return func(c *serverConfig) {
		switch len(h) {
		case 0:
		case 1:
			c.handler = h[0]
		default:
			c.handler = MultiHandler(h)
		}
	}
}

// WithServerNameFunc sets a function that populates RequestTagInfo.OperationName.
//
//	httpstats.ServerHandler(mux, h,
//	    httpstats.WithServerNameFunc(func(info *httpstats.RequestTagInfo) {
//	        info.OperationName = info.Method + " " + routePattern
//	    }),
//	)
func WithServerNameFunc(f func(*RequestTagInfo)) ServerOption {
	return func(c *serverConfig) { c.nameFunc = f }
}

// ServerHandler wraps an http.Handler with the full request lifecycle:
//
//	TagRequest → Begin → InHeader → InData* → OutHeader → OutData* → End
//
// If the wrapped handler panics, End fires with a panic error and the panic is
// re-raised untouched, so net/http (and any recovery middleware above) behaves
// exactly as without the wrapper, including for http.ErrAbortHandler. A handler
// that returns without writing gets an OutHeader for the implicit 200.
//
// The ResponseWriter handed to the handler keeps http.Flusher, http.Hijacker
// and http.Pusher if and only if the server's writer has them, and also
// works with http.ResponseController.
func ServerHandler(base http.Handler, opts ...ServerOption) http.Handler {
	cfg := &serverConfig{handler: NoopHandler{}}
	for _, o := range opts {
		o(cfg)
	}
	return &serverHandler{base: base, handler: cfg.handler, nameFunc: cfg.nameFunc}
}

type serverHandler struct {
	base     http.Handler
	handler  Handler
	nameFunc func(*RequestTagInfo)
}

func (sh *serverHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, state := withRequestState(InjectTags(r.Context(), NewTags()))

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	info := &RequestTagInfo{
		Client:         false,
		Method:         r.Method,
		Host:           r.Host,
		URL:            r.URL,
		RequestPattern: r.Pattern,
		Header:         r.Header,
		Proto:          r.Proto,
		Scheme:         scheme,
		RemoteAddr:     r.RemoteAddr,
	}
	if sh.nameFunc != nil {
		sh.nameFunc(info)
	}

	var ended bool
	// end fires the End event exactly once. The route comes from what routers
	// recorded; http.Request.Pattern only stands in when no router group took
	// the request, because a group's root mux pattern ("/api/") is not a route.
	end := func(endErr error) {
		if ended {
			return
		}
		ended = true

		// Keep the tag info current for handlers that retained the pointer.
		if r.Pattern != "" {
			info.RequestPattern = r.Pattern
		}

		route := state.route()
		if route.Pattern == "" && route.Group == "" && r.Pattern != "" {
			route.Pattern = patternPath(r.Pattern)
		}
		sh.handler.HandleRequest(ctx, &End{
			Client:       false,
			EndTime:      time.Now(),
			Error:        endErr,
			Route:        route,
			HandlerError: state.handlerErr(),
		})
	}

	// A panic is reported and then re-raised: swallowing it would change what
	// net/http does (log it, or stay quiet for http.ErrAbortHandler, and drop
	// the connection) and would hide the failure from the caller's own recovery
	// middleware.
	defer func() {
		p := recover()
		if p == nil {
			return
		}
		func() {
			defer func() { _ = recover() }()
			end(&panicError{val: p})
		}()
		panic(p)
	}()

	ctx = sh.handler.TagRequest(ctx, info)
	r = r.WithContext(ctx)
	rsw := newResponseStatsWriter(w, sh.handler, ctx)

	startTime := time.Now()
	sh.handler.HandleRequest(ctx, &Begin{Client: false, StartTime: startTime})
	sh.handler.HandleRequest(ctx, &InHeader{
		Client:        false,
		Header:        r.Header.Clone(),
		ContentLength: r.ContentLength,
		Method:        r.Method,
	})

	if r.Body != nil && r.Body != http.NoBody {
		r.Body = &bodyEventReader{
			inbound: true, client: false,
			body:    r.Body,
			handler: sh.handler,
			ctx:     ctx,
		}
	}

	sh.base.ServeHTTP(rsw.capable(), r)
	rsw.finish()

	// Context errors (client disconnect, deadline) take precedence over
	// write errors — a canceled context may cause the write to fail.
	endErr := r.Context().Err()
	if writeErr := rsw.writeError(); writeErr != nil && endErr == nil {
		endErr = writeErr
	}
	end(endErr)
}

// responseStatsWriter wraps http.ResponseWriter to fire OutHeader/OutData
// events. State is atomic or mutex-guarded because a handler may write from
// several goroutines (HTTP/2 full-duplex).
//
// capable returns the value handed to handlers: it exposes http.Flusher,
// http.Hijacker and http.Pusher exactly when the wrapped writer does, so type
// assertions behave as they would without the wrapper.
type responseStatsWriter struct {
	w           http.ResponseWriter
	handler     Handler
	ctx         context.Context
	wroteHeader atomic.Bool
	hijacked    atomic.Bool

	mu         sync.Mutex
	statusCode int
	err        error
}

func newResponseStatsWriter(
	w http.ResponseWriter,
	h Handler,
	ctx context.Context, //nolint:revive // ResponseWriter-first is idiomatic for HTTP middleware
) *responseStatsWriter {
	return &responseStatsWriter{
		w:          w,
		handler:    h,
		ctx:        ctx,
		statusCode: http.StatusOK,
	}
}

func (w *responseStatsWriter) Header() http.Header { return w.w.Header() }

func (w *responseStatsWriter) WriteHeader(code int) {
	// net/http panics on codes outside 100..999; let it, without recording.
	if code < 100 || code > 999 {
		w.w.WriteHeader(code)
		return
	}
	if w.wroteHeader.Load() {
		return
	}
	if code < 200 && code != http.StatusSwitchingProtocols {
		w.handler.HandleRequest(w.ctx, &OutHeader{
			Client:     false,
			StatusCode: code,
			Header:     w.w.Header().Clone(),
		})
		w.w.WriteHeader(code)
		return
	}
	if !w.emitHeader(code) {
		return
	}
	w.w.WriteHeader(code)
}

// emitHeader records the final status and fires OutHeader once. It reports
// whether this call was the one that sent the header.
func (w *responseStatsWriter) emitHeader(code int) bool {
	if w.wroteHeader.Swap(true) {
		return false
	}
	w.mu.Lock()
	w.statusCode = code
	w.mu.Unlock()
	w.handler.HandleRequest(w.ctx, &OutHeader{
		Client:        false,
		StatusCode:    code,
		Header:        w.w.Header().Clone(),
		ContentLength: contentLengthFromHeader(w.w.Header()),
	})

	return true
}

// finish accounts for the 200 that net/http sends when a handler returns
// without writing anything. A hijacked connection has no response to account for.
func (w *responseStatsWriter) finish() {
	if w.hijacked.Load() {
		return
	}
	w.emitHeader(http.StatusOK)
}

func (w *responseStatsWriter) writeError() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.err
}

func (w *responseStatsWriter) recordWrite(n int, err error) {
	if n > 0 {
		w.handler.HandleRequest(w.ctx, &OutData{Client: false, Length: n})
	}
	if err == nil {
		return
	}
	w.mu.Lock()
	if w.err == nil {
		w.err = err
	}
	w.mu.Unlock()
	w.handler.HandleRequest(w.ctx, &OutData{Client: false, Length: 0, Error: err})
}

func (w *responseStatsWriter) Write(b []byte) (int, error) {
	w.emitHeader(http.StatusOK)
	n, err := w.w.Write(b)
	w.recordWrite(n, err)

	return n, err //nolint:wrapcheck // returning ResponseWriter error directly
}

// WriteString implements io.StringWriter like net/http's own writers, saving
// io.WriteString a []byte copy.
func (w *responseStatsWriter) WriteString(s string) (int, error) {
	w.emitHeader(http.StatusOK)
	var (
		n   int
		err error
	)
	if sw, ok := w.w.(io.StringWriter); ok {
		n, err = sw.WriteString(s)
	} else {
		n, err = w.w.Write([]byte(s))
	}
	w.recordWrite(n, err)

	return n, err //nolint:wrapcheck // returning ResponseWriter error directly
}

func (w *responseStatsWriter) Unwrap() http.ResponseWriter { return w.w }

// FlushError is what http.ResponseController looks for. It sends the header
// like Flush does, then flushes whatever the wrapped chain supports.
func (w *responseStatsWriter) FlushError() error {
	w.emitHeader(http.StatusOK)

	return http.NewResponseController(w.w).Flush() //nolint:wrapcheck // delegating
}

// ReadFrom implements io.ReaderFrom so io.Copy and http.ServeFile can
// use the sendfile syscall instead of falling back to user-space
// buffering. Without this, serving static files through the wrapper
// silently degrades throughput and spikes CPU.
func (w *responseStatsWriter) ReadFrom(r io.Reader) (int64, error) {
	w.emitHeader(http.StatusOK)
	if rf, ok := w.w.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(r)
		w.recordWrite(int(n), err)

		return n, err //nolint:wrapcheck // delegating to underlying io.ReaderFrom
	}

	// Hide ReadFrom so io.Copy does not call straight back into this method.
	return io.Copy(struct{ io.Writer }{w}, r) //nolint:wrapcheck // fallback to user-space Copy
}

type flushPart struct{ rsw *responseStatsWriter }

func (f flushPart) Flush() {
	f.rsw.emitHeader(http.StatusOK)
	f.rsw.w.(http.Flusher).Flush() //nolint:forcetypeassert,errcheck // chosen only when supported
}

type hijackPart struct{ rsw *responseStatsWriter }

func (h hijackPart) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := h.rsw.w.(http.Hijacker).Hijack() //nolint:forcetypeassert,errcheck // chosen only when supported
	if err == nil {
		h.rsw.hijacked.Store(true)
	}

	return conn, rw, err //nolint:wrapcheck // returning Hijack error directly
}

type pushPart struct{ rsw *responseStatsWriter }

func (p pushPart) Push(target string, opts *http.PushOptions) error {
	pusher := p.rsw.w.(http.Pusher) //nolint:forcetypeassert,errcheck // chosen only when supported

	return pusher.Push(target, opts) //nolint:wrapcheck // returning Push error directly
}

// capable returns w with exactly the optional interfaces of the wrapped
// writer: Flusher, Hijacker and Pusher. (http.CloseNotifier is deprecated and
// deliberately not carried; use the request context.)
func (w *responseStatsWriter) capable() http.ResponseWriter {
	_, f := w.w.(http.Flusher)
	_, h := w.w.(http.Hijacker)
	_, p := w.w.(http.Pusher)

	switch {
	case f && h && p:
		return struct {
			*responseStatsWriter
			flushPart
			hijackPart
			pushPart
		}{w, flushPart{w}, hijackPart{w}, pushPart{w}}
	case f && h:
		return struct {
			*responseStatsWriter
			flushPart
			hijackPart
		}{w, flushPart{w}, hijackPart{w}}
	case f && p:
		return struct {
			*responseStatsWriter
			flushPart
			pushPart
		}{w, flushPart{w}, pushPart{w}}
	case h && p:
		return struct {
			*responseStatsWriter
			hijackPart
			pushPart
		}{w, hijackPart{w}, pushPart{w}}
	case f:
		return struct {
			*responseStatsWriter
			flushPart
		}{w, flushPart{w}}
	case h:
		return struct {
			*responseStatsWriter
			hijackPart
		}{w, hijackPart{w}}
	case p:
		return struct {
			*responseStatsWriter
			pushPart
		}{w, pushPart{w}}
	default:
		return w
	}
}

var (
	_ interface{ Unwrap() http.ResponseWriter } = (*responseStatsWriter)(nil)
	_ io.ReaderFrom                             = (*responseStatsWriter)(nil)
	_ io.StringWriter                           = (*responseStatsWriter)(nil)
)

// bodyEventReader wraps io.ReadCloser to fire InData/OutData on each Read.
// Non-EOF errors are reported as separate events with Length=0 and Error set.
type bodyEventReader struct {
	body    io.ReadCloser
	handler Handler
	ctx     context.Context
	client  bool
	inbound bool
}

func (r *bodyEventReader) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if n > 0 {
		if r.inbound {
			r.handler.HandleRequest(r.ctx, &InData{Client: r.client, Length: n})
		} else {
			r.handler.HandleRequest(r.ctx, &OutData{Client: r.client, Length: n})
		}
	}
	if err != nil && err != io.EOF {
		if r.inbound {
			r.handler.HandleRequest(r.ctx, &InData{Client: r.client, Length: 0, Error: err})
		} else {
			r.handler.HandleRequest(r.ctx, &OutData{Client: r.client, Length: 0, Error: err})
		}
	}
	return n, err //nolint:wrapcheck // returning Read error directly
}

func (r *bodyEventReader) Close() error { return r.body.Close() } //nolint:wrapcheck // delegating to underlying Closer

func contentLengthFromHeader(h http.Header) int64 {
	if v := h.Get("Content-Length"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return -1
}

// ServerConnState returns a function for http.Server.ConnState that fires
// ConnBegin and ConnEnd for each connection lifecycle.
//
//	srv.ConnState = httpstats.ServerConnState(myHandler)
func ServerConnState(h Handler) func(net.Conn, http.ConnState) {
	if h == nil {
		h = NoopHandler{}
	}
	sc := &serverConnTracker{handler: h}
	return sc.connState
}

type serverConnTracker struct {
	handler Handler
	mu      sync.Mutex
	conns   map[net.Conn]context.Context
}

func (sc *serverConnTracker) connState(conn net.Conn, state http.ConnState) {
	switch state {
	case http.StateNew:
		ctx := context.Background()
		ctx = sc.handler.TagConn(ctx, &ConnTagInfo{
			Client:     false,
			RemoteAddr: conn.RemoteAddr(),
			LocalAddr:  conn.LocalAddr(),
		})
		sc.handler.HandleConn(ctx, &ConnBegin{
			Client:     false,
			StartTime:  time.Now(),
			RemoteAddr: conn.RemoteAddr().String(),
		})
		sc.mu.Lock()
		if sc.conns == nil {
			sc.conns = make(map[net.Conn]context.Context)
		}
		sc.conns[conn] = ctx //nolint:fatcontext // store connection context for lifecycle
		sc.mu.Unlock()

	case http.StateClosed, http.StateHijacked:
		sc.mu.Lock()
		ctx, ok := sc.conns[conn]
		if ok {
			delete(sc.conns, conn)
		}
		sc.mu.Unlock()
		if ok {
			sc.handler.HandleConn(ctx, &ConnEnd{Client: false})
		}
	}
}
