## httpstats

Backend-agnostic `net/http` observability — single interface, any telemetry
backend. Modeled after gRPC-Go's `stats.Handler`. Zero dependencies beyond
the standard library.

```bash
go get github.com/piusalfred/httpstats
```

## Quick start

Write a handler that implements `httpstats.Handler` (see the [reference
implementations](docs/README.md) for OpenTelemetry and Prometheus), then
wire it in:

```go
// Server middleware
wrapped := httpstats.ServerHandler(mux, httpstats.WithServerHandler(myHandler))
http.ListenAndServe(":8080", wrapped)

// Client transport
ct := httpstats.NewClientTransport(httpstats.WithClientHandler(myHandler))
client := &http.Client{Transport: ct}
```

Combine multiple backends:

```go
httpstats.ServerHandler(mux, httpstats.WithServerHandler(otelHandler, promHandler))
```

## Event lifecycle

```text
Server: TagRequest → Begin → InHeader → InData* → OutHeader → OutData* → End
Client: TagRequest → Begin → OutHeader → OutData* → InHeader → InData* → End
```

Connection events (`TagConn → HandleConn`) fire independently.

## Context propagation

`RequestTagInfo.Header` is the request header seen in `TagRequest`, where a
handler also starts its span. On the client it is a private clone of the
outgoing request's header, so injecting trace context there changes what is
sent without mutating the caller's request. On the server it is the incoming
header, for extracting an upstream trace context; do not modify it.

```go
func (h *tracing) TagRequest(ctx context.Context, info *httpstats.RequestTagInfo) context.Context {
    if !info.Client {
        ctx = propagator.Extract(ctx, propagation.HeaderCarrier(info.Header))
    }
    ctx, span := tracer.Start(ctx, httpstats.DefaultOperationName(info))
    if info.Client {
        propagator.Inject(ctx, propagation.HeaderCarrier(info.Header))
    }
    return context.WithValue(ctx, spanKey{}, span)
}
```

## Server routing and std-lib behavior

Routers report where a request landed so recorders can label it without using
the concrete URL path:

```go
httpstats.AddGroup(ctx, "/users")   // router group prefix, accumulates when nested
httpstats.SetRoute(ctx, "/{id}")    // template relative to the group -> "/users/{id}"
httpstats.SetError(ctx, err)        // handler error returned by value
```

`End.Route` and `End.HandlerError` carry the final values (`http.Request.Pattern`
stands in when no group took the request). All three helpers are no-ops outside
`ServerHandler`.

`ServerHandler` stays transparent to `net/http`: the writer it passes on keeps
`http.Flusher`, `http.Hijacker` and `http.Pusher` exactly when the real one has
them and works with `http.ResponseController`; a handler that writes nothing
still gets an `OutHeader` for the implicit 200; panics are reported through
`End` and re-raised untouched, including `http.ErrAbortHandler`.

## Packages

| Path | Purpose |
|---|---|
| `httpstats` | Core interface, stats types, middleware, transport |
| `docs/` | Reference implementations for OTEL and Prometheus |

## Development

```bash
bash tools/setup.sh   # install gotools, sync tools
make hooks            # install pre-commit hooks
```

Tools are managed via [gotools](https://github.com/piusalfred/gotools).

### Make targets

| Command | Does |
|---|---|
| `make fmt` | go mod tidy, license headers, YAML, Go imports |
| `make lint` | golangci-lint (no fixes) |
| `make fix` | go fix + lint --fix + fmt |
| `make test` | Tests with race detector and coverage |
| `make ci` | Build + fmt + lint + vet + test + vulncheck + mdlint |
| `make hooks` | Install and run pre-commit hooks |
| `make tidy` | go mod tidy |
| `make cover` | Tests with coverage profile output |
| `make bench` | Run benchmarks |

### Pre-commit

On every commit, lefthook runs:

- `fmt` — formats Go files, stages the result
- `check` — golangci-lint + go vet
- `test` — full test suite with race detector

## License

MIT
