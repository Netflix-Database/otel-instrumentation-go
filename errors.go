package otel

import (
	"context"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// RecordError records the exception on the span AND sets the span status to
// Error.
//
// Both are required. RecordError alone leaves the status Unset, so the span is
// not counted as a failure and "error rate per endpoint" or "per query"
// silently under-reports on the shared dashboard. Having one helper is the
// only way this stays consistent across services.
func RecordError(span trace.Span, err error) {
	if span == nil || err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// RecordErrorOnContext records against whichever span is active on ctx.
func RecordErrorOnContext(ctx context.Context, err error) {
	if err == nil {
		return
	}
	RecordError(trace.SpanFromContext(ctx), err)
}

// WithErrorRecording runs fn and records any error with the correct span
// status before returning it unchanged.
func WithErrorRecording(ctx context.Context, fn func(context.Context) error) error {
	err := fn(ctx)
	if err != nil {
		RecordErrorOnContext(ctx, err)
	}
	return err
}
