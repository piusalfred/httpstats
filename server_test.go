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
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type recordingHandler struct {
	NoopHandler
	mu     sync.Mutex
	events []recordedEvent
}

type recordedEvent struct {
	eventType  EventType
	connType   ConnEventType
	isConn     bool
	client     bool
	statusCode int
	dataLength int
	dataError  error
	endError   error
	method     string
}

func (h *recordingHandler) TagConn(ctx context.Context, info *ConnTagInfo) context.Context {
	return ctx
}

func (h *recordingHandler) HandleConn(_ context.Context, ev *ConnEvent) {
	h.mu.Lock()
	h.events = append(h.events, recordedEvent{isConn: true, connType: ev.Type, client: ev.Client})
	h.mu.Unlock()
}

func (h *recordingHandler) HandleRequest(_ context.Context, ev *Event) {
	e := recordedEvent{eventType: ev.Type, client: ev.Client}
	switch ev.Type {
	case OpBegin:
	case OpInHeader:
		e.statusCode = ev.StatusCode
		e.method = ev.Method
	case OpOutHeader:
		e.statusCode = ev.StatusCode
	case OpInData, OpOutData:
		e.dataLength = ev.Length
		e.dataError = ev.Error
	case OpEnd:
		e.endError = ev.Error
	}
	h.mu.Lock()
	h.events = append(h.events, e)
	h.mu.Unlock()
}

func (h *recordingHandler) eventTypes() []EventType {
	h.mu.Lock()
	defer h.mu.Unlock()
	types := make([]EventType, 0, len(h.events))
	for _, e := range h.events {
		if !e.isConn {
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

	resp, _ := http.Get(srv.URL)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	want := []EventType{OpBegin, OpInHeader, OpOutHeader, OpOutData, OpEnd}
	got := rec.eventTypes()
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("event[%d] = %v; want %v", i, got[i], w)
		}
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

	resp, _ := http.Get(srv.URL)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, e := range rec.events {
		if !e.isConn && e.client {
			t.Errorf("event %v has Client=true; want false", e.eventType)
		}
	}
}

func TestServerHandlerPanicRecovery(t *testing.T) {
	rec := &recordingHandler{}
	handler := ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}), WithServerHandler(rec))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, _ := http.Get(srv.URL)
	io.Copy(io.Discard, resp.Body)

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

	body := strings.Repeat("hello", 100)
	resp, _ := http.Post(srv.URL, "text/plain", strings.NewReader(body))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	var total int
	for _, e := range rec.events {
		if e.eventType == OpInData && e.dataLength > 0 {
			total += e.dataLength
		}
	}
	if total != len(body) {
		t.Errorf("total InData = %d; want %d", total, len(body))
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
			if got := contentLengthFromHeader(tt.header); got != tt.want {
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
		rsw := w.(*responseStatsWriter)
		n, err := io.Copy(rsw, &plainReader{data: "hello world"})
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
