## Writing a Handler Implementation

The [`httpstats.Handler`](../httpstats.go) interface has four methods. The
core middleware calls them at specific points in every request lifecycle.
Your job is to translate those calls into telemetry for your backend.

This guide walks through two complete examples — OpenTelemetry and
Prometheus — explaining the decisions at each step. Use them as a reference
when writing your own.

### How the interface works

Every request goes through a fixed sequence. `TagRequest` fires first — use
it to create a span or capture start time. Store whatever you need in the
context so `HandleRequest` can pick it up later.

```text
TagRequest → Begin → InHeader → InData* → OutHeader → OutData* → End
```

`HandleRequest` receives concrete event types. Switch on them:

| Event | When it fires | What to do |
|---|---|---|
| `Begin` | Request starts | Capture start time (already done in TagRequest, but Begin gives a second chance) |
| `InHeader` | Headers received | Record status code on the span or in state |
| `OutHeader` | Headers sent | Same — which side sets status depends on `Client` field |
| `InData` | Body chunk read | Accumulate bytes read into an atomic counter |
| `OutData` | Body chunk written | Accumulate bytes written into an atomic counter |
| `End` | Request done | End the span, record metrics from accumulated counters |

`HandleRequest` may be called from multiple goroutines simultaneously under
HTTP/2 full-duplex (a server reading the request body while writing the
response). Any shared state — like byte counters — must use `sync/atomic`.

### Per-request state

Both examples follow the same pattern: create a struct in `TagRequest`,
store it with `context.WithValue`, retrieve it in `HandleRequest`.

```go
type requestState struct {
    startTime  time.Time
    bytesRead  atomic.Int64
    bytesWrote atomic.Int64
    statusCode int
    method     string
}

type requestStateKey struct{}

func (h *Handler) TagRequest(ctx context.Context, info *httpstats.RequestTagInfo) context.Context {
    return context.WithValue(ctx, requestStateKey{}, &requestState{
        startTime: time.Now(),
        method:    info.Method,
    })
}

func requestStateFromContext(ctx context.Context) *requestState {
    s, _ := ctx.Value(requestStateKey{}).(*requestState)
    return s
}
```

The key type is an unexported empty struct — this guarantees no other
package can collide with it. If `requestStateFromContext` returns nil, it
means `TagRequest` wasn't called for this context; return early.

### OpenTelemetry

The OTEL handler creates a span in `TagRequest` and records metrics in
`HandleRequest`. Span naming uses `httpstats.DefaultOperationName`, which
picks up `http.Request.Pattern` (Go 1.22+) when available — this gives you
low-cardinality names like `GET /users/{id}` instead of `HTTP GET`.

The handler accepts a `TracerProvider` and `MeterProvider` through options,
defaulting to no-op implementations. This means the handler is safe to
create even before you've wired up an exporter — it just won't produce
output until providers are set.

