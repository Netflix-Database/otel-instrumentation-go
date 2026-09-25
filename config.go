package otel

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// RequestIDHeader is the one request-id header used by every Netdb service, in
// every language. Changing it breaks cross-service correlation.
const RequestIDHeader = "X-Request-Id"

// RequestIDBaggageKey is the baggage key the request id travels under, and the
// span attribute it is written to.
const RequestIDBaggageKey = "request.id"

// Health endpoint paths, identical across every service so one probe config
// and one dashboard panel work everywhere.
const (
	LivenessPath  = "/livez"
	ReadinessPath = "/readyz"
)

// SemconvVersion is the OpenTelemetry semantic convention version this package
// emits. Attribute names moved between versions - db.statement became
// db.query.text, db.system became db.system.name - so the shared Grafana
// dashboards are built against exactly this version. Bumping it is a breaking
// change for those dashboards and must happen across all languages at once.
const SemconvVersion = "1.43.0"

// SemconvStabilityOptIn must be set before instrumentation packages initialise,
// otherwise each falls back to its own default convention and the database and
// http attribute names diverge between services.
const SemconvStabilityOptIn = "database,http"

// RequiredResourceAttributes is the set every service must report.
// A missing one produces traces that cannot be filtered by environment or
// region, which is only ever discovered when you need them.
var RequiredResourceAttributes = []string{
	"service.name",
	"service.version",
	"service.instance.id",
	"deployment.environment.name",
	"cloud.region",
}

// Config is the validated telemetry configuration.
type Config struct {
	// Disabled mirrors OTEL_SDK_DISABLED. When true the SDK installs
	// no exporters and the logger prints human-readable console output.
	Disabled bool
	// Endpoint is the OTLP/gRPC endpoint. No other protocol is supported.
	Endpoint string
	// SamplingRatio is the parent-based ratio.
	SamplingRatio float64
	// ResourceAttributes is the validated attribute set.
	ResourceAttributes map[string]string
	ServiceName        string
}

// ConfigError reports every configuration problem at once. Fixing environment
// variables one redeploy at a time is miserable, so they are batched.
type ConfigError struct {
	Problems []string
}

func (e *ConfigError) Error() string {
	var b strings.Builder
	b.WriteString("invalid telemetry configuration:\n")
	for _, p := range e.Problems {
		b.WriteString("  - ")
		b.WriteString(p)
		b.WriteString("\n")
	}
	b.WriteString("see .env.example for the full set of required variables")
	return b.String()
}

func parseResourceAttributes(raw string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		eq := strings.Index(pair, "=")
		if eq < 1 {
			continue
		}
		out[strings.TrimSpace(pair[:eq])] = strings.TrimSpace(pair[eq+1:])
	}
	return out
}

// LoadConfig validates the environment at startup so a misconfigured
// service fails immediately and loudly, rather than running for a week and
// producing telemetry nobody can group by environment.
func LoadConfig() (*Config, error) {
	if os.Getenv("OTEL_SDK_DISABLED") == "true" {
		name := os.Getenv("OTEL_SERVICE_NAME")
		if name == "" {
			name = "unknown_service"
		}
		// Development needs none of the production variables.
		return &Config{Disabled: true, ServiceName: name}, nil
	}

	var problems []string

	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		problems = append(problems, "OTEL_EXPORTER_OTLP_ENDPOINT is required")
	}

	// The full resource attribute set, or the dashboards cannot slice by
	// environment, region or instance.
	attrs := parseResourceAttributes(os.Getenv("OTEL_RESOURCE_ATTRIBUTES"))
	if name := os.Getenv("OTEL_SERVICE_NAME"); name != "" {
		if _, ok := attrs["service.name"]; !ok {
			attrs["service.name"] = name
		}
	}
	// Known only at build time and per container, so not something to set in
	// Dokploy. Only filled in when absent, so an explicit value still wins.
	if sha := os.Getenv("GIT_SHA"); sha != "" && attrs["service.version"] == "" {
		attrs["service.version"] = sha
	}
	if host, err := os.Hostname(); err == nil && attrs["service.instance.id"] == "" {
		attrs["service.instance.id"] = host
	}
	for _, key := range RequiredResourceAttributes {
		if attrs[key] == "" {
			problems = append(problems, fmt.Sprintf("OTEL_RESOURCE_ATTRIBUTES is missing %q", key))
		}
	}

	// One strategy everywhere, so per-endpoint error rates on the
	// dashboard are comparable between services.
	ratioRaw := os.Getenv("OTEL_TRACES_SAMPLER_ARG")
	if ratioRaw == "" {
		ratioRaw = "1.0"
	}
	ratio, err := strconv.ParseFloat(ratioRaw, 64)
	if err != nil || ratio < 0 || ratio > 1 {
		problems = append(problems, fmt.Sprintf("OTEL_TRACES_SAMPLER_ARG must be a number in [0,1], got %q", ratioRaw))
	}

	if len(problems) > 0 {
		return nil, &ConfigError{Problems: problems}
	}

	return &Config{
		Disabled:           false,
		Endpoint:           endpoint,
		SamplingRatio:      ratio,
		ResourceAttributes: attrs,
		ServiceName:        attrs["service.name"],
	}, nil
}

// ApplySemconvStabilityOptIn sets OTEL_SEMCONV_STABILITY_OPT_IN if unset, and
// merges the required values into an existing setting rather than replacing it.
//
// This runs in init() because the contrib instrumentation packages read the
// variable when they initialise. Setting it later has no effect, which is a
// silent failure - hence also documenting that it belongs in the deployment
// environment.
func ApplySemconvStabilityOptIn() {
	existing := os.Getenv("OTEL_SEMCONV_STABILITY_OPT_IN")
	if existing == "" {
		_ = os.Setenv("OTEL_SEMCONV_STABILITY_OPT_IN", SemconvStabilityOptIn)
		return
	}
	have := map[string]bool{}
	var ordered []string
	for _, v := range strings.Split(existing, ",") {
		v = strings.TrimSpace(v)
		if v == "" || have[v] {
			continue
		}
		have[v] = true
		ordered = append(ordered, v)
	}
	for _, needed := range strings.Split(SemconvStabilityOptIn, ",") {
		if !have[needed] {
			have[needed] = true
			ordered = append(ordered, needed)
		}
	}
	_ = os.Setenv("OTEL_SEMCONV_STABILITY_OPT_IN", strings.Join(ordered, ","))
}

func init() {
	ApplySemconvStabilityOptIn()
}
