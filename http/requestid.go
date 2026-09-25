package http

import (
	"context"
	"net/http"
	"regexp"

	otelcfg "github.com/Netflix-Database/otel-instrumentation-go"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// RequestIDHeader is re-exported so callers need not import both packages.
const RequestIDHeader = otelcfg.RequestIDHeader

const requestIDMaxLength = 128

// An inbound request id is attacker-controlled at the edge and ends up in log
// lines and span attributes, so it is length-capped and character-restricted.
// Without this it is a log-forging vector and an unbounded metric cardinality
// source. Commas in particular would corrupt the baggage header.
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)

func validRequestID(id string) bool {
	return id != "" && len(id) <= requestIDMaxLength && requestIDPattern.MatchString(id)
}

// Options configures the middleware.
type Options struct {
	// TrustInboundBaggage must stay false at a public edge. Baggage propagates
	// verbatim to every downstream service, so an untrusted client could
	// otherwise inject arbitrary keys onto spans across the whole system
	//.
	TrustInboundBaggage bool
	// AllowedBaggageKeys are kept from inbound baggage when trusted. The
	// request id is always replaced with ours. Defaults to none.
	AllowedBaggageKeys []string
}

// RequestIDMiddleware ensures every request carries a valid request id,
// publishes it as baggage so it propagates to downstream services, and writes
// it onto the active span.
//
// Inbound baggage from untrusted callers is dropped entirely. traceparent is
// deliberately left alone: dropping it would break distributed traces, and
// unlike baggage it is structurally validated by the propagator and carries no
// free-form values.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return RequestIDMiddlewareWithOptions(Options{})(next)
}

// RequestIDMiddlewareWithOptions is RequestIDMiddleware with an explicit policy.
func RequestIDMiddlewareWithOptions(opts Options) func(http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, k := range opts.AllowedBaggageKeys {
		allowed[k] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(otelcfg.RequestIDHeader)
			if !validRequestID(id) {
				id = uuid.NewString()
			}
			r.Header.Set(otelcfg.RequestIDHeader, id)
			w.Header().Set(otelcfg.RequestIDHeader, id)

			ctx := r.Context()

			// Rebuild baggage from scratch rather than appending to
			// whatever arrived.
			members := make([]baggage.Member, 0, len(allowed)+1)
			if opts.TrustInboundBaggage {
				for _, m := range baggage.FromContext(ctx).Members() {
					if m.Key() == otelcfg.RequestIDBaggageKey || !allowed[m.Key()] {
						continue
					}
					members = append(members, m)
				}
			}
			if m, err := baggage.NewMember(otelcfg.RequestIDBaggageKey, id); err == nil {
				members = append(members, m)
			}
			if bag, err := baggage.New(members...); err == nil {
				ctx = baggage.ContextWithBaggage(ctx, bag)
			}

			span := trace.SpanFromContext(ctx)
			span.SetAttributes(attribute.String(otelcfg.RequestIDBaggageKey, id))

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequestIDFromContext returns the request id carried in baggage, if any.
func RequestIDFromContext(ctx context.Context) string {
	return baggage.FromContext(ctx).Member(otelcfg.RequestIDBaggageKey).Value()
}

// InjectOutbound copies trace context, baggage and the request id header onto
// an outbound request, so the id reaches the next service.
//
// otelhttp injects traceparent for instrumented clients, but nothing else sets
// the request id header downstream services read.
func InjectOutbound(req *http.Request) *http.Request {
	ctx := req.Context()
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	if m := baggage.FromContext(ctx).Member(otelcfg.RequestIDBaggageKey); m.Value() != "" {
		req.Header.Set(otelcfg.RequestIDHeader, m.Value())
	}
	return req
}
