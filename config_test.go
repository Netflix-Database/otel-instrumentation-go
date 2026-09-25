package otel

import (
	"os"
	"strings"
	"testing"
)

func clearOTelEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, "OTEL_") {
			t.Setenv(key, "")
			_ = os.Unsetenv(key)
		}
	}
	t.Setenv("GIT_SHA", "")
	_ = os.Unsetenv("GIT_SHA")
}

const validAttrs = "service.name=svc,service.version=abc123,service.instance.id=i-1," +
	"deployment.environment.name=prod,cloud.region=eu-central-1"

// Development needs none of the production configuration.
func TestLoadConfig_DisabledShortCircuits(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv("OTEL_SDK_DISABLED", "true")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("expected no error when disabled, got %v", err)
	}
	if !cfg.Disabled {
		t.Fatal("expected Disabled to be true")
	}
}

// Every problem reported at once, not one redeploy at a time.
func TestLoadConfig_ReportsAllProblemsTogether(t *testing.T) {
	clearOTelEnv(t)

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected an error for empty configuration")
	}
	msg := err.Error()

	if !strings.Contains(msg, "OTEL_EXPORTER_OTLP_ENDPOINT is required") {
		t.Errorf("missing endpoint problem in: %s", msg)
	}
	for _, attr := range RequiredResourceAttributes {
		// service.instance.id always falls back to the hostname.
		if attr == "service.instance.id" {
			continue
		}
		if !strings.Contains(msg, attr) {
			t.Errorf("missing %q in: %s", attr, msg)
		}
	}
	if got := strings.Count(msg, "  - "); got < 5 {
		t.Errorf("expected all problems batched, got %d", got)
	}
}

const staticAttrs = "service.name=svc,deployment.environment.name=prod,cloud.region=eu-central-1"

// The build SHA and container only exist at build and run time, so they must
// not have to be set by hand.
func TestLoadConfig_VersionAndInstanceFallBack(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4317")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", staticAttrs)
	t.Setenv("GIT_SHA", "deadbeef")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("expected valid config, got %v", err)
	}
	host, _ := os.Hostname()
	if got := cfg.ResourceAttributes["service.version"]; got != "deadbeef" {
		t.Errorf("service.version = %q, want deadbeef", got)
	}
	if got := cfg.ResourceAttributes["service.instance.id"]; got != host {
		t.Errorf("service.instance.id = %q, want %q", got, host)
	}
}

func TestLoadConfig_ExplicitAttributesWin(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4317")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", validAttrs)
	t.Setenv("GIT_SHA", "deadbeef")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("expected valid config, got %v", err)
	}
	if got := cfg.ResourceAttributes["service.version"]; got != "abc123" {
		t.Errorf("service.version = %q, want abc123", got)
	}
	if got := cfg.ResourceAttributes["service.instance.id"]; got != "i-1" {
		t.Errorf("service.instance.id = %q, want i-1", got)
	}
}

// No silent default in production: a build without GIT_SHA still fails.
func TestLoadConfig_MissingGitShaIsReported(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4317")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", staticAttrs)

	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), `"service.version"`) {
		t.Fatalf("expected missing service.version, got %v", err)
	}
	if strings.Count(err.Error(), "  - ") != 1 {
		t.Errorf("expected exactly one problem, got: %v", err)
	}
}

func TestLoadConfig_Valid(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4317")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", validAttrs)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("expected valid config, got %v", err)
	}
	if cfg.ServiceName != "svc" {
		t.Errorf("service name = %q, want svc", cfg.ServiceName)
	}
	// Default is keep-everything.
	if cfg.SamplingRatio != 1.0 {
		t.Errorf("sampling ratio = %v, want 1.0", cfg.SamplingRatio)
	}
	// The build SHA travels as service.version.
	if cfg.ResourceAttributes["service.version"] != "abc123" {
		t.Errorf("service.version = %q", cfg.ResourceAttributes["service.version"])
	}
}

func TestLoadConfig_RejectsOutOfRangeSampler(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4317")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", validAttrs)
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "1.5")

	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "[0,1]") {
		t.Fatalf("expected out-of-range sampler to be rejected, got %v", err)
	}
}

// The opt-in must be set, and must merge rather than clobber.
func TestApplySemconvStabilityOptIn(t *testing.T) {
	t.Setenv("OTEL_SEMCONV_STABILITY_OPT_IN", "")
	_ = os.Unsetenv("OTEL_SEMCONV_STABILITY_OPT_IN")

	ApplySemconvStabilityOptIn()
	if got := os.Getenv("OTEL_SEMCONV_STABILITY_OPT_IN"); got != SemconvStabilityOptIn {
		t.Fatalf("got %q, want %q", got, SemconvStabilityOptIn)
	}

	t.Setenv("OTEL_SEMCONV_STABILITY_OPT_IN", "custom")
	ApplySemconvStabilityOptIn()
	got := os.Getenv("OTEL_SEMCONV_STABILITY_OPT_IN")
	for _, want := range []string{"custom", "database", "http"} {
		if !strings.Contains(got, want) {
			t.Errorf("merged opt-in %q missing %q", got, want)
		}
	}
}

// Every language must agree on the header and baggage key.
func TestCrossLanguageConstants(t *testing.T) {
	if RequestIDHeader != "X-Request-Id" {
		t.Errorf("request id header drifted: %q", RequestIDHeader)
	}
	if RequestIDBaggageKey != "request.id" {
		t.Errorf("baggage key drifted: %q", RequestIDBaggageKey)
	}
	if LivenessPath != "/livez" || ReadinessPath != "/readyz" {
		t.Errorf("health paths drifted: %q %q", LivenessPath, ReadinessPath)
	}
}
