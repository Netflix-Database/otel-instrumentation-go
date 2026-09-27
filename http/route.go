package http

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// SetRoute records the route template of the request - "/items/{id}", never
// the path - on the server span, as http.route, and names the span
// "GET /items/{id}".
//
// otelhttp only reads the route when the request starts, before any router has
// run, so without this every request shows as "(no route)" on the shared
// dashboards. http.ServeMux routes are recorded automatically by
// RequestIDMiddleware and RouteMiddleware; call this directly only from a
// router of your own once it has picked a route.
func SetRoute(r *http.Request, route string) {
	span := trace.SpanFromContext(r.Context())
	span.SetAttributes(attribute.String("http.route", route))
	span.SetName(r.Method + " " + route)
}

// RouteMiddleware wraps an http.ServeMux and records the pattern it matched
// once it has served the request.
//
// RequestIDMiddleware already does this, so it is only needed in a handler
// chain without it:
//
//	otelhttp.NewHandler(auth(otelMiddlewares.RouteMiddleware(mux)), "/")
func RouteMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		recordPattern(r)
	})
}

// recordPattern reads the pattern ServeMux wrote onto r while routing it.
func recordPattern(r *http.Request) {
	if route := routeFromPattern(r.Pattern); route != "" {
		SetRoute(r, route)
	}
}

// routeFromPattern turns a ServeMux pattern ("GET example.com/items/{id}")
// into the route template ("/items/{id}"): no method, no host, and no "{$}"
// end-of-path marker.
func routeFromPattern(pattern string) string {
	i := strings.IndexByte(pattern, '/')
	if i < 0 {
		return ""
	}
	return strings.TrimSuffix(pattern[i:], "{$}")
}
