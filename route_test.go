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
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJoinPath(t *testing.T) {
	t.Parallel()

	tests := []struct{ base, p, want string }{
		{"", "/users/{id}", "/users/{id}"},
		{"/", "/users", "/users"},
		{"/users", "/{id}", "/users/{id}"},
		{"/users", "/", "/users"},
		{"/api", "files/{path...}", "/api/files/{path...}"},
		{"/api/v1", "/x/", "/api/v1/x/"},
		{"/users", "", "/users"},
		{"", "", ""},
	}
	for _, tt := range tests {
		if got := joinPath(tt.base, tt.p); got != tt.want {
			t.Errorf("joinPath(%q, %q) = %q; want %q", tt.base, tt.p, got, tt.want)
		}
	}
}

func TestPatternPath(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		"GET /users/{id}": "/users/{id}",
		"/users/":         "/users/",
		"POST  /x":        "/x",
		"example.com/a":   "example.com/a",
	} {
		if got := patternPath(in); got != want {
			t.Errorf("patternPath(%q) = %q; want %q", in, got, want)
		}
	}
}

// endCapture records the End event of every request.
type endCapture struct {
	NoopHandler
	ends chan *End
}

func newEndCapture() *endCapture { return &endCapture{ends: make(chan *End, 8)} }

func (c *endCapture) HandleRequest(_ context.Context, s RequestStats) {
	if e, ok := s.(*End); ok {
		c.ends <- e
	}
}

func serve(t *testing.T, h http.Handler, target string) (*End, *http.Response) {
	t.Helper()

	c := newEndCapture()
	srv := httptest.NewServer(ServerHandler(h, WithServerHandler(c)))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + target)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	return <-c.ends, resp
}

func TestEndRoute_GroupAndPattern(t *testing.T) {
	t.Parallel()

	inner := http.NewServeMux()
	inner.HandleFunc("GET /{id}", func(w http.ResponseWriter, r *http.Request) {
		SetRoute(r.Context(), "/{id}")
	})
	root := http.NewServeMux()
	root.Handle("/users/", http.StripPrefix("/users", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		AddGroup(r.Context(), "/users")
		inner.ServeHTTP(w, r)
	})))

	end, _ := serve(t, root, "/users/1234")
	if end.Route.Group != "/users" || end.Route.Pattern != "/users/{id}" {
		t.Errorf("Route = %+v; want group /users, pattern /users/{id}", end.Route)
	}
}

func TestEndRoute_UnmatchedInGroupKeepsGroupOnly(t *testing.T) {
	t.Parallel()

	root := http.NewServeMux()
	root.Handle("/auth/", http.StripPrefix("/auth", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		AddGroup(r.Context(), "/auth")
		http.NotFound(w, r)
	})))

	end, resp := serve(t, root, "/auth/whatever/123")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
	// The root mux pattern "/auth/" must not be reported as a route.
	if end.Route.Group != "/auth" || end.Route.Pattern != "" {
		t.Errorf("Route = %+v; want group /auth only", end.Route)
	}
}

func TestEndRoute_NoGroupFallsBackToRequestPattern(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", func(http.ResponseWriter, *http.Request) {})

	end, _ := serve(t, mux, "/items/9")
	if end.Route.Group != "" || end.Route.Pattern != "/items/{id}" {
		t.Errorf("Route = %+v", end.Route)
	}

	end, _ = serve(t, mux, "/nothing/1")
	if end.Route != (Route{}) {
		t.Errorf("unmatched Route = %+v; want zero", end.Route)
	}
}

func TestNestedGroupsAccumulate(t *testing.T) {
	t.Parallel()

	end, _ := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		AddGroup(r.Context(), "/api")
		AddGroup(r.Context(), "/v1")
		SetRoute(r.Context(), "/ping")
	}), "/")
	if end.Route.Group != "/api/v1" || end.Route.Pattern != "/api/v1/ping" {
		t.Errorf("Route = %+v", end.Route)
	}
}

func TestRouteHelpersWithoutServerHandlerAreNoops(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	AddGroup(ctx, "/x")
	SetRoute(ctx, "/y")
	SetError(ctx, errors.New("z"))
}

func TestEndHandlerError(t *testing.T) {
	t.Parallel()

	first, second := errors.New("first"), errors.New("second")
	end, _ := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetError(r.Context(), nil)
		SetError(r.Context(), first)
		SetError(r.Context(), second)
		w.WriteHeader(http.StatusInternalServerError)
	}), "/")
	if !errors.Is(end.HandlerError, first) {
		t.Errorf("HandlerError = %v; want first", end.HandlerError)
	}
}

// statusCapture keeps the OutHeader status codes.
type statusCapture struct {
	NoopHandler
	statuses []int
}

func (c *statusCapture) HandleRequest(_ context.Context, s RequestStats) {
	if o, ok := s.(*OutHeader); ok {
		c.statuses = append(c.statuses, o.StatusCode)
	}
}

func TestImplicit200WhenHandlerWritesNothing(t *testing.T) {
	t.Parallel()

	c := &statusCapture{}
	h := ServerHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), WithServerHandler(c))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if len(c.statuses) != 1 || c.statuses[0] != http.StatusOK {
		t.Errorf("OutHeader statuses = %v; want [200]", c.statuses)
	}
}

