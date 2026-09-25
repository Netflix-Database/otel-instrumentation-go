package http

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	otelcfg "github.com/Netflix-Database/otel-instrumentation-go"
	"github.com/hellofresh/health-go/v5"
)

// Health paths, re-exported so callers need not import both packages.
const (
	LivenessPath  = otelcfg.LivenessPath
	ReadinessPath = otelcfg.ReadinessPath
)

// DefaultCheckTimeout bounds a single dependency probe, so one hung
// dependency cannot hold the whole readiness response open.
const DefaultCheckTimeout = 3 * time.Second

// DependencyCheck is a single readiness probe. Keep it cheap - SELECT 1, PING.
//
// Check is a plain func(context.Context) error, which is exactly
// health-go's CheckFunc, so the ready-made probes work unchanged:
//
//	otelhttp.DependencyCheck{Name: "db", Check: healthMySql.New(healthMySql.Config{DSN: dsn})}
type DependencyCheck struct {
	Name string
	// Check reports an error when the dependency is unhealthy.
	Check func(context.Context) error
	// NonCritical checks are reported but do not make the service unready.
	// Use for dependencies the service degrades without rather than dies
	// without. Maps to health-go's SkipOnErr.
	NonCritical bool
	// Timeout for this check. Defaults to DefaultCheckTimeout.
	Timeout time.Duration
}

// durationRecorder collects per-check durations for one measurement.
//
// health-go reports which checks failed but not how long each took, and
// duration_ms is part of the response contract shared with the .NET and Node
// services. The recorder is passed through the context rather than held on the
// handler, because Measure runs checks concurrently and several requests can
// be in flight at once.
type durationRecorder struct {
	mu sync.Mutex
	d  map[string]time.Duration
}

type durationRecorderKey struct{}

func (r *durationRecorder) record(name string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.d[name] = d
}

func (r *durationRecorder) get(name string) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.d[name]
}

type checkResult struct {
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

type healthResponse struct {
	Status string                 `json:"status"`
	Checks map[string]checkResult `json:"checks,omitempty"`
}

// Health serves the two health endpoints on top of health-go.
type Health struct {
	h     *health.Health
	names []string
}

// NewHealth builds a health container from the given checks.
//
// Orchestration - concurrency, per-check timeouts, the critical/non-critical
// split - is health-go's. This package only fixes the two paths and the
// response shape, so one probe config and one dashboard panel work across
// every language.
func NewHealth(checks ...DependencyCheck) (*Health, error) {
	h, err := health.New()
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(checks))
	for _, c := range checks {
		timeout := c.Timeout
		if timeout <= 0 {
			timeout = DefaultCheckTimeout
		}

		name, probe := c.Name, c.Check
		cfg := health.Config{
			Name:      name,
			Timeout:   timeout,
			SkipOnErr: c.NonCritical,
			Check: func(ctx context.Context) error {
				start := time.Now()
				err := probe(ctx)
				if rec, ok := ctx.Value(durationRecorderKey{}).(*durationRecorder); ok {
					rec.record(name, time.Since(start))
				}
				return err
			},
		}

		if err := h.Register(cfg); err != nil {
			return nil, err
		}
		names = append(names, name)
	}

	return &Health{h: h, names: names}, nil
}

// LivenessHandler answers whether the process is alive. It deliberately runs
// no checks: a liveness probe that touches the database restarts the app
// whenever the database blips, turning a dependency outage into an outage plus
// a restart loop.
func LivenessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeHealth(w, http.StatusOK, healthResponse{Status: "ok"})
	})
}

// ReadinessHandler answers whether the service can serve traffic. Returns 503
// when a critical dependency fails, so load balancers and Docker's
// service_healthy condition hold traffic back.
func (x *Health) ReadinessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &durationRecorder{d: make(map[string]time.Duration, len(x.names))}
		ctx := context.WithValue(r.Context(), durationRecorderKey{}, rec)

		res := x.h.Measure(ctx)

		checks := make(map[string]checkResult, len(x.names))
		for _, name := range x.names {
			entry := checkResult{
				Status:     "ok",
				DurationMS: rec.get(name).Milliseconds(),
			}
			// Failures carries only the checks that failed, keyed by name.
			// The message can embed a DSN with credentials, but it is
			// health-go's own text and already only the error string - never
			// a wrapped chain.
			if msg, failed := res.Failures[name]; failed {
				entry.Status = "error"
				entry.Error = msg
			}
			checks[name] = entry
		}

		code := http.StatusOK
		if res.Status == health.StatusUnavailable {
			code = http.StatusServiceUnavailable
		}

		writeHealth(w, code, healthResponse{Status: overallStatus(res.Status), Checks: checks})
	})
}

// RegisterHealth mounts both endpoints on a mux at the shared paths.
func RegisterHealth(mux *http.ServeMux, checks ...DependencyCheck) error {
	h, err := NewHealth(checks...)
	if err != nil {
		return err
	}

	mux.Handle(otelcfg.LivenessPath, LivenessHandler())
	mux.Handle(otelcfg.ReadinessPath, h.ReadinessHandler())
	return nil
}

// overallStatus maps health-go's status onto the wording the .NET and Node
// services emit.
func overallStatus(s health.Status) string {
	switch s {
	case health.StatusOK:
		return "ok"
	case health.StatusPartiallyAvailable:
		return "degraded"
	default:
		return "error"
	}
}

func writeHealth(w http.ResponseWriter, code int, body healthResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
