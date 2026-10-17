## httpstats

Backend-agnostic `net/http` observability — single interface, any telemetry
backend. Modeled after gRPC-Go's `stats.Handler`. Zero dependencies beyond
the standard library.

```bash
go get github.com/riffbuild/httpstats
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
