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

	resp, err := client.Get(backend.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	want := []string{
		"TagRequest",
		"Begin",
		"OutHeader",
		"InHeader",
		"InData",
		"End",
	}
	// Filter out connection events (TagConn, HandleConn) which may fire
	// at any point during the request lifecycle.
	allTypes := rec.eventTypes()
	got := make([]string, 0, len(allTypes))
	for _, t := range allTypes {
		switch t {
		case "TagConn", "HandleConn", "ConnBegin", "ConnEnd":
			continue
		}
		got = append(got, t)
	}
	if len(got) < len(want) {
		t.Fatalf("got %d events, want at least %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("event[%d] = %q; want %q", i, got[i], w)
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

	resp, err := client.Get(backend.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, e := range rec.events {
		if e.eventType == "HandleRequest" && !e.client {
			t.Errorf("%s event has Client=false; want true", e.statsType)
		}
	}
}

func TestClientTransportStatusCodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
	}{
		{"200 OK", http.StatusOK},
		{"404 Not Found", http.StatusNotFound},
		{"500 Internal Server Error", http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
			}))
			defer backend.Close()

			rec := &recordingHandler{}
			ct := NewClientTransport(WithClientHandler(rec))
			client := &http.Client{Transport: ct}

			resp, err := client.Get(backend.URL)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)

			rec.mu.Lock()
			defer rec.mu.Unlock()
			var inHdr *recordedEvent
			for i := range rec.events {
				if rec.events[i].statsType == "InHeader" {
					inHdr = &rec.events[i]
					break
				}
			}
			if inHdr == nil {
				t.Fatal("InHeader event not found")
			}
			if inHdr.statusCode != tt.statusCode {
				t.Errorf("InHeader.statusCode = %d; want %d", inHdr.statusCode, tt.statusCode)
			}
		})
	}
}

func TestClientTransport_ConnectionError(t *testing.T) {
	rec := &recordingHandler{}
	ct := NewClientTransport(WithClientHandler(rec))
	client := &http.Client{Transport: ct}

	_, err := client.Get("http://127.0.0.1:19999/nope")
	if err == nil {
		t.Skip("expected connection error")
	}

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
		t.Fatal("End event not found on connection error")
	}
	if endEvent.endError == nil {
		t.Error("End.Error is nil on connection error; want non-nil")
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

	body := strings.Repeat("data", 250) // 1000 bytes
	resp, err := client.Post(backend.URL, "text/plain", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	var totalOutData int
	for _, e := range rec.events {
		if e.statsType == "OutData" && e.dataLength > 0 {
			totalOutData += e.dataLength
		}
	}
	if totalOutData != len(body) {
		t.Errorf("total OutData length = %d; want %d", totalOutData, len(body))
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

	resp, err := client.Get(backend.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
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
	if totalInData != 500 {
		t.Errorf("total InData length = %d; want 500", totalInData)
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

	resp, err := client.Get(backend.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close() // close without reading all

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

	resp, err := client.Get(backend.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(io.Discard, resp.Body)
	}()
	go func() {
		defer wg.Done()
		resp.Body.Close()
	}()
	wg.Wait()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	endCount := 0
	for _, e := range rec.events {
		if e.statsType == "End" {
			endCount++
		}
	}
	if endCount != 1 {
		t.Errorf("End event count = %d; want exactly 1", endCount)
	}
}

func TestClientTransportTagRequestMethod(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	rec := &recordingHandler{}
	ct := NewClientTransport(WithClientHandler(rec))
	client := &http.Client{Transport: ct}

	req, _ := http.NewRequest(http.MethodPut, backend.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	var tagReq *recordedEvent
	for i := range rec.events {
		if rec.events[i].eventType == "TagRequest" {
			tagReq = &rec.events[i]
			break
		}
	}
	if tagReq == nil {
		t.Fatal("TagRequest event not found")
	}
	if tagReq.method != "PUT" {
		t.Errorf("TagRequest.method = %q; want %q", tagReq.method, "PUT")
	}
}
