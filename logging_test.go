package otel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// captureLogger builds a logger writing JSON into buf, through the same
// redaction core the real logger uses.
func captureLogger(buf *bytes.Buffer) *zap.Logger {
	encCfg := zap.NewProductionEncoderConfig()
	core := zapcore.NewCore(zapcore.NewJSONEncoder(encCfg), zapcore.AddSync(buf), zap.DebugLevel)
	return zap.New(redactCore{core})
}

// Secrets must never reach the log backend.
func TestRedaction_CensorsSecretFields(t *testing.T) {
	var buf bytes.Buffer
	logger := captureLogger(&buf)

	logger.Info("login",
		zap.String("password", "hunter2"),
		zap.String("userPassword", "nested-secret"),
		zap.String("Authorization", "Bearer abc"),
		zap.String("cookie", "session=1"),
		zap.String("api_key", "ak-1"),
		zap.String("refreshToken", "rt-1"),
		zap.String("client_secret", "cs-1"),
		zap.String("username", "yannick"),
	)

	out := buf.String()
	for _, leak := range []string{"hunter2", "nested-secret", "Bearer abc", "session=1", "ak-1", "rt-1", "cs-1"} {
		if strings.Contains(out, leak) {
			t.Errorf("secret %q leaked into logs: %s", leak, out)
		}
	}
	if !strings.Contains(out, "yannick") {
		t.Errorf("non-secret field was dropped: %s", out)
	}
	if strings.Count(out, redactedPlaceholder) != 7 {
		t.Errorf("expected 7 redactions, got %d: %s", strings.Count(out, redactedPlaceholder), out)
	}
}

// Redaction must survive .With(), or per-request loggers leak.
func TestRedaction_AppliesThroughWith(t *testing.T) {
	var buf bytes.Buffer
	logger := captureLogger(&buf).With(zap.String("token", "tok-1"))
	logger.Info("request")

	if strings.Contains(buf.String(), "tok-1") {
		t.Errorf("secret leaked through With(): %s", buf.String())
	}
}

func TestRedaction_MatchingIsCaseInsensitive(t *testing.T) {
	for _, key := range []string{"PASSWORD", "Api_Key", "X-API-KEY", "apiKey", "Refresh-Token", "Secret"} {
		var buf bytes.Buffer
		captureLogger(&buf).Info("m", zap.String(key, "leak-me"))
		if strings.Contains(buf.String(), "leak-me") {
			t.Errorf("field %q was not redacted", key)
		}
	}
}

// The log shape must match what the Node services emit.
func TestLogSchema_FieldNames(t *testing.T) {
	var buf bytes.Buffer
	encCfg := zap.NewProductionEncoderConfig()
	encCfg.TimeKey = "time"
	encCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	encCfg.LevelKey = "level"
	encCfg.MessageKey = "msg"
	encCfg.EncodeLevel = zapcore.LowercaseLevelEncoder
	core := zapcore.NewCore(zapcore.NewJSONEncoder(encCfg), zapcore.AddSync(&buf), zap.DebugLevel)
	zap.New(redactCore{core}).Info("hello")

	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("not json: %v", err)
	}
	for _, key := range []string{"time", "level", "msg"} {
		if _, ok := m[key]; !ok {
			t.Errorf("missing %q in log line: %s", key, buf.String())
		}
	}
	if m["level"] != "info" {
		t.Errorf("level = %v, want lowercase name", m["level"])
	}
}

// Trace and request ids must reach the logs.
func TestWithContext_AddsCorrelationFields(t *testing.T) {
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(tracetest.NewInMemoryExporter()))
	ctx, span := tp.Tracer("t").Start(t.Context(), "op")
	defer span.End()

	var buf bytes.Buffer
	WithContext(ctx, captureLogger(&buf)).Info("hi")

	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if m["trace_id"] == nil || m["span_id"] == nil {
		t.Errorf("missing trace correlation: %s", buf.String())
	}
}

// Recording an error must also set the span status, or error rate
// panels under-report.
func TestRecordError_SetsSpanStatus(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	ctx, span := tp.Tracer("t").Start(t.Context(), "op")

	RecordErrorOnContext(ctx, errors.New("boom"))
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Status.Code != codes.Error {
		t.Errorf("status = %v, want Error", spans[0].Status.Code)
	}
	if len(spans[0].Events) == 0 {
		t.Error("exception event was not recorded")
	}
}

func TestWithErrorRecording_ReturnsErrorUnchanged(t *testing.T) {
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(tracetest.NewInMemoryExporter()))
	ctx, span := tp.Tracer("t").Start(t.Context(), "op")
	defer span.End()

	sentinel := errors.New("original")
	got := WithErrorRecording(ctx, func(context.Context) error { return sentinel })
	if !errors.Is(got, sentinel) {
		t.Errorf("error was not returned unchanged: %v", got)
	}
}
