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

func WithServerNameFunc(f func(*RequestTagInfo)) ServerOption {
	return func(c *serverConfig) { c.nameFunc = f }
}

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
	ctx := r.Context()
	ctx = InjectTags(ctx, NewTags())

	info := &RequestTagInfo{
		Client:         false,
		Method:         r.Method,
		Host:           r.Host,
		URL:            r.URL,
		RequestPattern: r.Pattern,
	}
	if sh.nameFunc != nil {
		sh.nameFunc(info)
	}

	var rsw *responseStatsWriter
	defer func() {
		if p := recover(); p != nil {
			if rsw != nil && !rsw.wroteHeader.Load() {
				rsw.WriteHeader(http.StatusInternalServerError)
			}
			ev := NewEvent()
			ev.Type = OpEnd
			ev.EndTime = time.Now()
			ev.Error = &panicError{val: p}
			sh.handler.HandleRequest(ctx, ev)
			PutEvent(ev)
		}
	}()

	ctx = sh.handler.TagRequest(ctx, info)
	r = r.WithContext(ctx)
	rsw = newResponseStatsWriter(w, sh.handler, ctx)

	ev := NewEvent()
	ev.Type = OpBegin
	ev.Client = false
	ev.StartTime = time.Now()
	sh.handler.HandleRequest(ctx, ev)
	PutEvent(ev)

	ev = NewEvent()
	ev.Type = OpInHeader
	ev.Client = false
	ev.Header = r.Header.Clone()
	ev.ContentLength = r.ContentLength
	ev.Method = r.Method
	sh.handler.HandleRequest(ctx, ev)
	PutEvent(ev)

	if r.Body != nil {
		r.Body = &bodyEventReader{
			inbound: true, client: false,
			body:    r.Body,
			handler: sh.handler,
			ctx:     ctx,
		}
	}

	sh.base.ServeHTTP(rsw, r)

	if r.Pattern != "" {
		info.RequestPattern = r.Pattern
	}

	endErr := r.Context().Err()
	if writeErr := rsw.writeErr; writeErr != nil && endErr == nil {
		endErr = writeErr
	}
	ev = NewEvent()
	ev.Type = OpEnd
	ev.Client = false
	ev.EndTime = time.Now()
	ev.Error = endErr
	sh.handler.HandleRequest(ctx, ev)
	PutEvent(ev)
}

type responseStatsWriter struct {
	w           http.ResponseWriter
	handler     Handler
	ctx         context.Context
	statusCode  int
	writeErr    error
	wroteHeader atomic.Bool
}

func newResponseStatsWriter(
	w http.ResponseWriter,
	h Handler,
	ctx context.Context, //nolint:revive // ResponseWriter-first is idiomatic
) *responseStatsWriter {
	return &responseStatsWriter{w: w, handler: h, ctx: ctx, statusCode: http.StatusOK}
}

func (w *responseStatsWriter) Header() http.Header { return w.w.Header() }

func (w *responseStatsWriter) WriteHeader(code int) {
	if w.wroteHeader.Load() {
		return
	}
	w.statusCode = code
	if code >= 100 && code < 200 {
		w.w.WriteHeader(code)
		return
	}
	if w.wroteHeader.Swap(true) {
		return
	}
	ev := NewEvent()
	ev.Type = OpOutHeader
	ev.Client = false
	ev.StatusCode = code
	ev.Header = w.w.Header().Clone()
	ev.ContentLength = contentLengthFromHeader(w.w.Header())
	w.handler.HandleRequest(w.ctx, ev)
	PutEvent(ev)
	w.w.WriteHeader(code)
}

func (w *responseStatsWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader.Load() {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.w.Write(b)
	if n > 0 {
		ev := NewEvent()
		ev.Type = OpOutData
		ev.Client = false
		ev.Length = n
		w.handler.HandleRequest(w.ctx, ev)
		PutEvent(ev)
	}
	if err != nil {
		if w.writeErr == nil {
			w.writeErr = err
		}
		ev := NewEvent()
		ev.Type = OpOutData
		ev.Client = false
		ev.Error = err
		w.handler.HandleRequest(w.ctx, ev)
		PutEvent(ev)
	}
	return n, err //nolint:wrapcheck // returning ResponseWriter error directly
}

func (w *responseStatsWriter) Unwrap() http.ResponseWriter { return w.w }

var _ interface{ Unwrap() http.ResponseWriter } = (*responseStatsWriter)(nil)

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
		ev := NewEvent()
		if r.inbound {
			ev.Type = OpInData
		} else {
			ev.Type = OpOutData
		}
		ev.Client = r.client
		ev.Length = n
		r.handler.HandleRequest(r.ctx, ev)
		PutEvent(ev)
	}
	if err != nil && err != io.EOF {
		ev := NewEvent()
		if r.inbound {
			ev.Type = OpInData
		} else {
			ev.Type = OpOutData
		}
		ev.Client = r.client
		ev.Error = err
		r.handler.HandleRequest(r.ctx, ev)
		PutEvent(ev)
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
		sc.handler.HandleConn(ctx, &ConnEvent{
			Type:       ConnOpBegin,
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
			sc.handler.HandleConn(ctx, &ConnEvent{Type: ConnOpEnd, Client: false})
		}
	}
}
