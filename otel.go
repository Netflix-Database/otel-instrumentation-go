package otel

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
)

// SetupOTelSDK wires traces, metrics and logs over OTLP/gRPC and returns a
// shutdown function that flushes them.
//
// The configuration is validated first, so a service with a bad
// OTEL_RESOURCE_ATTRIBUTES fails at boot instead of exporting unlabelled
// telemetry.
func SetupOTelSDK(ctx context.Context) (func(context.Context) error, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return func(context.Context) error { return nil }, err
	}
	return SetupOTelSDKWithConfig(ctx, cfg)
}

// SetupOTelSDKWithConfig is SetupOTelSDK with an already-validated config.
func SetupOTelSDKWithConfig(ctx context.Context, cfg *Config) (func(context.Context) error, error) {
	var shutdownFuncs []func(context.Context) error

	// Each registered cleanup is invoked once; errors are joined so one
	// failing exporter does not hide the others.
	shutdown := func(ctx context.Context) error {
		var err error
		for _, fn := range shutdownFuncs {
			err = errors.Join(err, fn(ctx))
		}
		shutdownFuncs = nil
		return err
	}

	// TraceContext plus Baggage, so the request id propagates.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	res, err := newResource(ctx, cfg)
	if err != nil {
		return shutdown, err
	}

	tracerProvider, err := newTracerProvider(ctx, cfg, res)
	if err != nil {
		return shutdown, errors.Join(err, shutdown(ctx))
	}
	shutdownFuncs = append(shutdownFuncs, tracerProvider.Shutdown)
	otel.SetTracerProvider(tracerProvider)

	meterProvider, err := newMeterProvider(ctx, cfg, res)
	if err != nil {
		return shutdown, errors.Join(err, shutdown(ctx))
	}
	shutdownFuncs = append(shutdownFuncs, meterProvider.Shutdown)
	otel.SetMeterProvider(meterProvider)

	loggerProvider, err := newLoggerProvider(ctx, cfg, res)
	if err != nil {
		return shutdown, errors.Join(err, shutdown(ctx))
	}
	shutdownFuncs = append(shutdownFuncs, loggerProvider.Shutdown)
	global.SetLoggerProvider(loggerProvider)

	// Runtime metrics - GC, heap, goroutines - as standard
	// instruments so one dashboard panel covers every Go service.
	if !cfg.Disabled {
		if err := runtime.Start(runtime.WithMeterProvider(meterProvider)); err != nil {
			return shutdown, errors.Join(fmt.Errorf("failed to start runtime metrics: %w", err), shutdown(ctx))
		}
	}

	return shutdown, nil
}

// newResource builds the resource from the validated attribute set
// rather than relying on the env detector, so a typo fails at startup instead
// of silently producing an unlabelled service.
func newResource(ctx context.Context, cfg *Config) (*resource.Resource, error) {
	if cfg.Disabled {
		return resource.Default(), nil
	}
	attrs := make([]attribute.KeyValue, 0, len(cfg.ResourceAttributes))
	for k, v := range cfg.ResourceAttributes {
		attrs = append(attrs, attribute.String(k, v))
	}
	return resource.New(ctx, resource.WithAttributes(attrs...))
}

func newTracerProvider(ctx context.Context, cfg *Config, res *resource.Resource) (*trace.TracerProvider, error) {
	// No exporters in development.
	if cfg.Disabled {
		return trace.NewTracerProvider(trace.WithResource(res)), nil
	}

	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP trace exporter: %w", err)
	}

	return trace.NewTracerProvider(
		trace.WithResource(res),
		trace.WithBatcher(exporter),
		// Parent-based, so a sampled trace stays sampled across
		// service hops. Without this, distributed traces come back with holes.
		trace.WithSampler(trace.ParentBased(trace.TraceIDRatioBased(cfg.SamplingRatio))),
	), nil
}

func newMeterProvider(ctx context.Context, cfg *Config, res *resource.Resource) (*metric.MeterProvider, error) {
	if cfg.Disabled {
		return metric.NewMeterProvider(metric.WithResource(res)), nil
	}

	exporter, err := otlpmetricgrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP metric exporter: %w", err)
	}

	return metric.NewMeterProvider(
		metric.WithResource(res),
		metric.WithReader(metric.NewPeriodicReader(exporter)),
	), nil
}

func newLoggerProvider(ctx context.Context, cfg *Config, res *resource.Resource) (*log.LoggerProvider, error) {
	// Logs go over OTLP like traces and metrics.
	if cfg.Disabled {
		return log.NewLoggerProvider(log.WithResource(res)), nil
	}

	exporter, err := otlploggrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP log exporter: %w", err)
	}

	return log.NewLoggerProvider(
		log.WithResource(res),
		log.WithProcessor(log.NewBatchProcessor(exporter)),
	), nil
}
