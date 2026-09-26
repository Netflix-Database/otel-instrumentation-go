package otel

import (
	"net/http"
	"sync"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/baggage"
)

// NewTransport wraps base - http.DefaultTransport when nil - so every
// outgoing request gets a CLIENT span and carries the trace context, baggage
// and the X-Request-Id header on to the next service.
//
// SetupOTelSDK already does this to http.DefaultTransport, which covers
// http.Get, http.DefaultClient and any http.Client with a nil Transport. Call
// this only for a client that brings its own transport:
//
//	client := &http.Client{Transport: otel.NewTransport(&http.Transport{...})}
//
// Wrapping an already wrapped transport returns it unchanged, so there is
// never a second span per request.
func NewTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if _, ok := base.(*transport); ok {
		return base
	}
	return &transport{next: otelhttp.NewTransport(base)}
}

type transport struct {
	next http.RoundTripper
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	// otelhttp injects traceparent and baggage; the request id header is
	// ours. A header the caller set explicitly wins.
	if id := baggage.FromContext(req.Context()).Member(RequestIDBaggageKey).Value(); id != "" && req.Header.Get(RequestIDHeader) == "" {
		// A RoundTripper must not modify the caller's request.
		req = req.Clone(req.Context())
		req.Header.Set(RequestIDHeader, id)
	}
	return t.next.RoundTrip(req)
}

// CloseIdleConnections lets http.Client.CloseIdleConnections reach the
// underlying transport, as it would without the wrapper.
func (t *transport) CloseIdleConnections() {
	if c, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

var instrumentDefaultTransportOnce sync.Once

// instrumentDefaultTransport replaces http.DefaultTransport with an
// instrumented one. Most outbound calls in the services go through it without
// ever naming a transport, so this is what gives them client spans - and the
// service map its outgoing edges - without touching every call site.
//
// Checked against every dependency of the Go services: none type-asserts
// http.DefaultTransport to *http.Transport, which is the one thing this would
// break.
func instrumentDefaultTransport() {
	instrumentDefaultTransportOnce.Do(func() {
		http.DefaultTransport = NewTransport(http.DefaultTransport)
	})
}
