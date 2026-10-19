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

package httpstats //nolint:testpackage // tests access unexported types (responseStatsWriter, bodyEventReader)

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// recordingHandler is a test Handler that records all events in order.
type recordingHandler struct {
	NoopHandler
	mu     sync.Mutex
	events []recordedEvent
}

type recordedEvent struct {
	eventType  string // "TagConn", "HandleConn", "TagRequest", "HandleRequest"
	client     bool
	statsType  string // Begin, InHeader, OutHeader, InData, OutData, End
	statusCode int
	dataLength int
	dataError  error
	endError   error
	method     string
}

func (h *recordingHandler) TagConn(ctx context.Context, info *ConnTagInfo) context.Context {
	h.mu.Lock()
	h.events = append(h.events, recordedEvent{eventType: "TagConn", client: info.Client})
	h.mu.Unlock()
	return ctx
}

func (h *recordingHandler) HandleConn(ctx context.Context, stats ConnStats) {
	e := recordedEvent{eventType: "HandleConn"}
	switch s := stats.(type) {
	case *ConnBegin:
		e.client = s.Client
		e.statsType = "ConnBegin"
	case *ConnEnd:
		e.client = s.Client
		e.statsType = "ConnEnd"
	}
	h.mu.Lock()
	h.events = append(h.events, e)
	h.mu.Unlock()
}

func (h *recordingHandler) TagRequest(ctx context.Context, info *RequestTagInfo) context.Context {
	h.mu.Lock()
	h.events = append(h.events, recordedEvent{
		eventType: "TagRequest",
		client:    info.Client,
		method:    info.Method,
	})
	h.mu.Unlock()
	return ctx
}

func (h *recordingHandler) HandleRequest(ctx context.Context, stats RequestStats) {
	e := recordedEvent{eventType: "HandleRequest"}
	switch s := stats.(type) {
	case *Begin:
		e.client = s.Client
		e.statsType = "Begin"
	case *InHeader:
		e.client = s.Client
		e.statsType = "InHeader"
		e.statusCode = s.StatusCode
		e.method = s.Method
	case *OutHeader:
		e.client = s.Client
		e.statsType = "OutHeader"
		e.statusCode = s.StatusCode
	case *InData:
		e.client = s.Client
		e.statsType = "InData"
		e.dataLength = s.Length
		e.dataError = s.Error
	case *OutData:
		e.client = s.Client
		e.statsType = "OutData"
		e.dataLength = s.Length
		e.dataError = s.Error
	case *End:
		e.client = s.Client
		e.statsType = "End"
		e.endError = s.Error
	}
	h.mu.Lock()
	h.events = append(h.events, e)
	h.mu.Unlock()
}

func (h *recordingHandler) eventTypes() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.eventTypesUnsafe()
}

func (h *recordingHandler) eventTypesUnsafe() []string {
	types := make([]string, 0, len(h.events))
	for _, e := range h.events {
		if e.eventType == "HandleRequest" {
			types = append(types, e.statsType)
		} else {
			types = append(types, e.eventType)
		}
	}
	return types
}

func TestServerHandlerEventOrder(t *testing.T) {
	rec := &recordingHandler{}

	handler := ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello"))
	}), WithServerHandler(rec))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	want := []string{
		"TagRequest",
		"Begin",
		"InHeader",
		"OutHeader",
		"OutData",
		"End",
	}
	got := rec.eventTypes()
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("event[%d] = %q; want %q", i, got[i], w)
		}
	}
}

func TestServerHandlerStatusCodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
	}{
		{"200 OK", http.StatusOK},
		{"201 Created", http.StatusCreated},
		{"404 Not Found", http.StatusNotFound},
		{"500 Internal Server Error", http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := &recordingHandler{}
			handler := ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
			}), WithServerHandler(rec))

			srv := httptest.NewServer(handler)
			defer srv.Close()

			resp, err := http.Get(srv.URL)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)

			var outHdr *recordedEvent
			rec.mu.Lock()
			for i := range rec.events {
				if rec.events[i].statsType == "OutHeader" {
					outHdr = &rec.events[i]
					break
				}
			}
			rec.mu.Unlock()

			if outHdr == nil {
				t.Fatal("OutHeader event not found")
			}
			if outHdr.statusCode != tt.statusCode {
				t.Errorf("OutHeader.statusCode = %d; want %d", outHdr.statusCode, tt.statusCode)
			}
		})
	}
}

