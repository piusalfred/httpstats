// Package httpstats provides a backend-agnostic observability interface for
// net/http clients and servers, following the same decoupled design philosophy
// as gRPC-Go's stats.Handler.
//
// This branch implements the flat-variant Event design with sync.Pool reuse
// for benchmarking against the sealed-interface approach on main.
//
// Reference implementations for OpenTelemetry and Prometheus live in the
// docs/ directory.
//
// # Quick Start — Server
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
//	composed := httpstats.MultiHandler{otelHandler, promHandler}
//	wrapped := httpstats.ServerHandler(myMux, httpstats.WithServerHandler(composed))
//
// # Mid-Flight Telemetry Enrichment
//
//	func myHandler(w http.ResponseWriter, r *http.Request) {
//	    if tags := httpstats.FromContext(r.Context()); tags != nil {
//	        tags.Set("tenant_id", extractTenant(r))
//	    }
//	}
//
// # Event Lifecycle
//
//	Server: TagRequest → OpBegin → OpInHeader → OpInData* → OpOutHeader → OpOutData* → OpEnd
//	Client: TagRequest → OpBegin → OpOutHeader → OpOutData* → OpInHeader → OpInData* → OpEnd
//
// Connection events (TagConn → HandleConn) fire independently.
//
// # InData / OutData Asymmetry
//
//	Server: OpInData = request body read,  OpOutData = response body written
//	Client: OpOutData = request body written, OpInData = response body read
//
// Always check ev.Client before interpreting payload direction.
//
// # Full-Duplex Concurrency
//
// HandleRequest for OpInData/OpOutData may fire from different goroutines
// than OpEnd. All byte counters in Handler implementations MUST use
// sync/atomic. The Event struct is pooled — do not retain pointers to it
// after HandleRequest returns.
//
// # Context Immutability
//
// Use the Tags container (FromContext / Set) for labels discovered after
// TagRequest returns.
//
// # Naming Strategy
//
// DefaultOperationName uses http.Request.Pattern (Go 1.22+) when available,
// producing names like "GET /users/{id}". Override with WithServerNameFunc
// or WithClientNameFunc.
//
// # Error Classification
//
// Use ClassifyEndError on OpEnd's Error field to distinguish client
// disconnects, deadlines, write failures, and panics.
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
	"sync"
	"time"
)

// Handler defines the interface for HTTP statistics collection.
type Handler interface {
	TagConn(ctx context.Context, info *ConnTagInfo) context.Context
	HandleConn(ctx context.Context, ev *ConnEvent)
	TagRequest(ctx context.Context, info *RequestTagInfo) context.Context
	HandleRequest(ctx context.Context, ev *Event)
}

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
	OperationName  string
	RequestPattern string
}

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

// ---- Event types ----

// EventType identifies the phase of the request lifecycle.
type EventType int

const (
	OpBegin EventType = iota
	OpEnd
	OpInHeader
	OpOutHeader
	OpInData
	OpOutData
)

// Event is a flat variant struct for all request-level lifecycle events.
// Only fields relevant to the current Type are populated; others are zero.
// Events are pooled via NewEvent/PutEvent — do not retain pointers.
type Event struct {
	Type          EventType
	Client        bool
	StartTime     time.Time
	EndTime       time.Time
	StatusCode    int
	Length        int
	Error         error
	Method        string
	Header        http.Header
	ContentLength int64
}

//nolint:gochecknoglobals // pooled for zero-alloc event lifecycle
var eventPool = sync.Pool{
	New: func() any { return new(Event) },
}

// NewEvent returns a pooled Event. Call PutEvent after the handler processes it.
func NewEvent() *Event {
	ev, _ := eventPool.Get().(*Event)
	return ev
}

// PutEvent returns an Event to the pool after resetting pointer fields.
func PutEvent(ev *Event) {
	ev.Type = 0
	ev.Client = false
	ev.StartTime = time.Time{}
	ev.EndTime = time.Time{}
	ev.StatusCode = 0
	ev.Length = 0
	ev.Error = nil
	ev.Method = ""
	ev.Header = nil
	ev.ContentLength = 0
	eventPool.Put(ev)
}

// ConnEventType identifies connection-level lifecycle phases.
type ConnEventType int

const (
	ConnOpBegin ConnEventType = iota
	ConnOpEnd
)

// ConnEvent is a flat variant struct for connection lifecycle events.
type ConnEvent struct {
	Type       ConnEventType
	Client     bool
	StartTime  time.Time
	RemoteAddr string
	Error      error
}

// ---- Error classification ----

type EndErrorKind int

const (
	EndErrorNone EndErrorKind = iota
	EndErrorCanceled
	EndErrorDeadline
	EndErrorWrite
	EndErrorRead
	EndErrorPanic
	EndErrorOther
)

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

// ---- Handler composition ----

type MultiHandler []Handler

func (mh MultiHandler) TagConn(ctx context.Context, info *ConnTagInfo) context.Context {
	for _, h := range mh {
		ctx = h.TagConn(ctx, info) //nolint:fatcontext // threading context through handlers
	}
	return ctx
}

func (mh MultiHandler) HandleConn(ctx context.Context, ev *ConnEvent) {
	for _, h := range mh {
		h.HandleConn(ctx, ev)
	}
}

func (mh MultiHandler) TagRequest(ctx context.Context, info *RequestTagInfo) context.Context {
	for _, h := range mh {
		ctx = h.TagRequest(ctx, info) //nolint:fatcontext // threading context through handlers
	}
	return ctx
}

func (mh MultiHandler) HandleRequest(ctx context.Context, ev *Event) {
	for _, h := range mh {
		h.HandleRequest(ctx, ev)
	}
}

var _ Handler = MultiHandler{}

type NoopHandler struct{}

func (NoopHandler) TagConn(ctx context.Context, _ *ConnTagInfo) context.Context       { return ctx }
func (NoopHandler) HandleConn(_ context.Context, _ *ConnEvent)                        {}
func (NoopHandler) TagRequest(ctx context.Context, _ *RequestTagInfo) context.Context { return ctx }
func (NoopHandler) HandleRequest(_ context.Context, _ *Event)                         {}

var _ Handler = NoopHandler{}
