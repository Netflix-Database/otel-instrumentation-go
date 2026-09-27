package http

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// serveTraced runs one request through otelhttp wrapping handler, the way the
// services build their servers, and returns the server span.
func serveTraced(t *testing.T, handler http.Handler, method, target string) sdktrace.ReadOnlySpan {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	h := otelhttp.NewHandler(handler, "/", otelhttp.WithTracerProvider(tp))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, target, nil))
	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	return spans[0]
}

func routeOf(span sdktrace.ReadOnlySpan) string {
	for _, kv := range span.Attributes() {
		if kv.Key == "http.route" {
			return kv.Value.AsString()
		}
	}
	return ""
}

func itemsMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("GET /{$}", func(http.ResponseWriter, *http.Request) {})
	return mux
}

// The services' own chain: otelhttp -> RequestIDMiddleware -> ServeMux. The
// middleware hands the mux a copy of the request, which used to hide the
// matched pattern from otelhttp.
func TestRequestIDMiddlewareRecordsRoute(t *testing.T) {
	span := serveTraced(t, RequestIDMiddleware(itemsMux()), "GET", "/items/42")
	if got := routeOf(span); got != "/items/{id}" {
		t.Errorf("http.route = %q, want /items/{id}", got)
	}
	if span.Name() != "GET /items/{id}" {
		t.Errorf("span name = %q, want GET /items/{id}", span.Name())
	}
}

// A chain without RequestIDMiddleware, like the storage node's.
func TestRouteMiddlewareRecordsRoute(t *testing.T) {
	passThrough := func(next http.Handler) http.Handler { return next }
	span := serveTraced(t, passThrough(RouteMiddleware(itemsMux())), "GET", "/items/7")
	if got := routeOf(span); got != "/items/{id}" {
		t.Errorf("http.route = %q, want /items/{id}", got)
	}
}

// "{$}" is ServeMux syntax, not part of the route.
func TestExactRootPattern(t *testing.T) {
	span := serveTraced(t, RequestIDMiddleware(itemsMux()), "GET", "/")
	if got := routeOf(span); got != "/" {
		t.Errorf("http.route = %q, want /", got)
	}
}

// An unmatched request records no route rather than a wrong one.
func TestNoRouteWhenNothingMatches(t *testing.T) {
	span := serveTraced(t, RequestIDMiddleware(itemsMux()), "GET", "/nope")
	if got := routeOf(span); got != "" {
		t.Errorf("http.route = %q, want none", got)
	}
}

// A custom router reports its own match.
func TestSetRouteFromCustomRouter(t *testing.T) {
	router := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetRoute(r, "/files/{id}")
	})
	span := serveTraced(t, RequestIDMiddleware(router), "DELETE", "/files/9")
	if got := routeOf(span); got != "/files/{id}" {
		t.Errorf("http.route = %q, want /files/{id}", got)
	}
	if span.Name() != "DELETE /files/{id}" {
		t.Errorf("span name = %q, want DELETE /files/{id}", span.Name())
	}
}

func TestRouteFromPattern(t *testing.T) {
	for pattern, want := range map[string]string{
		"GET /items/{id}":           "/items/{id}",
		"/items/{id}":               "/items/{id}",
		"example.com/items/":        "/items/",
		"POST example.com/a/{b...}": "/a/{b...}",
		"GET /{$}":                  "/",
		"":                          "",
	} {
		if got := routeFromPattern(pattern); got != want {
			t.Errorf("routeFromPattern(%q) = %q, want %q", pattern, got, want)
		}
	}
}
