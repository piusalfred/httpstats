package httpstats //nolint:testpackage // tests access unexported types (responseStatsWriter, bodyEventReader)

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestServerHandlerPanicRecovery(t *testing.T) {
	rec := &recordingHandler{}

	handler := ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}), WithServerHandler(rec))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", resp.StatusCode)
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
	if endEvent.endError == nil {
		t.Error("End.Error is nil after panic; want panicError")
	}
	kind := ClassifyEndError(endEvent.endError)
	if kind != EndErrorPanic {
		t.Errorf("ClassifyEndError = %v; want EndErrorPanic", kind)
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

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
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

func TestServerHandler_ClientDisconnect(t *testing.T) {
	t.Skip("flaky: httptest buffers all writes, handler completes before client close")
	rec := &recordingHandler{}

	done := make(chan struct{})
	handler := ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		w.WriteHeader(http.StatusOK)
		for range 100 {
			if _, err := w.Write([]byte(strings.Repeat("x", 1024))); err != nil {
				return
			}
		}
	}), WithServerHandler(rec))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	// Wait for the handler to finish.
	<-done

	rec.mu.Lock()
	defer rec.mu.Unlock()
	var endEvent *recordedEvent
	for i := range rec.events {
		if rec.events[i].statsType == "End" {
			endEvent = &rec.events[i]
			break
		}
	}
	if endEvent == nil {
		t.Fatal("End event not found — should always fire after ServeHTTP")
	}
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
