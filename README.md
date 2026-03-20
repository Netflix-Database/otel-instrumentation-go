# otel-instrumentation-go

Small helpers for wiring OpenTelemetry traces, metrics, and logs in Go services, plus HTTP request ID middleware.

## What this package provides

- OTel SDK setup for traces, metrics, and logs via OTLP/gRPC
- A Zap logger that writes to both console and OpenTelemetry logs
- A context-aware logger helper that attaches `trace_id` and `span_id`
- HTTP middleware that ensures `X-Request-Id` is present and adds it to the active span

## Installation

```bash
go get github.com/Netflix-Database/otel-instrumentation-go
```

## Quick start

```go
package main

import (
	"context"
	"log"

	otel "github.com/Netflix-Database/otel-instrumentation-go"
)

func main() {
	ctx := context.Background()

	shutdown, err := otel.SetupOTelSDK(ctx)
	if err != nil {
		log.Fatalf("failed to set up OpenTelemetry: %v", err)
	}
	defer func() {
		if err := shutdown(context.Background()); err != nil {
			log.Printf("failed to shut down OpenTelemetry: %v", err)
		}
	}()

	logger := otel.CreateLogger("my-service")
	logger.Info("service started")
}
```

## Logger helpers

### `CreateLogger(name string) *zap.Logger`

Creates a Zap logger with a tee core:

- human-readable console output to stdout
- OpenTelemetry log output via the configured OTel logger provider

```go
logger := otel.CreateLogger("orders-api")
logger.Info("ready")
```

### `LoggerFromContext(ctx, name) *zap.Logger`

Returns a logger that includes trace correlation fields when a valid span context is present:

- `trace_id`
- `span_id`

```go
logger := otel.LoggerFromContext(ctx, "orders-api")
logger.Info("processing request")
```

## HTTP request ID middleware

Package `github.com/Netflix-Database/otel-instrumentation-go/http` provides middleware:

- reads `X-Request-Id` from incoming requests
- generates one if missing
- sets `X-Request-Id` on the response
- writes `request.id` span attribute on the active span

```go
package main

import (
	"fmt"
	"net/http"

	otelhttp "github.com/Netflix-Database/otel-instrumentation-go/http"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})

	handler := otelhttp.RequestIDMiddleware(mux)
	_ = http.ListenAndServe(":8080", handler)
}
```

## Environment configuration

### `OTEL_PRETTY_PRINT=true`

When set to `true`, this package creates local SDK providers without OTLP exporters. This is useful for local development when no collector is running.

When `OTEL_PRETTY_PRINT` is not `true`, exporters are created with default OTLP/gRPC settings from the OpenTelemetry Go SDK and standard `OTEL_*` environment variables.

Common variables include:

- `OTEL_EXPORTER_OTLP_ENDPOINT`
- `OTEL_EXPORTER_OTLP_HEADERS`
- `OTEL_RESOURCE_ATTRIBUTES`

## Notes

- Call the `shutdown` function returned by `SetupOTelSDK` during graceful shutdown so buffered telemetry is flushed.
- `RequestIDMiddleware` expects an active span in request context to attach `request.id`; if no active span exists, setting the attribute is a no-op.