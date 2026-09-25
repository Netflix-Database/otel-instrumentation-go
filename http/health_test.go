package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func decode(t *testing.T, rec *httptest.ResponseRecorder) healthResponse {
	t.Helper()
	var body healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad json: %v (%s)", err, rec.Body.String())
	}
	return body
}

func readiness(t *testing.T, checks ...DependencyCheck) *httptest.ResponseRecorder {
	t.Helper()
	h, err := NewHealth(checks...)
	if err != nil {
		t.Fatalf("NewHealth: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ReadinessHandler().ServeHTTP(rec, httptest.NewRequest("GET", ReadinessPath, nil))
	return rec
}

// Liveness must never touch a dependency, and its body carries no checks key -
// byte-identical to the .NET and Node liveness handlers.
func TestLiveness_AlwaysOKWithoutChecks(t *testing.T) {
	rec := httptest.NewRecorder()
	LivenessHandler().ServeHTTP(rec, httptest.NewRequest("GET", LivenessPath, nil))

	if rec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", rec.Code)
	}
	if got := decode(t, rec); got.Status != "ok" {
		t.Errorf("status = %q, want ok", got.Status)
	}
	if strings.Contains(rec.Body.String(), "checks") {
		t.Errorf("liveness must not report checks: %s", rec.Body.String())
	}
}

func TestReadiness_OKWhenAllHealthy(t *testing.T) {
	rec := readiness(t,
		DependencyCheck{Name: "db", Check: func(context.Context) error { return nil }},
		DependencyCheck{Name: "redis", Check: func(context.Context) error { return nil }},
	)

	if rec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", rec.Code)
	}
	body := decode(t, rec)
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
	if body.Checks["db"].Status != "ok" || body.Checks["redis"].Status != "ok" {
		t.Errorf("per-dependency results missing: %+v", body.Checks)
	}
	// A passing check reports no error.
	if body.Checks["db"].Error != "" {
		t.Errorf("unexpected error on a passing check: %q", body.Checks["db"].Error)
	}
}

// A critical failure must return 503 so load balancers hold traffic back.
func TestReadiness_503OnCriticalFailure(t *testing.T) {
	rec := readiness(t,
		DependencyCheck{Name: "db", Check: func(context.Context) error { return errors.New("conn refused") }},
	)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", rec.Code)
	}
	body := decode(t, rec)
	if body.Status != "error" {
		t.Errorf("status = %q, want error", body.Status)
	}
	if body.Checks["db"].Status != "error" || !strings.Contains(body.Checks["db"].Error, "conn refused") {
		t.Errorf("failure not reported: %+v", body.Checks["db"])
	}
}

// Non-critical dependencies degrade the service rather than remove it.
func TestReadiness_NonCriticalStays200(t *testing.T) {
	rec := readiness(t,
		DependencyCheck{
			Name:        "search",
			Check:       func(context.Context) error { return errors.New("down") },
			NonCritical: true,
		},
	)

	if rec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", rec.Code)
	}
	body := decode(t, rec)
	if body.Status != "degraded" {
		t.Errorf("status = %q, want degraded", body.Status)
	}
	if body.Checks["search"].Status != "error" {
		t.Errorf("non-critical failure should still be reported: %+v", body.Checks["search"])
	}
}

// A critical failure wins over a non-critical one.
func TestReadiness_CriticalWinsOverDegraded(t *testing.T) {
	rec := readiness(t,
		DependencyCheck{Name: "search", Check: func(context.Context) error { return errors.New("down") }, NonCritical: true},
		DependencyCheck{Name: "db", Check: func(context.Context) error { return errors.New("gone") }},
	)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", rec.Code)
	}
	if got := decode(t, rec).Status; got != "error" {
		t.Errorf("status = %q, want error", got)
	}
}

// A check that ignores its context must not hang the probe past its timeout.
func TestReadiness_TimesOutOnHangingCheck(t *testing.T) {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- readiness(t, DependencyCheck{
			Name:    "slow",
			Timeout: 150 * time.Millisecond,
			Check:   func(context.Context) error { time.Sleep(5 * time.Second); return nil },
		})
	}()

	select {
	case rec := <-done:
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("code = %d, want 503", rec.Code)
		}
		if got := decode(t, rec).Checks["slow"]; got.Status != "error" {
			t.Errorf("timed-out check should be an error: %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("readiness probe hung past its timeout")
	}
}

// duration_ms is part of the shared response contract, and health-go does not
// report it - the context recorder has to.
func TestReadiness_ReportsPerCheckDuration(t *testing.T) {
	rec := readiness(t, DependencyCheck{
		Name:  "db",
		Check: func(context.Context) error { time.Sleep(20 * time.Millisecond); return nil },
	})

	if got := decode(t, rec).Checks["db"].DurationMS; got < 15 {
		t.Errorf("duration_ms = %d, want the measured time", got)
	}
}

func TestReadiness_NoStore(t *testing.T) {
	rec := readiness(t)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// Both endpoints mount at the shared paths.
func TestRegisterHealth(t *testing.T) {
	mux := http.NewServeMux()
	if err := RegisterHealth(mux); err != nil {
		t.Fatalf("RegisterHealth: %v", err)
	}

	for _, path := range []string{LivenessPath, ReadinessPath} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s returned %d, want 200", path, rec.Code)
		}
	}
}
