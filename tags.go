package httpstats

import (
	"context"
	"sync"
)

type tagsKey struct{}

// Tags is a thread-safe container for low-cardinality telemetry labels
// discovered mid-flight. It is injected into the request context by the
// middleware and can be enriched by downstream handlers.
//
//	func myHandler(w http.ResponseWriter, r *http.Request) {
//	    if tags := httpstats.FromContext(r.Context()); tags != nil {
//	        tags.Set("tenant_id", extractTenant(r))
//	    }
//	}
//
// Tags uses sync.Map internally. Reads are lock-free once values are
// stored, which fits the typical write-once-read-many telemetry pattern.
type Tags struct {
	m sync.Map
}

// NewTags creates a Tags container.
func NewTags() *Tags {
	return &Tags{}
}

// Set stores a key-value pair. Safe for concurrent use.
func (t *Tags) Set(key, value string) {
	t.m.Store(key, value)
}

// Get retrieves the value for key. The boolean indicates whether the key
// was present. Safe for concurrent use.
func (t *Tags) Get(key string) (string, bool) {
	v, ok := t.m.Load(key)
	if !ok {
		return "", false
	}
	return v.(string), true //nolint:errcheck // sync.Map loads into correct type
}

// Range calls fn for each key-value pair. Iteration stops if fn returns
// false. Safe for concurrent use.
func (t *Tags) Range(fn func(key, value string) bool) {
	t.m.Range(func(key, value any) bool {
		return fn(key.(string), value.(string)) //nolint:errcheck // sync.Map Range values are always strings
	})
}

// InjectTags places a Tags container into the context.
func InjectTags(ctx context.Context, tags *Tags) context.Context {
	return context.WithValue(ctx, tagsKey{}, tags)
}

// FromContext extracts the Tags container injected by InjectTags.
// Returns nil if no Tags are present.
func FromContext(ctx context.Context) *Tags {
	tags, _ := ctx.Value(tagsKey{}).(*Tags)
	return tags
}