```go
package otel

import (
    "context"
    "sync/atomic"
    "time"

    "go.opentelemetry.io/otel/attribute"
    "go.opentelemetry.io/otel/metric"
    "go.opentelemetry.io/otel/metric/noop"
    "go.opentelemetry.io/otel/trace"
    nooptrace "go.opentelemetry.io/otel/trace/noop"

    "github.com/piusalfred/httpstats"
)

// config holds providers injected through options.
type config struct {
    TracerProvider trace.TracerProvider
    MeterProvider  metric.MeterProvider
}

func newConfig(opts ...Option) *config {
    cfg := &config{
        TracerProvider: nooptrace.NewTracerProvider(),
        MeterProvider:  noop.NewMeterProvider(),
    }
    for _, o := range opts {
        o.apply(cfg)
    }
    return cfg
}

type Option interface{ apply(*config) }

type optionFunc func(*config)
func (f optionFunc) apply(c *config) { f(c) }

func WithTracerProvider(tp trace.TracerProvider) Option {
    return optionFunc(func(c *config) { c.TracerProvider = tp })
}
func WithMeterProvider(mp metric.MeterProvider) Option {
    return optionFunc(func(c *config) { c.MeterProvider = mp })
}

const version = "0.1.0"

type Handler struct {
    tracer  trace.Tracer
    metrics *metrics
}

type metrics struct {
    serverDuration metric.Float64Histogram
    clientDuration metric.Float64Histogram
    serverReqSize  metric.Int64Histogram
    serverRespSize metric.Int64Histogram
    clientReqSize  metric.Int64Histogram
    clientRespSize metric.Int64Histogram
}

func NewServerHandler(opts ...Option) (*Handler, error) {
    return newHandler(newConfig(opts...))
}

func NewClientHandler(opts ...Option) (*Handler, error) {
    return newHandler(newConfig(opts...))
}

func newHandler(cfg *config) (*Handler, error) {
    tracer := cfg.TracerProvider.Tracer(
        "github.com/piusalfred/httpstats/otel",
        trace.WithInstrumentationVersion(version),
    )
    meter := cfg.MeterProvider.Meter(
        "github.com/piusalfred/httpstats/otel",
        metric.WithInstrumentationVersion(version),
    )

    h := &Handler{tracer: tracer, metrics: &metrics{}}

    registerDuration := func(field *metric.Float64Histogram, name, desc string) error {
        v, err := meter.Float64Histogram(name, metric.WithDescription(desc), metric.WithUnit("s"))
        *field = v
        return err
    }
    registerSize := func(field *metric.Int64Histogram, name, desc string) error {
        v, err := meter.Int64Histogram(name, metric.WithDescription(desc), metric.WithUnit("By"))
        *field = v
        return err
    }

    if err := registerDuration(&h.metrics.serverDuration,
        "http.server.request.duration", "Duration of HTTP server requests"); err != nil {
        return nil, err
    }
    if err := registerDuration(&h.metrics.clientDuration,
        "http.client.request.duration", "Duration of HTTP client requests"); err != nil {
        return nil, err
    }
    if err := registerSize(&h.metrics.serverReqSize,
        "http.server.request.body.size", "Size of HTTP server request bodies"); err != nil {
        return nil, err
    }
    if err := registerSize(&h.metrics.serverRespSize,
        "http.server.response.body.size", "Size of HTTP server response bodies"); err != nil {
        return nil, err
    }
    if err := registerSize(&h.metrics.clientReqSize,
        "http.client.request.body.size", "Size of HTTP client request bodies"); err != nil {
        return nil, err
    }
    if err := registerSize(&h.metrics.clientRespSize,
        "http.client.response.body.size", "Size of HTTP client response bodies"); err != nil {
        return nil, err
    }

    return h, nil
}

func (h *Handler) TagConn(ctx context.Context, info *httpstats.ConnTagInfo) context.Context { return ctx }
func (h *Handler) HandleConn(ctx context.Context, stats httpstats.ConnStats)               {}

func (h *Handler) TagRequest(ctx context.Context, info *httpstats.RequestTagInfo) context.Context {
    spanName := httpstats.DefaultOperationName(info)

    spanKind := trace.SpanKindServer
    if info.Client {
        spanKind = trace.SpanKindClient
    }

    attrs := []attribute.KeyValue{
        attribute.String("http.request.method", info.Method),
    }
    if info.Host != "" {
        attrs = append(attrs, attribute.String("server.address", info.Host))
    }

    ctx, span := h.tracer.Start(ctx, spanName, trace.WithSpanKind(spanKind), trace.WithAttributes(attrs...))

    return context.WithValue(ctx, requestStateKey{}, &requestState{
        span:      span,
        startTime: time.Now(),
        method:    info.Method,
    })
}

func (h *Handler) HandleRequest(ctx context.Context, stats httpstats.RequestStats) {
    state := requestStateFromContext(ctx)
    if state == nil {
        return
    }

    switch s := stats.(type) {
    case *httpstats.Begin:
        if !s.StartTime.IsZero() {
            state.startTime = s.StartTime
        }

    case *httpstats.InHeader:
        if s.Client && s.StatusCode > 0 {
            state.statusCode = s.StatusCode
            state.span.SetAttributes(attribute.Int("http.response.status_code", s.StatusCode))
        }

    case *httpstats.OutHeader:
        if !s.Client && s.StatusCode > 0 {
            state.statusCode = s.StatusCode
            state.span.SetAttributes(attribute.Int("http.response.status_code", s.StatusCode))
        }

    case *httpstats.InData:
        if s.Length > 0 {
            state.bytesRead.Add(int64(s.Length))
        }

    case *httpstats.OutData:
        if s.Length > 0 {
            state.bytesWrote.Add(int64(s.Length))
        }

    case *httpstats.End:
        if s.Error != nil {
            state.span.RecordError(s.Error)
        }

        // Merge mid-flight tags into span attributes.
        if tags := httpstats.FromContext(ctx); tags != nil {
            tags.Range(func(k, v string) bool {
                state.span.SetAttributes(attribute.String("httpstats."+k, v))
                return true
            })
        }

        state.span.End()

        bytesRead := state.bytesRead.Load()
        bytesWrote := state.bytesWrote.Load()
        duration := s.EndTime.Sub(state.startTime)
        metricAttrs := metric.WithAttributes(
            attribute.String("http.request.method", state.method),
            attribute.Int("http.response.status_code", state.statusCode),
        )

        if s.Client && h.metrics.clientDuration != nil {
            h.metrics.clientDuration.Record(ctx, duration.Seconds(), metricAttrs)
        }
        if !s.Client && h.metrics.serverDuration != nil {
            h.metrics.serverDuration.Record(ctx, duration.Seconds(), metricAttrs)
        }

        if s.Client && h.metrics.clientReqSize != nil && bytesWrote > 0 {
            h.metrics.clientReqSize.Record(ctx, bytesWrote, metricAttrs)
        }
        if s.Client && h.metrics.clientRespSize != nil && bytesRead > 0 {
            h.metrics.clientRespSize.Record(ctx, bytesRead, metricAttrs)
        }
        if !s.Client && h.metrics.serverReqSize != nil && bytesRead > 0 {
            h.metrics.serverReqSize.Record(ctx, bytesRead, metricAttrs)
        }
        if !s.Client && h.metrics.serverRespSize != nil && bytesWrote > 0 {
            h.metrics.serverRespSize.Record(ctx, bytesWrote, metricAttrs)
        }
    }
}

type requestState struct {
    span       trace.Span
    startTime  time.Time
    bytesRead  atomic.Int64
    bytesWrote atomic.Int64
    statusCode int
    method     string
}

type requestStateKey struct{}

func requestStateFromContext(ctx context.Context) *requestState {
    s, _ := ctx.Value(requestStateKey{}).(*requestState)
    return s
}
```

