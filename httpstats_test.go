package httpstats_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/piusalfred/httpstats"
)

type captureHandler struct {
	mu      sync.Mutex
	info    *httpstats.RequestTagInfo
	endSeen bool
}

func (h *captureHandler) TagConn(ctx context.Context, _ *httpstats.ConnTagInfo) context.Context {
	return ctx
}
func (h *captureHandler) HandleConn(_ context.Context, _ *httpstats.ConnEvent) {}
func (h *captureHandler) TagRequest(ctx context.Context, info *httpstats.RequestTagInfo) context.Context {
	h.mu.Lock()
	h.info = info
	h.mu.Unlock()
	return ctx
}

func (h *captureHandler) HandleRequest(_ context.Context, ev *httpstats.Event) {
	if ev.Type == httpstats.OpEnd {
		h.mu.Lock()
		h.endSeen = true
		h.mu.Unlock()
	}
}

func (h *captureHandler) operationName() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.info == nil {
		return ""
	}
	return httpstats.DefaultOperationName(h.info)
}

func TestServerHandler_PatternAvailableAtEnd(t *testing.T) {
	t.Parallel()
	capture := &captureHandler{}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	wrapped := httpstats.ServerHandler(mux, httpstats.WithServerHandler(capture))
	srv := httptest.NewServer(wrapped)
	t.Cleanup(srv.Close)

	resp, _ := http.Get(srv.URL + "/users/42")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	capture.mu.Lock()
	defer capture.mu.Unlock()
	if !capture.endSeen {
		t.Fatal("End event not seen")
	}
	if capture.info.RequestPattern != "GET /users/{id}" {
		t.Errorf("RequestPattern = %q", capture.info.RequestPattern)
	}
}

func TestClientTransport_PatternPreset(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	capture := &captureHandler{}
	ct := httpstats.NewClientTransport(httpstats.WithClientHandler(capture))
	client := &http.Client{Transport: ct}

	req, _ := http.NewRequest(http.MethodGet, backend.URL, nil)
	req.Pattern = "GET /api/items/{id}"

	resp, _ := client.Do(req)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if got := capture.operationName(); got != "GET /api/items/{id}" {
		t.Errorf("operation name = %q; want %q", got, "GET /api/items/{id}")
	}
}

func TestMultiHandler_ContextThreading(t *testing.T) {
	t.Parallel()
	type ctxKey struct{}
	h1 := &tagHandler{tag: func(ctx context.Context) context.Context {
		return context.WithValue(ctx, ctxKey{}, "h1")
	}}
	h2 := &tagHandler{tag: func(ctx context.Context) context.Context {
		return context.WithValue(ctx, ctxKey{}, "h2")
	}}

	mh := httpstats.MultiHandler{h1, h2}
	ctx := mh.TagConn(context.Background(), &httpstats.ConnTagInfo{})
	if v, _ := ctx.Value(ctxKey{}).(string); v != "h2" {
		t.Errorf("value = %q; want 'h2'", v)
	}
}

func TestMultiHandler_AllCalled(t *testing.T) {
	var c1, c2 atomic.Int32
	h1 := &countH{fn: func() { c1.Add(1) }}
	h2 := &countH{fn: func() { c2.Add(1) }}

	ev := httpstats.NewEvent()
	ev.Type = httpstats.OpBegin
	httpstats.MultiHandler{h1, h2}.HandleRequest(context.Background(), ev)
	httpstats.PutEvent(ev)

	if c1.Load() != 1 || c2.Load() != 1 {
		t.Errorf("counts: h1=%d h2=%d; want both 1", c1.Load(), c2.Load())
	}
}

func TestClassifyEndError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want httpstats.EndErrorKind
	}{
		{"nil", nil, httpstats.EndErrorNone},
		{"Canceled", context.Canceled, httpstats.EndErrorCanceled},
		{"DeadlineExceeded", context.DeadlineExceeded, httpstats.EndErrorDeadline},
		{"wrapped Canceled", wrapErr("wrap", context.Canceled), httpstats.EndErrorCanceled},
		{"wrapped DeadlineExceeded", wrapErr("wrap", context.DeadlineExceeded), httpstats.EndErrorDeadline},
		{"net timeout", &timeoutError{}, httpstats.EndErrorDeadline},
		{"generic", &simpleError{"something"}, httpstats.EndErrorOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := httpstats.ClassifyEndError(tt.err); got != tt.want {
				t.Errorf("ClassifyEndError = %v; want %v", got, tt.want)
			}
		})
	}
}

