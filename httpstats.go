// Package httpstats provides a backend-agnostic observability interface for
// net/http clients and servers, following the same decoupled design philosophy
// as gRPC-Go's stats.Handler.
//
// Reference implementations for OpenTelemetry and Prometheus live in the
// docs/ directory. Copy them into your project to use as a starting point —
// they are not importable library packages.
//
// # Quick Start — Server
//
// Implement the Handler interface for your telemetry backend, or copy a
// reference implementation from the docs/ directory:
//
//	wrapped := httpstats.ServerHandler(myMux, httpstats.WithServerHandler(myHandler))
//	http.ListenAndServe(":8080", wrapped)
//
// # Quick Start — Client
//
//	ct := httpstats.NewClientTransport(httpstats.WithClientHandler(myHandler))
//	client := &http.Client{Transport: ct}
//	resp, _ := client.Get("https://api.example.com")
//
// # Combining Backends
//
// Use MultiHandler to run OTEL (tracing) and Prometheus (metrics) side by side:
//
//	composed := httpstats.MultiHandler{otelHandler, promHandler}
//	wrapped := httpstats.ServerHandler(myMux, httpstats.WithServerHandler(composed))
//
// # Mid-Flight Telemetry Enrichment
//
// Downstream handlers can attach labels discovered at runtime (tenant ID, user
// role) via the Tags container injected into every request context:
//
//	func myHandler(w http.ResponseWriter, r *http.Request) {
//	    if tags := httpstats.FromContext(r.Context()); tags != nil {
//	        tags.Set("tenant_id", extractTenant(r))
//	    }
//	}
//
// # Event Lifecycle
//
// Server-side event order:
//
//	TagRequest → Begin → InHeader → InData* → OutHeader → OutData* → End
//
// Client-side event order:
//
//	TagRequest → Begin → OutHeader → OutData* → InHeader → InData* → End
//
// Connection events (TagConn → HandleConn) fire independently and may overlap
// with request events.
//
// # InData / OutData Asymmetry
//
// Unlike gRPC where payload flows are symmetric, HTTP is asymmetric. The
// meaning of InData and OutData depends on the Client field:
//
//	Server: InData = request body read,  OutData = response body written
//	Client: OutData = request body written, InData = response body read
//
// Always check the Client field before interpreting payload direction.
//
// # Full-Duplex Concurrency
//
// In HTTP/2 and HTTP/3, the request body can be read concurrently with the
// response body being written — on different goroutines:
//
//   - HandleRequest may be called concurrently for InData/OutData events.
//   - All byte counters in Handler implementations MUST use sync/atomic.
//
// # Client Connection Tracing
//
// The ClientTransport hooks httptrace to emit connection-level timing data.
// When a new connection is dialed (pool miss), ConnBegin carries:
//
//	DNSDuration — time spent resolving the hostname
//	TCPDuration — time spent on the TCP handshake
//	TLSDuration — time spent on the TLS handshake
//
// All durations are zero on reused connections (pool hit). InHeader.TTFB
// captures the round-trip time from the last request byte to the first
// response byte — server queuing plus network transit.
//
// # Context Immutability
//
// context.Context is immutable — TagRequest can inject state, but downstream
// handlers cannot push new labels back. Use the Tags container (FromContext /
// Set) for any label discovered after TagRequest returns.
//
// # Naming Strategy
//
// DefaultOperationName uses http.Request.Pattern (Go 1.22+) when available,
// producing names like "GET /users/{id}". If neither OperationName nor Pattern
// is set, it falls back to "HTTP {method}". Use WithServerNameFunc or
// WithClientNameFunc to override with a custom naming scheme.
//
// # Error Classification
//
// Use ClassifyEndError on the End event's Error field to distinguish client
// disconnects (context.Canceled), deadline exhaustion, write failures, and
// handler panics. Backends use this to set span status codes and metric labels.
package httpstats

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Handler defines the interface for HTTP statistics collection.
//
// Implementations must be safe for concurrent use: HandleRequest may be
// called concurrently for InData/OutData events under HTTP/2 full-duplex.
//
// Context flow:
//
//	TagConn(ctx) → ctx'
//	  └─ TagRequest(ctx') → ctx''
//	       └─ HandleRequest(ctx'', ...)
//	  └─ HandleConn(ctx', ...)
type Handler interface {
	TagConn(ctx context.Context, info *ConnTagInfo) context.Context
	HandleConn(ctx context.Context, stats ConnStats)
	TagRequest(ctx context.Context, info *RequestTagInfo) context.Context
	HandleRequest(ctx context.Context, stats RequestStats)
}

// ConnStats is the sealed interface for connection-level statistics.
type ConnStats interface{ isConnStats() }

// RequestStats is the sealed interface for request-level statistics.
type RequestStats interface{ isRequestStats() }

type ConnTagInfo struct {
	Client     bool
	RemoteAddr net.Addr
	LocalAddr  net.Addr
	TLS        *tls.ConnectionState
}

type RequestTagInfo struct {
	Client         bool
	Method         string
	Host           string
	URL            *url.URL
	OperationName  string // defaults to "HTTP {method}" if empty
	RequestPattern string // http.Request.Pattern (Go 1.22+)
}

// DefaultOperationName returns the operation name. Precedence:
//  1. info.OperationName if explicitly set
//  2. info.RequestPattern if available:
//     a. Used directly if it already contains a space (e.g. "GET /users/{id}")
//     b. Prefixed with method otherwise (e.g. "/users/{id}" → "GET /users/{id}")
//  3. "HTTP {method}" as final fallback
func DefaultOperationName(info *RequestTagInfo) string {
	if info.OperationName != "" {
		return info.OperationName
	}
	if info.RequestPattern != "" {
		if strings.Contains(info.RequestPattern, " ") || info.Method == "" {
			return info.RequestPattern
		}
		return info.Method + " " + info.RequestPattern
	}
	return fmt.Sprintf("HTTP %s", info.Method)
}

