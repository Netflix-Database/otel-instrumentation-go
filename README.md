# otel-instrumentation-go

Shared OpenTelemetry, logging and health wiring for Netdb's Go services.
Services depend on this package instead of configuring the SDK themselves, so
every service emits the same span attributes, the same log schema and the same
metrics — which is what makes one Grafana dashboard work across all of them.

The Node equivalent is `otel-instrumentation-next`. The two are kept in
deliberate lockstep: same header, same baggage key, same health paths, same
semconv version, same log field names.

## Installation

```bash
go get github.com/Netflix-Database/otel-instrumentation-go
```

## Quick start

```go
package main

import (
	"context"
	"net/http"

	otel "github.com/Netflix-Database/otel-instrumentation-go"
	otelhttp "github.com/Netflix-Database/otel-instrumentation-go/http"
)

func main() {
	ctx := context.Background()

	// Fails immediately with every configuration problem listed.
	shutdown, err := otel.SetupOTelSDK(ctx)
	if err != nil {
		panic(err)
	}

	logger := otel.CreateLogger("orders-api")

	mux := http.NewServeMux()
	otelhttp.RegisterHealth(mux, []otelhttp.DependencyCheck{
		{Name: "db", Check: db.PingContext},
		{Name: "redis", Check: func(ctx context.Context) error { return rdb.Ping(ctx).Err() }},
	}, 0)

	srv := &http.Server{Addr: ":8080", Handler: otelhttp.RequestIDMiddleware(mux)}
	go func() { _ = srv.ListenAndServe() }()

	logger.Info("service started")

	// Drains the server, closes dependencies, then flushes telemetry.
	_ = otel.WaitForShutdown(otel.ShutdownOptions{
		Server:            srv,
		CloseDependencies: func(ctx context.Context) error { return db.Close() },
		Shutdown:          shutdown,
	})
}
```

## Graceful shutdown

`WaitForShutdown` blocks on SIGTERM/SIGINT and then drains in a fixed order:

1. stop accepting requests, finish in-flight ones
2. close db/redis/rabbit connections
3. flush the OTel exporters

Exporters go last on purpose: draining and closing pools both produce spans, and
anything recorded after the flush is lost — which is exactly the telemetry you
want when a deploy goes wrong.

`Timeout` defaults to 15s and must stay below your orchestrator's grace period,
or the flush never completes before SIGKILL.

## Health endpoints