func TestNoImplicit200AfterExplicitStatus(t *testing.T) {
	t.Parallel()

	c := &statusCapture{}
	h := ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), WithServerHandler(c))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if len(c.statuses) != 1 || c.statuses[0] != http.StatusNoContent {
		t.Errorf("OutHeader statuses = %v; want [204]", c.statuses)
	}
}

func TestFlushSendsHeaderEvent(t *testing.T) {
	t.Parallel()

	c := &statusCapture{}
	h := ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush()
	}), WithServerHandler(c))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if len(c.statuses) != 1 || c.statuses[0] != http.StatusOK || !rec.Flushed {
		t.Errorf("statuses = %v flushed = %v", c.statuses, rec.Flushed)
	}
}

// bareWriter implements http.ResponseWriter and nothing else.
type bareWriter struct {
	h    http.Header
	body strings.Builder
}

func (b *bareWriter) Header() http.Header         { return b.h }
func (b *bareWriter) WriteHeader(int)             {}
func (b *bareWriter) Write(p []byte) (int, error) { return b.body.Write(p) }

func TestWriterCapabilitiesMirrorUnderlying(t *testing.T) {
	t.Parallel()

	var gotF, gotH, gotP bool
	probe := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, gotF = w.(http.Flusher)
		_, gotH = w.(http.Hijacker)
		_, gotP = w.(http.Pusher)
	})
	h := ServerHandler(probe)

	// A bare writer: none of the optional interfaces may appear.
	h.ServeHTTP(&bareWriter{h: http.Header{}}, httptest.NewRequest(http.MethodGet, "/", nil))
	if gotF || gotH || gotP {
		t.Errorf("bare writer: flusher=%v hijacker=%v pusher=%v; want all false", gotF, gotH, gotP)
	}

	// A recorder is a Flusher only.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !gotF || gotH || gotP {
		t.Errorf("recorder: flusher=%v hijacker=%v pusher=%v; want only flusher", gotF, gotH, gotP)
	}
}

type fullWriter struct {
	*httptest.ResponseRecorder
	hijacked bool
	pushed   string
}

func (f *fullWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	f.hijacked = true

	return nil, nil, nil
}

func (f *fullWriter) Push(target string, _ *http.PushOptions) error {
	f.pushed = target

	return nil
}

func TestHijackPushDelegate_AndHijackSkipsImplicit200(t *testing.T) {
	t.Parallel()

	fw := &fullWriter{ResponseRecorder: httptest.NewRecorder()}
	c := &statusCapture{}
	h := ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, _, err := w.(http.Hijacker).Hijack(); err != nil {
			t.Error(err)
		}
		if err := w.(http.Pusher).Push("/style.css", nil); err != nil {
			t.Error(err)
		}
		if _, ok := w.(http.Flusher); !ok {
			t.Error("Flusher lost")
		}
	}), WithServerHandler(c))
	h.ServeHTTP(fw, httptest.NewRequest(http.MethodGet, "/", nil))

	if !fw.hijacked || fw.pushed != "/style.css" {
		t.Errorf("hijacked=%v pushed=%q", fw.hijacked, fw.pushed)
	}
	if len(c.statuses) != 0 {
		t.Errorf("OutHeader fired for a hijacked connection: %v", c.statuses)
	}
}

func TestResponseControllerThroughWrapper(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	var flushErr error
	h := ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flushErr = http.NewResponseController(w).Flush()
	}))
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if flushErr != nil || !rec.Flushed {
		t.Errorf("Flush err=%v flushed=%v", flushErr, rec.Flushed)
	}
}

// Regression: ReadFrom on a writer without io.ReaderFrom used to call itself forever.
func TestReadFromWithoutUnderlyingReaderFrom(t *testing.T) {
	t.Parallel()

	bw := &bareWriter{h: http.Header{}}
	h := ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := io.Copy(w, &plainReader{data: "hello"}); err != nil {
			t.Error(err)
		}
		if _, err := io.WriteString(w, " world"); err != nil {
			t.Error(err)
		}
	}))
	h.ServeHTTP(bw, httptest.NewRequest(http.MethodGet, "/", nil))

	if bw.body.String() != "hello world" {
		t.Errorf("body = %q", bw.body.String())
	}
}

func TestInvalidStatusCodePanicsLikeNetHTTP(t *testing.T) {
	t.Parallel()

	c := &statusCapture{}
	h := ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer func() { _ = recover() }()
		w.WriteHeader(99)
	}), WithServerHandler(c))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	for _, s := range c.statuses {
		if s == 99 {
			t.Errorf("invalid status was recorded: %v", c.statuses)
		}
	}
}

func TestTagInfoCarriesProtoSchemeRemote(t *testing.T) {
	t.Parallel()

	var got RequestTagInfo
	h := ServerHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		WithServerHandler(tagSpy{fn: func(i *RequestTagInfo) { got = *i }}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got.Proto != "HTTP/1.1" || got.Scheme != "http" || got.RemoteAddr == "" {
		t.Errorf("info = %+v", got)
	}
}

type tagSpy struct {
	NoopHandler
	fn func(*RequestTagInfo)
}

func (s tagSpy) TagRequest(ctx context.Context, i *RequestTagInfo) context.Context {
	s.fn(i)

	return ctx
}
