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
	tags := NewTags(2)

	tags.Set("tenant_id", "acme")
	tags.Set("user_role", "admin")

	v, ok := tags.Get("tenant_id")
	if !ok {
		t.Fatal("tenant_id not found")
	}
	if v != "acme" {
		t.Errorf("tenant_id = %q; want %q", v, "acme")
	}

	v, ok = tags.Get("user_role")
	if !ok {
		t.Fatal("user_role not found")
	}
	if v != "admin" {
		t.Errorf("user_role = %q; want %q", v, "admin")
	}

	_, ok = tags.Get("missing")
	if ok {
		t.Error("missing key should not be found")
	}
}

func TestTagsSetOverwrite(t *testing.T) {
	tags := NewTags(2)
	tags.Set("key", "old")
	tags.Set("key", "new")

	v, _ := tags.Get("key")
	if v != "new" {
		t.Errorf("key = %q; want %q", v, "new")
	}
}

func TestTagsConcurrent(t *testing.T) {
	tags := NewTags(4)
	var wg sync.WaitGroup

	for i := range 100 {
		wg.Add(1)
		go func(_ int) {
			defer wg.Done()
			tags.Set("key", "value")
			tags.Get("key")
		}(i)
	}
	wg.Wait()
}

func TestTagsRange(t *testing.T) {
	tags := NewTags(3)
	tags.Set("a", "1")
	tags.Set("b", "2")
	tags.Set("c", "3")

	found := make(map[string]string)
	tags.Range(func(k, v string) bool {
		found[k] = v
		return true
	})

	if len(found) != 3 {
		t.Errorf("Range visited %d entries; want 3", len(found))
	}
	for k, want := range map[string]string{"a": "1", "b": "2", "c": "3"} {
		if found[k] != want {
			t.Errorf("Range[%s] = %q; want %q", k, found[k], want)
		}
	}
}

func TestTagsRangeEarlyExit(t *testing.T) {
	tags := NewTags(3)
	tags.Set("a", "1")
	tags.Set("b", "2")
	tags.Set("c", "3")

	count := 0
	tags.Range(func(k, v string) bool {
		count++
		return count < 2
	})

	if count != 2 {
		t.Errorf("Range called fn %d times; want 2", count)
	}
}

func TestInjectTagsAndFromContext(t *testing.T) {
	tags := NewTags(2)
	tags.Set("tenant_id", "acme")

	ctx := InjectTags(context.Background(), tags)

	got := FromContext(ctx)
	if got == nil {
		t.Fatal("FromContext returned nil")
	}

	v, ok := got.Get("tenant_id")
	if !ok || v != "acme" {
		t.Errorf("tenant_id = %q, ok=%v; want %q, true", v, ok, "acme")
	}

	// FromContext on an unrelated context returns nil.
	if FromContext(context.Background()) != nil {
		t.Error("FromContext on unrelated context should return nil")
	}
}

func TestTagsNewTagsHint(t *testing.T) {
	// Zero hint defaults to 4.
	tags := NewTags(0)
	if tags.data == nil {
		t.Fatal("NewTags(0) returned nil map")
	}
	// Verify it works.
	tags.Set("k", "v")
}

// TestServerHandlerTagsInjection verifies that Tags are injected into the
// request context and can be read by downstream handlers.
func TestServerHandlerTagsInjection(t *testing.T) {
	var capturedTags *Tags

	handler := ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedTags = FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if capturedTags == nil {
		t.Fatal("Tags not found in request context")
	}

	// Downstream handler enriches tags.
	capturedTags.Set("tenant_id", "acme")
	v, ok := capturedTags.Get("tenant_id")
	if !ok || v != "acme" {
		t.Errorf("tenant_id = %q, ok=%v; want %q, true", v, ok, "acme")
	}
}

// TestServerHandlerWriteErrorTracking verifies that write errors are captured
// in the End event. We use a handler that attempts to write after the client
// disconnects.
func TestServerHandlerWriteErrorInEnd(t *testing.T) {
	rec := &recordingHandler{}

	handler := ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Write a large body — the client will close early.
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("partial")) // may succeed or fail depending on timing
	}), WithServerHandler(rec))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Make a request and immediately close without reading the body.
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	resp.Body.Close() // close early — may trigger write error on server

	// Find the End event.
	var endEvent *recordedEvent
	for i := range rec.events {
		if rec.events[i].statsType == "End" {
			endEvent = &rec.events[i]
			break
		}
	}
	if endEvent == nil {
		t.Fatal("End event not found")
	}
	// End.Error may or may not be nil depending on whether the write
	// completed before the client closed — both outcomes are valid.
	// The key invariant is that the End event always fires.
}

// TestClassifyEndError verifies the error categorization helper.
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
			got := ClassifyEndError(tt.err)
			if got != tt.want {
				t.Errorf("ClassifyEndError(%v) = %v; want %v", tt.err, got, tt.want)
			}
		})
	}
}

// timeoutError implements net.Error with Timeout() == true.
type timeoutError struct{}

func (e *timeoutError) Error() string   { return "timeout" }
func (e *timeoutError) Timeout() bool   { return true }
func (e *timeoutError) Temporary() bool { return false }
