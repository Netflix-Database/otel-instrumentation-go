package http

import (
	"net/http"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const requestIDHeader = "X-Request-Id"

func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if id == "" {
			id = uuid.NewString()
			r.Header.Set(requestIDHeader, id)
		}
		w.Header().Set(requestIDHeader, id)

		span := trace.SpanFromContext(r.Context())
		span.SetAttributes(attribute.String("request.id", id))

		next.ServeHTTP(w, r)
	})
}