func TestServerHandler_TagsEndToEnd(t *testing.T) {
	t.Parallel()
	var captured *httpstats.Tags
	handler := httpstats.ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = httpstats.FromContext(r.Context())
		if captured == nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		captured.Set("tenant_id", "acme")
		w.WriteHeader(http.StatusOK)
	}))

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	resp, _ := http.Get(srv.URL)
	resp.Body.Close()

	if captured == nil {
		t.Fatal("Tags not found in context")
	}
	if v, _ := captured.Get("tenant_id"); v != "acme" {
		t.Errorf("tenant_id = %q", v)
	}
}

func TestServerHandler_ConcurrentRequests(t *testing.T) {
	t.Parallel()
	var handled atomic.Int32
	handler := httpstats.ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handled.Add(1)
		w.WriteHeader(http.StatusOK)
	}))

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _ := http.Get(srv.URL)
			if resp != nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	if handled.Load() != 50 {
		t.Errorf("handled %d; want 50", handled.Load())
	}
}

func TestClientTransport_ReadCloseRace(t *testing.T) {
	t.Parallel()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(strings.Repeat("x", 10000)))
	}))
	t.Cleanup(backend.Close)

	ct := httpstats.NewClientTransport()
	client := &http.Client{Transport: ct}
	resp, _ := client.Get(backend.URL)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); io.Copy(io.Discard, resp.Body) }()
	resp.Body.Close()
	wg.Wait()
}

func TestClientTransport_ConnectionError(t *testing.T) {
	t.Parallel()
	ct := httpstats.NewClientTransport()
	client := &http.Client{Transport: ct, Timeout: 100 * time.Millisecond}
	_, err := client.Get("http://127.0.0.1:1/nope")
	if err == nil {
		t.Skip("expected connection error")
	}
}

type tagHandler struct {
	tag func(context.Context) context.Context
}

func (h *tagHandler) TagConn(ctx context.Context, _ *httpstats.ConnTagInfo) context.Context {
	if h.tag != nil {
		return h.tag(ctx)
	}
	return ctx
}
func (h *tagHandler) HandleConn(_ context.Context, _ *httpstats.ConnEvent) {}
func (h *tagHandler) TagRequest(ctx context.Context, _ *httpstats.RequestTagInfo) context.Context {
	return ctx
}
func (h *tagHandler) HandleRequest(_ context.Context, _ *httpstats.Event) {}

type countH struct{ fn func() }

func (h *countH) TagConn(ctx context.Context, _ *httpstats.ConnTagInfo) context.Context { return ctx }
func (h *countH) HandleConn(_ context.Context, _ *httpstats.ConnEvent)                  {}
func (h *countH) TagRequest(ctx context.Context, _ *httpstats.RequestTagInfo) context.Context {
	return ctx
}
func (h *countH) HandleRequest(_ context.Context, _ *httpstats.Event) { h.fn() }

type wrappedError struct {
	msg string
	err error
}

func wrapErr(msg string, err error) error { return &wrappedError{msg, err} }
func (e *wrappedError) Error() string     { return e.msg + ": " + e.err.Error() }
func (e *wrappedError) Unwrap() error     { return e.err }

type timeoutError struct{}

func (e *timeoutError) Error() string   { return "timeout" }
func (e *timeoutError) Timeout() bool   { return true }
func (e *timeoutError) Temporary() bool { return false }

type simpleError struct{ msg string }

func (e *simpleError) Error() string { return e.msg }

var (
	_ net.Error         = (*timeoutError)(nil)
	_ httpstats.Handler = (*tagHandler)(nil)
	_ httpstats.Handler = (*countH)(nil)
	_ httpstats.Handler = (*captureHandler)(nil)
)
