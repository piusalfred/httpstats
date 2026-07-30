package httpstats //nolint:testpackage // tests access unexported types

import (
	"context"
	"io"
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
	resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", resp.StatusCode)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	var endEvent *recordedEvent
	for i := range rec.events {
		if rec.events[i].eventType == OpEnd {
			endEvent = &rec.events[i]
			break
		}
	}
	if endEvent == nil {
		t.Fatal("End event not found after panic")
	}
	if endEvent.endError == nil {
		t.Error("End.Error is nil after panic")
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