Key points in the OTEL handler:

- **Span creation happens in `TagRequest`**, before the request is processed. This gives the most accurate duration when the span ends at `End` time.
- **Status code arrives through headers**, not through `End`. The middleware fires `OutHeader` when `WriteHeader` is called, which is why the handler sets the status code attribute there.
- **Byte counters use `atomic.Int64`**. Under HTTP/2 full-duplex, body reads and writes happen concurrently.
- **Metric names follow OTEL semantic conventions** (`http.server.request.duration`, `http.client.request.body.size`, etc.) so they compose with other instrumented libraries.
- **Client vs. server metrics are separate instruments**. The `Client` field on each event determines which histogram gets the observation.

### Prometheus

The Prometheus handler records RED metrics (rate, errors, duration) plus body
size histograms and an active-connections gauge. It implements
`prometheus.Collector` so it can be registered directly with a registry.

The main difference from OTEL: there is no span. State in `TagRequest` only
captures start time and method. Everything else is accumulated through
`HandleRequest` events and emitted at `End`.

```go
package prometheus

import (
    "context"
    "strconv"
    "sync/atomic"
    "time"

    "github.com/prometheus/client_golang/prometheus"

    "github.com/piusalfred/httpstats"
)

type config struct {
    DurationBuckets []float64
    SizeBuckets     []float64
}

func newConfig(opts ...Option) *config {
    cfg := &config{
        DurationBuckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
        SizeBuckets:     []float64{100, 1000, 10000, 100000, 1000000, 10000000},
    }
    for _, o := range opts {
        o.apply(cfg)
    }
    return cfg
}

type Option interface{ apply(*config) }

type optionFunc func(*config)
func (f optionFunc) apply(c *config) { f(c) }

func WithDurationBuckets(buckets []float64) Option {
    return optionFunc(func(c *config) { c.DurationBuckets = buckets })
}
func WithSizeBuckets(buckets []float64) Option {
    return optionFunc(func(c *config) { c.SizeBuckets = buckets })
}

type Handler struct {
    requestsTotal     *prometheus.CounterVec
    requestDuration   *prometheus.HistogramVec
    requestBodyBytes  *prometheus.HistogramVec
    responseBodyBytes *prometheus.HistogramVec
    connectionsActive prometheus.Gauge
}

func NewHandler(opts ...Option) *Handler {
    cfg := newConfig(opts...)
    return &Handler{
        requestsTotal: prometheus.NewCounterVec(
            prometheus.CounterOpts{
                Name: "http_requests_total",
                Help: "Total number of HTTP requests handled.",
            },
            []string{"method", "status_code"},
        ),
        requestDuration: prometheus.NewHistogramVec(
            prometheus.HistogramOpts{
                Name:    "http_request_duration_seconds",
                Help:    "Duration of HTTP requests in seconds.",
                Buckets: cfg.DurationBuckets,
            },
            []string{"method", "status_code"},
        ),
        requestBodyBytes: prometheus.NewHistogramVec(
            prometheus.HistogramOpts{
                Name:    "http_request_body_bytes",
                Help:    "Size of HTTP request bodies in bytes.",
                Buckets: cfg.SizeBuckets,
            },
            []string{"method"},
        ),
        responseBodyBytes: prometheus.NewHistogramVec(
            prometheus.HistogramOpts{
                Name:    "http_response_body_bytes",
                Help:    "Size of HTTP response bodies in bytes.",
                Buckets: cfg.SizeBuckets,
            },
            []string{"method", "status_code"},
        ),
        connectionsActive: prometheus.NewGauge(
            prometheus.GaugeOpts{
                Name: "http_connections_active",
                Help: "Current number of active HTTP connections.",
            },
        ),
    }
}

func (h *Handler) Describe(ch chan<- *prometheus.Desc) {
    h.requestsTotal.Describe(ch)
    h.requestDuration.Describe(ch)
    h.requestBodyBytes.Describe(ch)
    h.responseBodyBytes.Describe(ch)
    h.connectionsActive.Describe(ch)
}

func (h *Handler) Collect(ch chan<- prometheus.Metric) {
    h.requestsTotal.Collect(ch)
    h.requestDuration.Collect(ch)
    h.requestBodyBytes.Collect(ch)
    h.responseBodyBytes.Collect(ch)
    h.connectionsActive.Collect(ch)
}

func (h *Handler) TagConn(ctx context.Context, info *httpstats.ConnTagInfo) context.Context { return ctx }

func (h *Handler) HandleConn(_ context.Context, stats httpstats.ConnStats) {
    switch stats.(type) {
    case *httpstats.ConnBegin:
        h.connectionsActive.Inc()
    case *httpstats.ConnEnd:
        h.connectionsActive.Dec()
    }
}

func (h *Handler) TagRequest(ctx context.Context, info *httpstats.RequestTagInfo) context.Context {
    return context.WithValue(ctx, requestStateKey{}, &requestState{
        startTime: time.Now(),
        method:    info.Method,
    })
}

func (h *Handler) HandleRequest(ctx context.Context, stats httpstats.RequestStats) {
    state := requestStateFromContext(ctx)
    if state == nil {
        return
    }

    switch s := stats.(type) {
    case *httpstats.Begin:
        if !s.StartTime.IsZero() {
            state.startTime = s.StartTime
        }

    case *httpstats.InHeader:
        if s.Client && s.StatusCode > 0 {
            state.statusCode = s.StatusCode
        }

    case *httpstats.OutHeader:
        if !s.Client && s.StatusCode > 0 {
            state.statusCode = s.StatusCode
        }

    case *httpstats.InData:
        if s.Length > 0 {
            state.bytesRead.Add(int64(s.Length))
        }

    case *httpstats.OutData:
        if s.Length > 0 {
            state.bytesWrote.Add(int64(s.Length))
        }

    case *httpstats.End:
        labels := prometheus.Labels{
            "method":      state.method,
            "status_code": statusString(state.statusCode),
        }

        // Merge mid-flight tags into metric labels.
        if tags := httpstats.FromContext(ctx); tags != nil {
            tags.Range(func(k, v string) bool {
                labels[k] = v
                return true
            })
        }

        duration := s.EndTime.Sub(state.startTime)
        h.requestDuration.With(labels).Observe(duration.Seconds())
        h.requestsTotal.With(labels).Inc()

        bytesRead := state.bytesRead.Load()
        bytesWrote := state.bytesWrote.Load()

        // Map payload direction depending on client vs. server.
        reqBytes := bytesRead
        respBytes := bytesWrote
        if s.Client {
            reqBytes = bytesWrote
            respBytes = bytesRead
        }
        if reqBytes > 0 {
            h.requestBodyBytes.With(prometheus.Labels{"method": state.method}).Observe(float64(reqBytes))
        }
        if respBytes > 0 {
            h.responseBodyBytes.With(labels).Observe(float64(respBytes))
        }
    }
}

type requestState struct {
    startTime  time.Time
    bytesRead  atomic.Int64
    bytesWrote atomic.Int64
    statusCode int
    method     string
}

type requestStateKey struct{}

func requestStateFromContext(ctx context.Context) *requestState {
    s, _ := ctx.Value(requestStateKey{}).(*requestState)
    return s
}

func statusString(code int) string {
    if code >= 100 && code <= 599 {
        return strconv.Itoa(code)
    }
    return strconv.Itoa(code)
}
```

