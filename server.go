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
// If the wrapped handler panics, the panic is recovered, End fires with a
// panicError, and a 500 response is written.
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
	ctx = InjectTags(ctx, NewTags(defaultTagsCapacity))

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
			func() {
				defer func() { _ = recover() }()
				sh.handler.HandleRequest(ctx, &End{
					Client:  false,
					EndTime: time.Now(),
					Error:   &panicError{val: p},
				})
			}()
		}
	}()

	ctx = sh.handler.TagRequest(ctx, info)
	r = r.WithContext(ctx)
	rsw = newResponseStatsWriter(w, sh.handler, ctx)

	startTime := time.Now()
	sh.handler.HandleRequest(ctx, &Begin{Client: false, StartTime: startTime})
	sh.handler.HandleRequest(ctx, &InHeader{
		Client:        false,
		Header:        r.Header.Clone(),
		ContentLength: r.ContentLength,
		Method:        r.Method,
	})

	if r.Body != nil {
		r.Body = &bodyEventReader{
			inbound: true, client: false,
			body:    r.Body,
			handler: sh.handler,
			ctx:     ctx,
		}
	}

	sh.base.ServeHTTP(rsw, r)

	// The mux may have populated r.Pattern during dispatch (Go 1.22+).
	// Capture it so backends can use it for span/metric names.
	if r.Pattern != "" {
		info.RequestPattern = r.Pattern
	}

	// Context errors (client disconnect, deadline) take precedence over
	// write errors — a canceled context may cause the write to fail.
	endErr := r.Context().Err()
	if writeErr := rsw.writeErr; writeErr != nil && endErr == nil {
		endErr = writeErr
	}
	sh.handler.HandleRequest(ctx, &End{
		Client:  false,
		EndTime: time.Now(),
		Error:   endErr,
	})
}

// responseStatsWriter wraps http.ResponseWriter to fire OutHeader/OutData
// events. wroteHeader is atomic to avoid races under concurrent Write calls
// (HTTP/2 full-duplex).
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
	w.handler.HandleRequest(w.ctx, &OutHeader{
		Client:        false,
		StatusCode:    code,
		Header:        w.w.Header().Clone(),
		ContentLength: contentLengthFromHeader(w.w.Header()),
	})
	w.w.WriteHeader(code)
}

func (w *responseStatsWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader.Load() {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.w.Write(b)
	if n > 0 {
		w.handler.HandleRequest(w.ctx, &OutData{Client: false, Length: n})
	}
	if err != nil {
		if w.writeErr == nil {
			w.writeErr = err
		}
		w.handler.HandleRequest(w.ctx, &OutData{Client: false, Length: 0, Error: err})
	}
	return n, err //nolint:wrapcheck // returning ResponseWriter error directly
}

func (w *responseStatsWriter) Unwrap() http.ResponseWriter { return w.w }

var _ interface{ Unwrap() http.ResponseWriter } = (*responseStatsWriter)(nil)

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