// quietServer starts an httptest server whose panic logging is silenced.
func quietServer(h http.Handler) *httptest.Server {
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.Start()

	return srv
}

// A panic is reported through End and then re-raised, so net/http drops the
// connection exactly as it would without the wrapper.
func TestServerHandlerPanicIsReported_AndReraised(t *testing.T) {
	rec := &recordingHandler{}

	handler := ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}), WithServerHandler(rec))

	srv := quietServer(handler)
	defer srv.Close()

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	if resp, err := client.Get(srv.URL); err == nil {
		resp.Body.Close()
		t.Fatalf("got status %d; want the connection dropped", resp.StatusCode)
	}

	var endEvent *recordedEvent
	rec.mu.Lock()
	for i := range rec.events {
		if rec.events[i].statsType == "End" {
			endEvent = &rec.events[i]
			break
		}
	}
	rec.mu.Unlock()

	if endEvent == nil {
		t.Fatal("End event not found after panic")
	}
	if kind := ClassifyEndError(endEvent.endError); kind != EndErrorPanic {
		t.Errorf("ClassifyEndError = %v; want EndErrorPanic", kind)
	}
}

func TestServerHandlerPanicErrAbortHandler(t *testing.T) {
	rec := &recordingHandler{}
	var logged bytes.Buffer

	srv := httptest.NewUnstartedServer(ServerHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}), WithServerHandler(rec)))
	srv.Config.ErrorLog = log.New(&logged, "", 0)
	srv.Start()
	defer srv.Close()

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	if resp, err := client.Get(srv.URL); err == nil {
		resp.Body.Close()
		t.Fatal("want the connection aborted")
	}

	if logged.Len() != 0 {
		t.Errorf("net/http logged an ErrAbortHandler panic: %q", logged.String())
	}
	if !slices.Contains(rec.eventTypes(), "End") {
		t.Error("End did not fire for ErrAbortHandler")
	}
}

func TestServerHandlerPanicRecovery_EventOrder(t *testing.T) {
	rec := &recordingHandler{}

	handler := ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Fire InHeader/OutHeader/OutData before panicking to verify
		// End still fires after the panic.
		w.Write([]byte("partial"))
		panic("late-panic")
	}), WithServerHandler(rec))

	srv := quietServer(handler)
	defer srv.Close()

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	if resp, err := client.Get(srv.URL); err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	types := rec.eventTypes()
	// Verify End appears last.
	lastIdx := -1
	for i, t := range types {
		if t == "End" {
			lastIdx = i
		}
	}
	if lastIdx == -1 {
		t.Fatal("End event not found in event order")
	}
	if lastIdx != len(types)-1 {
		t.Errorf("End is at position %d; want last position %d", lastIdx, len(types)-1)
	}
}

// TestServerHandler_EndAlwaysFires verifies that the End event fires after
// every request, whether the handler succeeds or returns early.
func TestServerHandler_EndAlwaysFires(t *testing.T) {
	rec := &recordingHandler{}
	handler := ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}), WithServerHandler(rec))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, _ := http.Get(srv.URL)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, e := range rec.events {
		if e.statsType == "End" {
			return
		}
	}
	t.Fatal("End event not found — should always fire after ServeHTTP")
}

func TestServerHandlerRequestBodyInData(t *testing.T) {
	rec := &recordingHandler{}

	handler := ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Write(body)
	}), WithServerHandler(rec))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	body := strings.Repeat("hello", 100) // 500 bytes
	resp, err := http.Post(srv.URL, "text/plain", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	var totalInData int
	for _, e := range rec.events {
		if e.statsType == "InData" && e.dataLength > 0 {
			totalInData += e.dataLength
		}
	}
	if totalInData != len(body) {
		t.Errorf("total InData length = %d; want %d", totalInData, len(body))
	}
}

func TestServerHandlerClientField(t *testing.T) {
	rec := &recordingHandler{}

	handler := ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}), WithServerHandler(rec))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, e := range rec.events {
		if e.client {
			t.Errorf("%s event has Client=true; want false", e.eventType)
		}
	}
}

func TestContentLengthFromHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header http.Header
		want   int64
	}{
		{"present", http.Header{"Content-Length": {"42"}}, 42},
		{"missing", http.Header{}, -1},
		{"invalid", http.Header{"Content-Length": {"not-a-number"}}, -1},
		{"zero", http.Header{"Content-Length": {"0"}}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := contentLengthFromHeader(tt.header)
			if got != tt.want {
				t.Errorf("contentLengthFromHeader = %d; want %d", got, tt.want)
			}
		})
	}
}

func TestServerHandler_1xxFiresOutHeader(t *testing.T) {
	rec := &recordingHandler{}
	handler := ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusContinue)
		w.WriteHeader(http.StatusProcessing)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("done"))
	}), WithServerHandler(rec))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, _ := http.Get(srv.URL)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	rec.mu.Lock()
	defer rec.mu.Unlock()

	var codes []int
	for _, e := range rec.events {
		if e.statsType == "OutHeader" {
			codes = append(codes, e.statusCode)
		}
	}
	if len(codes) != 3 {
		t.Fatalf("expected 3 OutHeader events (100, 102, 200), got %d: %v", len(codes), codes)
	}
	if codes[0] != 100 || codes[1] != 102 || codes[2] != 200 {
		t.Errorf("codes = %v; want [100 102 200]", codes)
	}
}

func TestServerConnState(t *testing.T) {
	rec := &recordingHandler{}
	srv := &http.Server{Addr: ":0", ConnState: ServerConnState(rec)}

	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go srv.Serve(ln)
	defer srv.Close()

	resp, _ := http.Get("http://" + ln.Addr().String() + "/")
	if resp != nil {
		resp.Body.Close()
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	var connBegin, connEnd bool
	for _, e := range rec.events {
		if e.statsType == "ConnBegin" {
			connBegin = true
		}
		if e.statsType == "ConnEnd" {
			connEnd = true
		}
	}
	if !connBegin {
		t.Error("ConnBegin not fired")
	}
	// ConnEnd fires asynchronously — may or may not fire before srv.Close().
	_ = connEnd
}

func TestServerConnState_NilHandler(t *testing.T) {
	fn := ServerConnState(nil)
	if fn == nil {
		t.Fatal("ServerConnState(nil) returned nil")
	}
}

func TestWithServerHandler_ZeroHandlers(t *testing.T) {
	handler := ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), WithServerHandler())
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, _ := http.Get(srv.URL)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
}

func TestWithClientTransport(t *testing.T) {
	tr := &http.Transport{}
	ct := NewClientTransport(WithClientTransport(tr))
	if ct.rt != tr {
		t.Error("WithClientTransport did not set the transport")
	}
}

func TestWithClientNameFunc(t *testing.T) {
	ct := NewClientTransport(WithClientNameFunc(func(info *RequestTagInfo) {
		info.OperationName = "custom"
	}))
	if ct.nameFunc == nil {
		t.Error("WithClientNameFunc did not set the name func")
	}
}

func TestNoopHandler(t *testing.T) {
	n := NoopHandler{}
	ctx := context.Background()
	if n.TagConn(ctx, &ConnTagInfo{}) != ctx {
		t.Error("TagConn should return same context")
	}
	n.HandleConn(ctx, &ConnBegin{})
	n.HandleRequest(ctx, &Begin{})
}

func TestResponseStatsWriter_ReadFrom(t *testing.T) {
	rec := &recordingHandler{}
	handler := ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// io.Copy detects io.ReaderFrom on the destination, but only if the
		// source does NOT implement io.WriterTo. Use a plain reader.
		n, err := io.Copy(w, &plainReader{data: "hello world"})
		if err != nil {
			t.Errorf("ReadFrom: %v", err)
		}
		if n != 11 {
			t.Errorf("ReadFrom wrote %d bytes; want 11", n)
		}
	}), WithServerHandler(rec))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, _ := http.Get(srv.URL)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	var total int
	for _, e := range rec.events {
		if e.statsType == "OutData" && e.dataLength > 0 {
			total += e.dataLength
		}
	}
	if total != 11 {
		t.Errorf("total OutData = %d; want 11", total)
	}
}

type plainReader struct {
	data string
	pos  int
}

func (r *plainReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func TestServerTagRequestSeesHeaders(t *testing.T) {
	h := &headerHandler{}
	srv := httptest.NewServer(ServerHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		WithServerHandler(h)))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Traceparent", "00-abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if got := h.seen.Get("Traceparent"); got != "00-abc" {
		t.Errorf("TagRequest saw Traceparent %q, want 00-abc", got)
	}
}
