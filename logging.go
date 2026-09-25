package otel

import (
	"context"
	"os"

	"go.opentelemetry.io/contrib/bridges/otelzap"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/log/global"
	oteltrace "go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// CreateLogger builds a zap logger that writes to the console and, unless the
// SDK is disabled, to the OpenTelemetry log pipeline.
//
// In development this is human-readable console output only, with no
// exporter attached.
func CreateLogger(name string) *zap.Logger {
	disabled := os.Getenv("OTEL_SDK_DISABLED") == "true"

	level := zap.InfoLevel
	if lvl := os.Getenv("LOG_LEVEL"); lvl != "" {
		if parsed, err := zapcore.ParseLevel(lvl); err == nil {
			level = parsed
		}
	} else if disabled {
		level = zap.DebugLevel
	}

	var consoleCore zapcore.Core
	if disabled {
		// Pretty, colourised output for local development.
		encCfg := zap.NewDevelopmentEncoderConfig()
		encCfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
		consoleCore = zapcore.NewCore(zapcore.NewConsoleEncoder(encCfg), zapcore.AddSync(os.Stdout), level)
	} else {
		// Structured JSON with the same field names the Node services
		// emit, so one Grafana query works across both.
		encCfg := zap.NewProductionEncoderConfig()
		encCfg.TimeKey = "time"
		encCfg.EncodeTime = zapcore.ISO8601TimeEncoder
		encCfg.LevelKey = "level"
		encCfg.MessageKey = "msg"
		encCfg.EncodeLevel = zapcore.LowercaseLevelEncoder
		consoleCore = zapcore.NewCore(zapcore.NewJSONEncoder(encCfg), zapcore.AddSync(os.Stdout), level)
	}

	if disabled {
		return zap.New(redactCore{consoleCore})
	}

	otelCore := otelzap.NewCore(name, otelzap.WithLoggerProvider(global.GetLoggerProvider()))
	return zap.New(redactCore{zapcore.NewTee(consoleCore, otelCore)})
}

// LoggerFromContext returns a logger carrying trace_id, span_id and request.id
// so every log line can be joined back to its trace.
func LoggerFromContext(ctx context.Context, name string) *zap.Logger {
	return WithContext(ctx, CreateLogger(name))
}

// WithContext attaches the correlation fields to an existing logger, so
// services can build their logger once rather than per request.
func WithContext(ctx context.Context, logger *zap.Logger) *zap.Logger {
	fields := make([]zap.Field, 0, 3)

	spanCtx := oteltrace.SpanContextFromContext(ctx)
	if spanCtx.IsValid() {
		fields = append(fields,
			zap.String("trace_id", spanCtx.TraceID().String()),
			zap.String("span_id", spanCtx.SpanID().String()),
		)
	}

	if member := baggage.FromContext(ctx).Member(RequestIDBaggageKey); member.Value() != "" {
		fields = append(fields, zap.String(RequestIDBaggageKey, member.Value()))
	}

	if len(fields) == 0 {
		return logger
	}
	return logger.With(fields...)
}
