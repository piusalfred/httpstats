package httpstats //nolint:testpackage // tests access unexported types

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestTagsSetGet(t *testing.T) {
	tags := NewTags()
	tags.Set("tenant_id", "acme")
	tags.Set("user_role", "admin")

	v, ok := tags.Get("tenant_id")
	if !ok || v != "acme" {
		t.Errorf("tenant_id = %q, ok=%v", v, ok)
	}
	v, ok = tags.Get("user_role")
	if !ok || v != "admin" {
		t.Errorf("user_role = %q, ok=%v", v, ok)
	}
	if _, ok := tags.Get("missing"); ok {
		t.Error("missing key should not be found")
	}
}

func TestTagsSetOverwrite(t *testing.T) {
	tags := NewTags()
	tags.Set("key", "old")
	tags.Set("key", "new")
	if v, _ := tags.Get("key"); v != "new" {
		t.Errorf("key = %q; want 'new'", v)
	}
}

func TestTagsConcurrent(t *testing.T) {
	tags := NewTags()
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tags.Set("key", "value")
			tags.Get("key")
		}()
	}
	wg.Wait()
}

func TestTagsRange(t *testing.T) {
	tags := NewTags()
	tags.Set("a", "1")
	tags.Set("b", "2")
	tags.Set("c", "3")

	found := make(map[string]string)
	tags.Range(func(k, v string) bool { found[k] = v; return true })
	if len(found) != 3 {
		t.Errorf("visited %d entries; want 3", len(found))
	}
}

func TestTagsRangeEarlyExit(t *testing.T) {
	tags := NewTags()
	tags.Set("a", "1")
	tags.Set("b", "2")
	tags.Set("c", "3")

	count := 0
	tags.Range(func(k, v string) bool { count++; return count < 2 })
	if count != 2 {
		t.Errorf("fn called %d times; want 2", count)
	}
}

func TestInjectTagsAndFromContext(t *testing.T) {
	tags := NewTags()
	tags.Set("tenant_id", "acme")
	ctx := InjectTags(context.Background(), tags)

	got := FromContext(ctx)
	if got == nil {
		t.Fatal("FromContext returned nil")
	}
	if v, _ := got.Get("tenant_id"); v != "acme" {
		t.Errorf("tenant_id = %q", v)
	}
	if FromContext(context.Background()) != nil {
		t.Error("FromContext on unrelated context should return nil")
	}
}

func TestServerHandlerTagsInjection(t *testing.T) {
	var capturedTags *Tags
	handler := ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedTags = FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, _ := http.Get(srv.URL)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if capturedTags == nil {
		t.Fatal("Tags not found in request context")
	}
	capturedTags.Set("tenant_id", "acme")
	if v, _ := capturedTags.Get("tenant_id"); v != "acme" {
		t.Errorf("tenant_id = %q", v)
	}
}

func TestServerHandlerWriteErrorInEnd(t *testing.T) {
	rec := &recordingHandler{}
	handler := ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("partial"))
	}), WithServerHandler(rec))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, _ := http.Get(srv.URL)
	resp.Body.Close()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, e := range rec.events {
		if e.eventType == OpEnd {
			return
		}
	}
	t.Fatal("End event not found")
}

type timeoutError struct{}

func (e *timeoutError) Error() string   { return "timeout" }
func (e *timeoutError) Timeout() bool   { return true }
func (e *timeoutError) Temporary() bool { return false }

func TestClassifyEndError(t *testing.T) {
	errBoom := errors.New("boom")           //nolint:err113 // test error variable
	errSomething := errors.New("something") //nolint:err113 // test error variable

	tests := []struct {
		name string
		err  error
		want EndErrorKind
	}{
		{"nil error", nil, EndErrorNone},
		{"context.Canceled", context.Canceled, EndErrorCanceled},
		{"context.DeadlineExceeded", context.DeadlineExceeded, EndErrorDeadline},
		{"wrapped context.Canceled", fmt.Errorf("wrap: %w", context.Canceled), EndErrorCanceled},
		{"wrapped context.DeadlineExceeded", fmt.Errorf("wrap: %w", context.DeadlineExceeded), EndErrorDeadline},
		{"panic error", &panicError{val: errBoom}, EndErrorPanic},
		{"net timeout", &net.OpError{Err: &timeoutError{}}, EndErrorDeadline},
		{"generic error", errSomething, EndErrorOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyEndError(tt.err); got != tt.want {
				t.Errorf("ClassifyEndError(%v) = %v; want %v", tt.err, got, tt.want)
			}
		})
	}
}

var _ net.Error = (*timeoutError)(nil)
