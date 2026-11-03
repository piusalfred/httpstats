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

package httpstats

import (
	"context"

	"github.com/go4org/hashtriemap"
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
type Tags struct {
	m hashtriemap.HashTrieMap[string, string]
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
	return t.m.Load(key)
}

// Range calls fn for each key-value pair. Iteration stops if fn returns
// false. Safe for concurrent use.
func (t *Tags) Range(fn func(key, value string) bool) {
	t.m.Range(func(key, value string) bool {
		return fn(key, value)
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