Key points in the Prometheus handler:

- **Payload direction is explicit at `End`**. Because `InData`/`OutData` mean different things for server vs. client, the handler checks `s.Client` at `End` to decide which counter maps to request body and which to response body.
- **Labels are assembled at `End`**, not incrementally. Prometheus label values must be known when you call `With()`. Since the status code might not be known until `OutHeader`, the handler waits until `End` to build the label set.
- **Mid-flight tags are merged into labels**. The `Tags` container injected by the middleware lets downstream handlers add labels like `tenant_id`. The Prometheus handler reads them at `End` and merges them into the label map.
- **`prometheus.Collector` is implemented** so the handler registers directly with a registry.
- **Buckets are configurable**. Default buckets cover typical HTTP latency ranges and body sizes.

### Implementation Checklist

#### Concurrency

- Use `sync/atomic` for all per-request counters. `HandleRequest` for `InData` and `OutData` may fire from a different goroutine than `End`.
- Connection-level state (`HandleConn`) must also be thread-safe.

#### Context state

- Store per-request state with `context.WithValue` using an unexported key.
- Return early from `HandleRequest` if state is nil.

#### Payload direction

- Always check `s.Client` when interpreting `InData`/`OutData`. The meaning flips between server and client.

#### Mid-flight tags

- Read tags at `End` via `httpstats.FromContext(ctx)` and merge them into span attributes or metric labels.

