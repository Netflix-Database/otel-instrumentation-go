package otel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// installTestTracing swaps in an in-memory tracer provider and the SDK's
// propagator, and restores the globals afterwards.
func installTestTracing(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
	return recorder
}

func contextWithRequestID(t *testing.T, id string) context.Context {
	t.Helper()
	m, err := baggage.NewMember(RequestIDBaggageKey, id)
	if err != nil {
		t.Fatal(err)
	}
	bag, err := baggage.New(m)
	if err != nil {
		t.Fatal(err)
	}
	return baggage.ContextWithBaggage(context.Background(), bag)
}

// The next service receives the trace context, the baggage and the request id
// header, and the call is recorded as a CLIENT span.
func TestNewTransportPropagatesAndRecordsClientSpan(t *testing.T) {
	recorder := installTestTracing(t)

	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
	}))
	defer srv.Close()

	client := &http.Client{Transport: NewTransport(http.DefaultTransport)}
	req, _ := http.NewRequestWithContext(contextWithRequestID(t, "req-123"), http.MethodGet, srv.URL, nil)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	if got.Get(RequestIDHeader) != "req-123" {
		t.Errorf("request id header = %q, want req-123", got.Get(RequestIDHeader))
	}
	if got.Get("traceparent") == "" {
		t.Error("traceparent was not propagated")
	}
	if got.Get("baggage") == "" {
		t.Error("baggage was not propagated")
	}

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if spans[0].SpanKind() != trace.SpanKindClient {
		t.Errorf("span kind = %v, want client", spans[0].SpanKind())
	}
	// The downstream server's parent must be the client span, or the
	// service graph cannot pair the two halves.
	sc := trace.SpanContextFromContext(propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(got)))
	if sc.SpanID() != spans[0].SpanContext().SpanID() {
		t.Error("traceparent does not point at the client span")
	}
}

// A RoundTripper must not modify the caller's request, and an explicit header
// set by the caller wins over the baggage value.
func TestNewTransportLeavesCallerRequestAlone(t *testing.T) {
	installTestTracing(t)

	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(RequestIDHeader)
	}))
	defer srv.Close()
	client := &http.Client{Transport: NewTransport(nil)}

	req, _ := http.NewRequestWithContext(contextWithRequestID(t, "from-baggage"), http.MethodGet, srv.URL, nil)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if req.Header.Get(RequestIDHeader) != "" {
		t.Error("the caller's request was modified")
	}

	req, _ = http.NewRequestWithContext(contextWithRequestID(t, "from-baggage"), http.MethodGet, srv.URL, nil)
	req.Header.Set(RequestIDHeader, "explicit")
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if got != "explicit" {
		t.Errorf("request id header = %q, want the caller's explicit value", got)
	}
}

// Wrapping twice must not produce two spans per request.
func TestNewTransportIsIdempotent(t *testing.T) {
	once := NewTransport(http.DefaultTransport)
	if NewTransport(once) != once {
		t.Error("wrapping an instrumented transport wrapped it again")
	}
}

// SetupOTelSDK instruments http.DefaultTransport, so plain http.Client{}
// values and http.DefaultClient produce client spans with no code changes.
func TestSetupInstrumentsDefaultTransport(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv("OTEL_SDK_DISABLED", "true")

	prev := http.DefaultTransport
	t.Cleanup(func() {
		http.DefaultTransport = prev
		instrumentDefaultTransportOnce = sync.Once{}
	})

	shutdown, err := SetupOTelSDK(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(context.Background())

	if _, ok := http.DefaultTransport.(*transport); !ok {
		t.Fatalf("http.DefaultTransport is %T, want the instrumented transport", http.DefaultTransport)
	}
	wrapped := http.DefaultTransport

	// A second setup must not wrap it again.
	shutdown2, err := SetupOTelSDK(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown2(context.Background())
	if http.DefaultTransport != wrapped {
		t.Error("a second SetupOTelSDK wrapped http.DefaultTransport again")
	}
}