This builds on [`hellofresh/health-go`](https://github.com/hellofresh/health-go)
rather than replacing it, so its ready-made probes work unchanged — a
`DependencyCheck.Check` is exactly a `health.CheckFunc`:

```go
import (
    healthMySql "github.com/hellofresh/health-go/v5/checks/mysql"
    healthRedis "github.com/hellofresh/health-go/v5/checks/redis"
)

otelhttp.RegisterHealth(mux,
    otelhttp.DependencyCheck{
        Name:  "db",
        Check: healthMySql.New(healthMySql.Config{DSN: dsn}),
    },
    otelhttp.DependencyCheck{
        Name:  "redis",
        Check: healthRedis.New(healthRedis.Config{DSN: redisDSN}),
    },
    otelhttp.DependencyCheck{
        Name:        "search",
        Check:       func(ctx context.Context) error { return search.Ping(ctx) },
        NonCritical: true,
    },
)
```

It mounts `/livez` and `/readyz`. All this package adds is the two paths and a
response body identical to the .NET and Node services — health-go's own
`Handler()` emits a different shape (`failures`, `system`, `component`), which
the shared dashboard cannot parse.

`/livez` runs **no checks at all** — a liveness probe that touches the database
restarts the app whenever the database blips, turning a dependency outage into
an outage plus a restart loop. Its body is `{"status":"ok"}` with no `checks`
key.

`/readyz` returns 503 when a critical check fails, and 200 + `"degraded"` when
only `NonCritical` ones do — that is health-go's `SkipOnErr`, so the mapping is
its `Unavailable`/`Partially Available` split rather than bookkeeping of our
own. Each check gets a 3s timeout by default, enforced by health-go, so one
hung dependency cannot hold the probe open.

`duration_ms` is measured by this package: health-go reports *which* checks
failed but not how long each took, and that field is part of the response
contract shared with the other two languages.

## Request id and baggage

```go
// Public edge: inbound baggage is dropped entirely.
handler := otelhttp.RequestIDMiddleware(mux)

// Internal service: keep specific keys from callers you trust.
handler := otelhttp.RequestIDMiddlewareWithOptions(otelhttp.Options{
	TrustInboundBaggage: true,
	AllowedBaggageKeys:  []string{"request.id", "tenant.id"},
})(mux)

// Outbound: nothing to do for most clients - see "Outgoing HTTP" below.
// InjectOutbound is only for a client that bypasses both instrumented
// transports.
req = otelhttp.InjectOutbound(req)
```

An inbound `X-Request-Id` is reused only if it is at most 128 characters and
matches `[A-Za-z0-9_.:-]+`. It reaches log lines and span attributes, so an
unchecked value is a log-forging vector and an unbounded metric cardinality
source — a comma alone would corrupt the baggage header.

Inbound baggage can never overwrite the request id, even from a trusted caller:
a compromised internal service should not be able to relabel everyone else's
traces.

`traceparent` is deliberately left alone. Dropping it would break distributed
traces, and unlike baggage it is structurally validated and carries no
free-form values.

## Outgoing HTTP

`SetupOTelSDK` wraps `http.DefaultTransport` with otelhttp. Every call made
through it gets a CLIENT span, and the next service receives `traceparent`,
`baggage` and `X-Request-Id`. That covers `http.Get`, `http.DefaultClient` and
any `http.Client` whose `Transport` is nil — no code changes.

A client with its own transport bypasses that, so wrap it yourself:

```go
client := &http.Client{Transport: otel.NewTransport(&http.Transport{
	MaxIdleConnsPerHost: 32,
})}
```

Wrapping twice is harmless: an already-wrapped transport is returned
unchanged, so there is never a second span per request. A request id header the
caller set explicitly is kept.

Build requests with `http.NewRequestWithContext(ctx, ...)`. Without the
request's context the client span has no parent, so the call shows up in
Tempo as a trace of its own and carries no request id.

The one thing replacing `http.DefaultTransport` breaks is code that asserts
`http.DefaultTransport.(*http.Transport)`. None of the Go services'
dependencies do; check again when adding a library that tunes the default
transport.

## Errors on spans

```go
otel.RecordErrorOnContext(ctx, err)
// or
err := otel.WithErrorRecording(ctx, func(ctx context.Context) error { ... })
```

`RecordError` alone leaves the span status Unset, so the span is not counted as
a failure and "error rate per endpoint" under-reports. These helpers always do
both.

## Logging

zap, teed to the console and the OTLP log pipeline. `LoggerFromContext`
and `WithContext` attach `trace_id`, `span_id` and `request.id`.

Secrets are redacted before anything reaches a sink, under a contract shared
verbatim with the other two libraries — a secret that leaks in one language must
leak in all three, or the weakest service decides what ends up in the log
backend:

1. A name is **normalised** before matching: lowercased, with `-`, `_`, `.` and
   spaces removed. `api_key`, `apiKey`, `X-API-KEY` and `Api.Key` all reduce to
   `apikey`. HTTP header names are hyphenated and headers are the most common
   accidental leak.
2. The normalised name is **substring-matched** against `password`, `passwd`,
   `secret`, `token`, `apikey`, `authorization`, `cookie`, `credential`. The
   usual leak is a field that gained a secret months after the logging call was
   written, so `sessionToken` and `db_credential` match too.
3. A match replaces the **entire value** with `[redacted]`, whatever its type —
   the contents of a matching key are never inspected.
4. Matching applies at **every depth** up to 8, not only to top-level fields.

A value with nothing to censor is passed through as the instance it arrived as,
so clean log lines keep their exact shape and cost one walk with no allocation.
Only the branches holding a secret are rebuilt.

The walk covers `zap.Any`/`zap.Reflect` values, `zap.Object`/`zap.Array`
marshalers and `zap.Namespace`, and both the console and the exporter see the
censored values — redacting in only one place is how secrets end up in exactly
the backend you forgot about.

In development (`OTEL_SDK_DISABLED=true`) the logger prints colourised console
output and attaches no exporter.

## Configuration

Validated at startup by `LoadConfig()`, which reports every problem at once
rather than one redeploy at a time. See `.env.example`.

| Variable | Notes |
|---|---|
| `OTEL_SDK_DISABLED` | `true` in dev. Nothing else is then required. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | The collector endpoint. Use the gRPC port (4317) - the exporters are gRPC. |
| `OTEL_RESOURCE_ATTRIBUTES` | Must contain `service.name`, `service.version`, `service.instance.id`, `deployment.environment.name`, `cloud.region`. |
| `OTEL_TRACES_SAMPLER_ARG` | Parent-based ratio, 0.0–1.0, default 1.0. |
| `OTEL_SEMCONV_STABILITY_OPT_IN` | Set to `database,http` automatically; your value is merged. |
| `LOG_LEVEL` | Defaults to `debug` when disabled, `info` otherwise. |

> **Note:** `OTEL_PRETTY_PRINT` from previous versions is gone. Use
> `OTEL_SDK_DISABLED=true`, which is the standard OTel variable and matches the
> Node services.

## Semantic conventions

This package targets **semconv 1.43.0** and sets
`OTEL_SEMCONV_STABILITY_OPT_IN=database,http` in `init()`.

Attribute names moved between versions — `db.statement` became `db.query.text`,
`db.system` became `db.system.name`. The shared dashboards are built against
this version, so bumping it is a breaking change for them and must happen across
all languages at once.

Set the variable in the deployment environment as well: contrib instrumentation
packages read it when they initialise, and depending on import order `init()`
can run too late. Setting it twice is harmless; setting it nowhere is a silent
divergence.

## Instrumentation this package does not wrap

MySQL, Redis and RabbitMQ spans come from the
upstream libraries, called directly by each service:

```go
db, _ := otelsql.Open("mysql", dsn, otelsql.WithAttributes(...))  // github.com/XSAM/otelsql
redisotel.InstrumentTracing(rdb)                                   // redis/go-redis/v9
```

Wrapping them here would force every consumer to pull `go-redis`, `amqp091` and
a SQL driver into its build whether it uses them or not. The semconv opt-in this
package sets is what keeps their attribute names aligned with the Node services.
