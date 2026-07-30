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

## Packages

| Path | Purpose |
|---|---|
| `httpstats` | Core interface, stats types, middleware, transport |
| `docs/` | Reference implementations for OTEL and Prometheus |

## License

MIT