// ConnBegin is emitted when a new connection is established. On reused
// connections (pool hit), DNSDuration, TCPDuration, and TLSDuration are
// zero. On cold connections, they capture the cost of establishing the
// link — useful for isolating P99 latency spikes caused by DNS flapping,
// cross-region TCP handshake latency, or overloaded TLS termination.
type ConnBegin struct {
	Client      bool
	StartTime   time.Time
	RemoteAddr  string
	DNSDuration time.Duration // non-zero only on cold connections
	TCPDuration time.Duration // non-zero only on cold connections
	TLSDuration time.Duration // non-zero only on cold connections
}

func (*ConnBegin) isConnStats() {}

type ConnEnd struct {
	Client bool
	Error  error
}

func (*ConnEnd) isConnStats() {}

type Begin struct {
	Client    bool
	StartTime time.Time
}

func (*Begin) isRequestStats() {}

// End carries the final outcome of a request. Error is nil on success.
// Use ClassifyEndError to categorize the error for span status / labels.
type End struct {
	Client  bool
	EndTime time.Time
	Error   error
}

func (*End) isRequestStats() {}

// InHeader fires when response headers arrive (client) or request headers
// are received (server). On the client side, TTFB measures the time from
// the last byte of the request body to the first byte of the response —
// the round-trip transit plus server routing/queuing time. Zero on the
// server side.
type InHeader struct {
	Client        bool
	Header        http.Header
	ContentLength int64
	StatusCode    int
	Method        string
	TTFB          time.Duration // client only: time from request body finish to first response byte
}

func (*InHeader) isRequestStats() {}

// OutHeader fires when response headers are written (server) or request
// headers are sent (client). StatusCode is zero on the client side.
type OutHeader struct {
	Client        bool
	Header        http.Header
	ContentLength int64
	StatusCode    int
}

func (*OutHeader) isRequestStats() {}

// InData fires for each chunk read from the request body (server) or
// response body (client). When Error is non-nil (excluding io.EOF), the
// read ended abnormally.
type InData struct {
	Client bool
	Length int
	Error  error
}

func (*InData) isRequestStats() {}

// OutData fires for each chunk written to the response body (server) or
// request body (client). When Error is non-nil, the write ended abnormally.
type OutData struct {
	Client bool
	Length int
	Error  error
}

func (*OutData) isRequestStats() {}

// EndErrorKind categorizes the error on an End event.
type EndErrorKind int

const (
	EndErrorNone     EndErrorKind = iota
	EndErrorCanceled              // context.Canceled (client disconnect)
	EndErrorDeadline              // context.DeadlineExceeded or net.Error Timeout
	EndErrorWrite                 // write failure (broken pipe, reset)
	EndErrorRead                  // read failure
	EndErrorPanic                 // handler panic
	EndErrorOther                 // unclassified
)

// ClassifyEndError categorizes the error on an End event. A nil error
// returns EndErrorNone. Unwraps error chains via errors.Is and errors.As
// so wrapped context errors are detected.
func ClassifyEndError(err error) EndErrorKind {
	if err == nil {
		return EndErrorNone
	}
	if errors.Is(err, context.Canceled) {
		return EndErrorCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return EndErrorDeadline
	}
	var panicErr *panicError
	if errors.As(err, &panicErr) {
		return EndErrorPanic
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return EndErrorDeadline
	}
	return EndErrorOther
}

type panicError struct{ val any }

func (e *panicError) Error() string { return fmt.Sprintf("httpstats: handler panicked: %v", e.val) }
func (e *panicError) Unwrap() error {
	if err, ok := e.val.(error); ok {
		return err
	}
	return nil
}

// MultiHandler composes multiple Handler instances into one. TagConn and
// TagRequest are called in order, threading the context. HandleConn and
// HandleRequest are called on all handlers. An empty MultiHandler is a no-op.
//
// This lets you combine backends — e.g. OTEL for tracing and Prometheus
// for metrics — without either knowing about the other:
//
//	httpstats.MultiHandler{otelHandler, promHandler}
type MultiHandler []Handler

func (mh MultiHandler) TagConn(ctx context.Context, info *ConnTagInfo) context.Context {
	for _, h := range mh {
		ctx = h.TagConn(ctx, info) //nolint:fatcontext // threading context through handlers
	}
	return ctx
}

func (mh MultiHandler) HandleConn(ctx context.Context, stats ConnStats) {
	for _, h := range mh {
		h.HandleConn(ctx, stats)
	}
}

func (mh MultiHandler) TagRequest(ctx context.Context, info *RequestTagInfo) context.Context {
	for _, h := range mh {
		ctx = h.TagRequest(ctx, info) //nolint:fatcontext // threading context through handlers
	}
	return ctx
}

func (mh MultiHandler) HandleRequest(ctx context.Context, stats RequestStats) {
	for _, h := range mh {
		h.HandleRequest(ctx, stats)
	}
}

var _ Handler = MultiHandler{}

// NoopHandler is a Handler whose methods are all no-ops.
type NoopHandler struct{}

func (NoopHandler) TagConn(ctx context.Context, _ *ConnTagInfo) context.Context       { return ctx }
func (NoopHandler) HandleConn(_ context.Context, _ ConnStats)                         {}
func (NoopHandler) TagRequest(ctx context.Context, _ *RequestTagInfo) context.Context { return ctx }
func (NoopHandler) HandleRequest(_ context.Context, _ RequestStats)                   {}

var _ Handler = NoopHandler{}
