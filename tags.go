package httpstats

import (
	"context"
	"sync"
)

const defaultTagsCapacity = 4

type tagsKey struct{}

// Tags is a mutable, thread-safe container for low-cardinality telemetry
// labels discovered mid-flight. It is injected into the request context by
// TagRequest and can be enriched by downstream business logic.
//
//	func myHandler(w http.ResponseWriter, r *http.Request) {
//	    if tags := httpstats.FromContext(r.Context()); tags != nil {
//	        tags.Set("tenant_id", extractTenant(r))
//	    }
//	    // ...
//	}
type Tags struct {
	mu   sync.RWMutex
	data map[string]string
}

// NewTags creates a Tags container with capacity hint for the expected number
// of entries (helps avoid map reallocation). If hint is 0, a default capacity
// of 4 is used.
func NewTags(hint int) *Tags {
	if hint <= 0 {
		hint = defaultTagsCapacity
	}
	return &Tags{data: make(map[string]string, hint)}
}

// Set stores a key-value pair. It is safe to call from multiple goroutines.
// Overwrites any existing value for key.
func (t *Tags) Set(key, value string) {
	t.mu.Lock()
	t.data[key] = value
	t.mu.Unlock()
}

// Get retrieves the value for key. The boolean indicates whether the key
// was present. It is safe to call from multiple goroutines.
func (t *Tags) Get(key string) (string, bool) {
	t.mu.RLock()
	v, ok := t.data[key]
	t.mu.RUnlock()
	return v, ok
}

// Range calls fn for each key-value pair in the Tags. Iteration stops if fn
// returns false. It is safe to call from multiple goroutines; the callback
// must not call Set on the same Tags (deadlock).
func (t *Tags) Range(fn func(key, value string) bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for k, v := range t.data {
		if !fn(k, v) {
			return
		}
	}
}

// InjectTags places a Tags container into the context. Use this at TagRequest
// time to enable mid-flight enrichment.
func InjectTags(ctx context.Context, tags *Tags) context.Context {
	return context.WithValue(ctx, tagsKey{}, tags)
}

// FromContext extracts the Tags container injected by InjectTags.
// Returns nil if no Tags are present in the context.
func FromContext(ctx context.Context) *Tags {
	tags, _ := ctx.Value(tagsKey{}).(*Tags)
	return tags
}
