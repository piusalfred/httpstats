package httpstats //nolint:testpackage // tests access unexported types

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestClientTransportEventOrder(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello"))
	}))
	defer backend.Close()

	rec := &recordingHandler{}
	ct := NewClientTransport(WithClientHandler(rec))
	client := &http.Client{Transport: ct}

	resp, _ := client.Get(backend.URL)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	want := []EventType{OpBegin, OpOutHeader, OpInHeader, OpInData, OpEnd}
	got := rec.eventTypes()
	if len(got) < len(want) {
		t.Fatalf("got %d events, want at least %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("event[%d] = %v; want %v", i, got[i], w)
		}
	}
}

func TestClientTransportClientField(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	rec := &recordingHandler{}
	ct := NewClientTransport(WithClientHandler(rec))
	client := &http.Client{Transport: ct}

	resp, _ := client.Get(backend.URL)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, e := range rec.events {
		if !e.isConn && !e.client {
			t.Errorf("event %v has Client=false; want true", e.eventType)
		}
	}
}

func TestClientTransportRequestBodyOutData(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	rec := &recordingHandler{}
	ct := NewClientTransport(WithClientHandler(rec))
	client := &http.Client{Transport: ct}

	body := strings.Repeat("data", 250)
	resp, _ := client.Post(backend.URL, "text/plain", strings.NewReader(body))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	var total int
	for _, e := range rec.events {
		if e.eventType == OpOutData && e.dataLength > 0 {
			total += e.dataLength
		}
	}
	if total != len(body) {
		t.Errorf("total OutData = %d; want %d", total, len(body))
	}
}

func TestClientTransportResponseBodyInData(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(strings.Repeat("x", 500)))
	}))
	defer backend.Close()

	rec := &recordingHandler{}
	ct := NewClientTransport(WithClientHandler(rec))
	client := &http.Client{Transport: ct}

	resp, _ := client.Get(backend.URL)
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
	if total != 500 {
		t.Errorf("total InData = %d; want 500", total)
	}
}

func TestClientTransport_EndOnPrematureClose(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(strings.Repeat("x", 10000)))
	}))
	defer backend.Close()

	rec := &recordingHandler{}
	ct := NewClientTransport(WithClientHandler(rec))
	client := &http.Client{Transport: ct}

	resp, _ := client.Get(backend.URL)
	resp.Body.Close()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	var found bool
	for _, e := range rec.events {
		if e.eventType == OpEnd {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("End event not found after premature close")
	}
}

func TestClientTransport_EndOnceOnConcurrentReadClose(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(strings.Repeat("x", 10000)))
	}))
	defer backend.Close()

	rec := &recordingHandler{}
	ct := NewClientTransport(WithClientHandler(rec))
	client := &http.Client{Transport: ct}

	resp, _ := client.Get(backend.URL)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(io.Discard, resp.Body) }()
	go func() { defer wg.Done(); resp.Body.Close() }()
	wg.Wait()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	count := 0
	for _, e := range rec.events {
		if e.eventType == OpEnd {
			count++
		}
	}
	if count != 1 {
		t.Errorf("End count = %d; want 1", count)
	}
}
