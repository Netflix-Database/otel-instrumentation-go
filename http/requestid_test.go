package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	otelcfg "github.com/Netflix-Database/otel-instrumentation-go"
	"go.opentelemetry.io/otel/baggage"
)

// serve runs a request through the default middleware and reports what the
// downstream handler observed.
func serve(t *testing.T, req *http.Request) (*httptest.ResponseRecorder, string, baggage.Baggage) {
	t.Helper()
	var seenID string
	var seenBag baggage.Baggage
	rec := httptest.NewRecorder()
	RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenID = r.Header.Get(otelcfg.RequestIDHeader)
		seenBag = baggage.FromContext(r.Context())
	})).ServeHTTP(rec, req)
	return rec, seenID, seenBag
}

// A well-formed inbound id is reused so traces stay joined up.
func TestRequestID_ReusesValidInbound(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(otelcfg.RequestIDHeader, "abc-123")

	rec, seen, bag := serve(t, req)
	if seen != "abc-123" {
		t.Errorf("id = %q, want abc-123", seen)
	}
	if rec.Header().Get(otelcfg.RequestIDHeader) != "abc-123" {
		t.Error("id not echoed on the response")
	}
	if bag.Member(otelcfg.RequestIDBaggageKey).Value() != "abc-123" {
		t.Error("id not published as baggage")
	}
}

// The id reaches log lines and span attributes, so hostile values are replaced.
func TestRequestID_RejectsHostileInbound(t *testing.T) {
	cases := map[string]string{
		"space separated payload": "id level=error msg=fake",
		"comma splits baggage":    "a,b=c",
		"overlong":                strings.Repeat("x", 200),
		"empty":                   "",
		"semicolon metadata":      "id;prop=1",
	}
	for name, hostile := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			if hostile != "" {
				req.Header.Set(otelcfg.RequestIDHeader, hostile)
			}
			_, seen, _ := serve(t, req)
			if seen == hostile {
				t.Errorf("hostile id %q was accepted", hostile)
			}
			if !validRequestID(seen) {
				t.Errorf("replacement id %q is not itself valid", seen)
			}
		})
	}
}

// Untrusted baggage must not reach downstream services.
func TestBaggage_UntrustedInboundDropped(t *testing.T) {
	bag, _ := baggage.Parse("evil=1,tenant=acme")
	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(baggage.ContextWithBaggage(req.Context(), bag))

	_, _, got := serve(t, req)
	if got.Member("evil").Value() != "" {
		t.Error("untrusted baggage key survived")
	}
	if got.Member("tenant").Value() != "" {
		t.Error("untrusted baggage key survived")
	}
	if got.Member(otelcfg.RequestIDBaggageKey).Value() == "" {
		t.Error("our own request id should still be set")
	}
}

// Even a trusted caller only gets allow-listed keys through.
func TestBaggage_TrustedKeepsOnlyAllowListed(t *testing.T) {
	bag, _ := baggage.Parse("evil=1,tenant=acme")
	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(baggage.ContextWithBaggage(req.Context(), bag))

	var got baggage.Baggage
	mw := RequestIDMiddlewareWithOptions(Options{
		TrustInboundBaggage: true,
		AllowedBaggageKeys:  []string{"tenant"},
	})
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = baggage.FromContext(r.Context())
	})).ServeHTTP(httptest.NewRecorder(), req)

	if got.Member("tenant").Value() != "acme" {
		t.Error("allow-listed key was dropped")
	}
	if got.Member("evil").Value() != "" {
		t.Error("non-allow-listed key survived")
	}
}

// A trusted-but-compromised service must not be able to spoof the request id.
func TestBaggage_InboundCannotSpoofRequestID(t *testing.T) {
	bag, _ := baggage.Parse(otelcfg.RequestIDBaggageKey + "=spoofed")
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(otelcfg.RequestIDHeader, "real-id")
	req = req.WithContext(baggage.ContextWithBaggage(req.Context(), bag))

	var got baggage.Baggage
	mw := RequestIDMiddlewareWithOptions(Options{
		TrustInboundBaggage: true,
		AllowedBaggageKeys:  []string{otelcfg.RequestIDBaggageKey},
	})
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = baggage.FromContext(r.Context())
	})).ServeHTTP(httptest.NewRecorder(), req)

	if v := got.Member(otelcfg.RequestIDBaggageKey).Value(); v != "real-id" {
		t.Errorf("request id = %q, want the header value real-id", v)
	}
}

// The id must travel to the next service.
func TestInjectOutbound_SetsRequestIDHeader(t *testing.T) {
	m, _ := baggage.NewMember(otelcfg.RequestIDBaggageKey, "rid-9")
	bag, _ := baggage.New(m)

	out := httptest.NewRequest("GET", "http://downstream/", nil)
	out = out.WithContext(baggage.ContextWithBaggage(out.Context(), bag))
	out = InjectOutbound(out)

	if got := out.Header.Get(otelcfg.RequestIDHeader); got != "rid-9" {
		t.Errorf("outbound header = %q, want rid-9", got)
	}
}

func TestRequestIDFromContext(t *testing.T) {
	m, _ := baggage.NewMember(otelcfg.RequestIDBaggageKey, "rid-7")
	bag, _ := baggage.New(m)
	ctx := baggage.ContextWithBaggage(httptest.NewRequest("GET", "/", nil).Context(), bag)

	if got := RequestIDFromContext(ctx); got != "rid-7" {
		t.Errorf("got %q, want rid-7", got)
	}
}
