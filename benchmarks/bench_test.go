package benchmarks

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/piusalfred/httpstats"
)

func BenchmarkServer_Simple(b *testing.B) {
	h := &benchHandler{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	wrapped := httpstats.ServerHandler(mux, httpstats.WithServerHandler(h))
	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 16}}

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		resp, err := client.Get(srv.URL)
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func BenchmarkClient_Simple(b *testing.B) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer backend.Close()

	h := &benchHandler{}
	ct := httpstats.NewClientTransport(httpstats.WithClientHandler(h))
	client := &http.Client{Transport: ct}

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		resp, err := client.Get(backend.URL)
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func BenchmarkServer_Body(b *testing.B) {
	h := &benchHandler{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	wrapped := httpstats.ServerHandler(mux, httpstats.WithServerHandler(h))
	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 16}}
	payload := make([]byte, 1024)
	for i := range payload {
		payload[i] = 'x'
	}

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		resp, err := client.Post(srv.URL, "application/octet-stream", &byteReader{payload})
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func BenchmarkServer_Concurrent(b *testing.B) {
	h := &benchHandler{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	wrapped := httpstats.ServerHandler(mux, httpstats.WithServerHandler(h))
	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	client := &http.Client{
		Transport: &http.Transport{MaxIdleConnsPerHost: 64},
	}

	b.ReportAllocs()
	b.SetParallelism(4)
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := client.Get(srv.URL)
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

type byteReader struct{ data []byte }

func (r *byteReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}
func (r *byteReader) Close() error { return nil }

type benchHandler struct {
	httpstats.NoopHandler
}

func (h *benchHandler) TagRequest(ctx context.Context, info *httpstats.RequestTagInfo) context.Context {
	return context.WithValue(ctx, benchKey{}, &benchState{start: time.Now()})
}

func (h *benchHandler) HandleRequest(ctx context.Context, ev *httpstats.Event) {
	s := ctx.Value(benchKey{}).(*benchState)
	switch ev.Type {
	case httpstats.OpInData:
		s.read.Add(int64(ev.Length))
	case httpstats.OpOutData:
		s.wrote.Add(int64(ev.Length))
	case httpstats.OpEnd:
		_ = s.read.Load()
		_ = s.wrote.Load()
		_ = ev.EndTime.Sub(s.start)
	}
}

type benchKey struct{}

type benchState struct {
	start time.Time
	read  atomic.Int64
	wrote atomic.Int64
}