#### Error handling

- Use `httpstats.ClassifyEndError` to distinguish client disconnects, deadlines, and panics.

#### Naming

- Use `httpstats.DefaultOperationName` for span and metric names. It picks up `http.Request.Pattern` (Go 1.22+) for low-cardinality names.

#### Registration

- Prometheus: implement `prometheus.Collector`.
- OTEL: default to no-op providers so the handler works before exporters are wired up.

### Usage

Once your handler is implemented, wire it into your server or client. The
core package provides `ServerHandler` for middleware and `ClientTransport`
for `http.RoundTripper`.

#### Server middleware

```go
// 1. Create your handler.
handler, _ := otel.NewServerHandler(otel.WithTracerProvider(tp))

// 2. Wrap your mux.
mux := http.NewServeMux()
mux.HandleFunc("GET /users/{id}", handleUser)
wrapped := httpstats.ServerHandler(mux, httpstats.WithServerHandler(handler))

// 3. Use the wrapped handler with your server.
http.ListenAndServe(":8080", wrapped)
```

`ServerHandler` fires events before and after the inner handler runs. It
also recovers panics, writes a 500 response, and fires `End` with a
`panicError`. Your handler's `End` case will see this error — use
`httpstats.ClassifyEndError` to detect it.

#### Client transport

