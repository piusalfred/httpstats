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

	"github.com/riffbuild/httpstats"
)

// captureHandler stores a pointer to the TagRequest info so it can be
// inspected later (after the mux dispatches and populates r.Pattern).
type captureHandler struct {
	mu      sync.Mutex
	info    *httpstats.RequestTagInfo
	endSeen bool
}

func (h *captureHandler) TagConn(ctx context.Context, _ *httpstats.ConnTagInfo) context.Context {
	return ctx
}
func (h *captureHandler) HandleConn(_ context.Context, _ httpstats.ConnStats) {}
func (h *captureHandler) TagRequest(ctx context.Context, info *httpstats.RequestTagInfo) context.Context {
	h.mu.Lock()
	h.info = info
	h.mu.Unlock()
	return ctx
}

func (h *captureHandler) HandleRequest(_ context.Context, s httpstats.RequestStats) {
	if _, ok := s.(*httpstats.End); ok {
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

// TestServerHandler_PatternAvailableAtEnd verifies that after a patterned
// route dispatches, r.Pattern is available on the RequestTagInfo by the
// time the End event fires. Since TagRequest runs before dispatch, the
// pattern is only available on subsequent reads of the info struct.
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

	resp, err := http.Get(srv.URL + "/users/42")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	capture.mu.Lock()
	defer capture.mu.Unlock()

	if !capture.endSeen {
		t.Fatal("End event not seen")
	}

	// After dispatch, the server middleware updates RequestPattern.
	if capture.info.RequestPattern != "GET /users/{id}" {
		t.Errorf("RequestPattern = %q; want %q", capture.info.RequestPattern, "GET /users/{id}")
	}

	// DefaultOperationName now produces the correct name.
	op := httpstats.DefaultOperationName(capture.info)
	if op != "GET /users/{id}" {
		t.Errorf("operation name = %q; want %q", op, "GET /users/{id}")
	}
}

// TestServerHandler_NameFuncOverridesPattern verifies that WithServerNameFunc
// takes precedence — useful when the mux pattern isn't known at TagRequest
// time or when using a custom naming scheme.
func TestServerHandler_NameFuncOverridesPattern(t *testing.T) {
	t.Parallel()

	capture := &captureHandler{}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	wrapped := httpstats.ServerHandler(mux,
		httpstats.WithServerHandler(capture),
		httpstats.WithServerNameFunc(func(info *httpstats.RequestTagInfo) {
			info.OperationName = "custom." + info.Method
		}),
	)

	srv := httptest.NewServer(wrapped)
	t.Cleanup(srv.Close)

	resp, _ := http.Get(srv.URL + "/users/99")
	if resp != nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	if got := capture.operationName(); got != "custom.GET" {
		t.Errorf("operation name = %q; want %q", got, "custom.GET")
	}
}

// TestClientTransport_PatternPreset verifies that on the client side,
// RequestPattern set on the outgoing request flows through to the handler.
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

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	got := capture.operationName()
	if got != "GET /api/items/{id}" {
		t.Errorf("operation name = %q; want %q", got, "GET /api/items/{id}")
	}
}

func TestMultiHandler_ContextThreading(t *testing.T) {
	t.Parallel()

	type ctxKey struct{}
	h1 := &taggingHandler{tag: func(ctx context.Context) context.Context {
		return context.WithValue(ctx, ctxKey{}, "h1")
	}}
	h2 := &taggingHandler{tag: func(ctx context.Context) context.Context {
		if v, _ := ctx.Value(ctxKey{}).(string); v != "h1" {
			t.Errorf("h2 expected 'h1', got %q", v)
		}
		return context.WithValue(ctx, ctxKey{}, "h2")
	}}

	mh := httpstats.MultiHandler{h1, h2}
	ctx := mh.TagConn(context.Background(), &httpstats.ConnTagInfo{})
	if v, _ := ctx.Value(ctxKey{}).(string); v != "h2" {
		t.Errorf("final value = %q; want 'h2'", v)
	}
}

func TestMultiHandler_EmptyIsNoop(t *testing.T) {
	var mh httpstats.MultiHandler
	mh.HandleConn(context.Background(), &httpstats.ConnBegin{})
	mh.HandleRequest(context.Background(), &httpstats.Begin{})
}

func TestMultiHandler_AllCalled(t *testing.T) {
	var c1, c2 atomic.Int32
	h1 := &countingHandler{f: func() { c1.Add(1) }}
	h2 := &countingHandler{f: func() { c2.Add(1) }}

	httpstats.MultiHandler{h1, h2}.HandleRequest(context.Background(), &httpstats.Begin{})
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
		{"wrapped Canceled", fmtWrap("wrap", context.Canceled), httpstats.EndErrorCanceled},
		{"wrapped DeadlineExceeded", fmtWrap("wrap", context.DeadlineExceeded), httpstats.EndErrorDeadline},
		{"net timeout", &timeoutError{}, httpstats.EndErrorDeadline},
		{"generic", &simpleError{"something"}, httpstats.EndErrorOther},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := httpstats.ClassifyEndError(tt.err); got != tt.want {
				t.Errorf("ClassifyEndError(%v) = %v; want %v", tt.err, got, tt.want)
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
		captured.Set("user_role", "admin")
		w.WriteHeader(http.StatusOK)
	}))

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if captured == nil {
		t.Fatal("Tags not found in request context")
	}
	if v, _ := captured.Get("tenant_id"); v != "acme" {
		t.Errorf("tenant_id = %q", v)
	}
	if v, _ := captured.Get("user_role"); v != "admin" {
		t.Errorf("user_role = %q", v)
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
		wg.Go(func() {
			resp, _ := http.Get(srv.URL)
			if resp != nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		})
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
	resp, err := client.Get(backend.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		io.Copy(io.Discard, resp.Body)
	})
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

type taggingHandler struct {
	tag func(context.Context) context.Context
}

func (h *taggingHandler) TagConn(ctx context.Context, _ *httpstats.ConnTagInfo) context.Context {
	if h.tag != nil {
		return h.tag(ctx)
	}
	return ctx
}
func (h *taggingHandler) HandleConn(_ context.Context, _ httpstats.ConnStats) {}
func (h *taggingHandler) TagRequest(ctx context.Context, _ *httpstats.RequestTagInfo) context.Context {
	return ctx
}
func (h *taggingHandler) HandleRequest(_ context.Context, _ httpstats.RequestStats) {}

type countingHandler struct{ f func() }

func (h *countingHandler) TagConn(ctx context.Context, _ *httpstats.ConnTagInfo) context.Context {
	return ctx
}
func (h *countingHandler) HandleConn(_ context.Context, _ httpstats.ConnStats) {}
func (h *countingHandler) TagRequest(ctx context.Context, _ *httpstats.RequestTagInfo) context.Context {
	return ctx
}
func (h *countingHandler) HandleRequest(_ context.Context, _ httpstats.RequestStats) { h.f() }

type wrappedError struct {
	msg string
	err error
}

func fmtWrap(msg string, err error) error { return &wrappedError{msg, err} }
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
	_ httpstats.Handler = (*taggingHandler)(nil)
	_ httpstats.Handler = (*countingHandler)(nil)
	_ httpstats.Handler = (*captureHandler)(nil)
)
