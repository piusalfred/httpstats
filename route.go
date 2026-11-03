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
	"path"
	"strings"
	"sync"
)

// Route describes where a server request landed. Both fields are templates
// and therefore low-cardinality: they never contain a concrete URL path such
// as "/users/1234".
type Route struct {
	// Group is the prefix of the router group that took the request, such as
	// "/users" or "/auth". It is set even when no route inside the group
	// matched (a 404 or 405), so unmatched traffic is still attributable.
	Group string

	// Pattern is the full templated route, group included, for example
	// "/users/{id}". Empty when nothing matched.
	Pattern string
}

type requestStateKey struct{}

// requestState is the request-scoped record routers and handlers write into
// while the request is in flight. ServerHandler injects it before TagRequest
// and snapshots it into the End event, so recorders see the final values even
// though the context is immutable and routers such as http.StripPrefix hand
// downstream handlers a cloned *http.Request.
type requestState struct {
	mu      sync.Mutex
	group   string
	pattern string
	err     error
}

func stateFrom(ctx context.Context) *requestState {
	st, _ := ctx.Value(requestStateKey{}).(*requestState)
	return st
}

func withRequestState(ctx context.Context) (context.Context, *requestState) {
	st := &requestState{}
	return context.WithValue(ctx, requestStateKey{}, st), st
}

func (st *requestState) route() Route {
	st.mu.Lock()
	defer st.mu.Unlock()

	return Route{Group: st.group, Pattern: st.pattern}
}

func (st *requestState) handlerErr() error {
	st.mu.Lock()
	defer st.mu.Unlock()

	return st.err
}

// AddGroup records that the request entered a router group mounted at prefix.
// Nested groups accumulate: AddGroup("/api") then AddGroup("/v1") yields
// "/api/v1". It is a no-op when ctx carries no ServerHandler state, so
// routers can call it unconditionally.
func AddGroup(ctx context.Context, prefix string) {
	st := stateFrom(ctx)
	if st == nil {
		return
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	st.group = joinPath(st.group, prefix)
}

// SetRoute records the matched route template, relative to the groups entered
// so far: after AddGroup("/users"), SetRoute("/{id}") gives "/users/{id}". The
// last call wins. No-op without ServerHandler state.
func SetRoute(ctx context.Context, pattern string) {
	st := stateFrom(ctx)
	if st == nil {
		return
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	st.pattern = joinPath(st.group, pattern)
}

// SetError records an application-level error returned by a handler that
// reports failures by value instead of writing them, so the recorder can
// attach it to the span and log line. The first error wins. No-op without
// ServerHandler state or for a nil error.
func SetError(ctx context.Context, err error) {
	st := stateFrom(ctx)
	if st == nil || err == nil {
		return
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	if st.err == nil {
		st.err = err
	}
}

// joinPath concatenates a group prefix and a route template. Unlike
// path.Join it keeps a trailing slash on the template ("/files/" stays a
// subtree pattern) and never cleans away "{...}" wildcards.
func joinPath(base, p string) string {
	if p == "" {
		return base
	}
	if base == "" || base == "/" {
		if !strings.HasPrefix(p, "/") {
			return "/" + p
		}

		return p
	}

	if p == "/" {
		return base
	}

	joined := path.Join(base, p)
	if strings.HasSuffix(p, "/") && !strings.HasSuffix(joined, "/") {
		joined += "/"
	}

	return joined
}

// patternPath strips the optional method from an http.Request.Pattern
// ("GET /users/{id}" -> "/users/{id}"). A host-qualified pattern keeps its host.
func patternPath(pattern string) string {
	if _, after, ok := strings.Cut(pattern, " "); ok {
		return strings.TrimLeft(after, " ")
	}

	return pattern
}