```go
handler, _ := otel.NewClientHandler(otel.WithTracerProvider(tp))
ct := httpstats.NewClientTransport(httpstats.WithClientHandler(handler))
client := &http.Client{Transport: ct}

resp, err := client.Get("https://api.example.com/data")
if err != nil {
    // End was already fired with the error by the transport.
    return err
}
defer resp.Body.Close()
// Read the body — End fires when the body is fully consumed or closed.
io.Copy(io.Discard, resp.Body)
```

On the client side, `End` fires when the response body is fully consumed or
closed. If `RoundTrip` itself fails, `End` fires immediately with the
error. The transport uses `sync.Once` so you never get duplicate `End`
events.

#### Combining handlers

Use `MultiHandler` to run multiple handlers side by side — OTEL for tracing
and Prometheus for metrics, for example:

```go
otelHandler, _ := otel.NewServerHandler(otel.WithTracerProvider(tp))
promHandler := prometheus.NewHandler()
prometheus.MustRegister(promHandler)

composed := httpstats.MultiHandler{otelHandler, promHandler}
wrapped := httpstats.ServerHandler(mux, httpstats.WithServerHandler(composed))
```

Tag methods chain — each handler receives the context returned by the
each handler receives the context returned by the
previous one. Handle methods fire on all handlers independently.

#### Enriching telemetry mid-flight (Tags)

Downstream handlers can enrich telemetry after `TagRequest` has already
run. The middleware injects a `Tags` container into every request context:

```go
func handleUser(w http.ResponseWriter, r *http.Request) {
    // Discovered during auth — not known at TagRequest time.
    if tags := httpstats.FromContext(r.Context()); tags != nil {
        tags.Set("tenant_id", extractTenant(r))
        tags.Set("user_role", "admin")
    }
    // ... business logic ...
}
```

Both reference implementations read tags at `End` and merge them into span
attributes or metric labels. Only use low-cardinality values — every unique
`tenant_id` creates a new Prometheus label combination.

#### Best practices

- **Create handlers once, reuse them**. Handler instances are safe for concurrent use. Don't allocate a new handler per request.
- **Register Prometheus handlers before starting the server**. `prometheus.MustRegister` panics on duplicate registration.
- **Wire OTEL providers before creating the handler**. If you create the handler with no-op providers, spans and metrics are silently discarded until you swap in real providers.
- **Set operation names early**. Use `WithServerNameFunc` or `WithClientNameFunc` to populate `OperationName` if the built-in pattern logic doesn't cover your router.
- **Don't use tags unless you need them**. The `Tags` container allocates a mutex and map. If you skip mid-flight enrichment, `FromContext` returns nil and backends skip the extra work.
- **Use `sync/atomic` for any shared state**. Anything touched by both `InData`/`OutData` and `End` must be atomic.
