package otel

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// ShutdownOptions configures graceful termination.
type ShutdownOptions struct {
	// Server is stopped first, so it finishes in-flight requests and stops
	// accepting new ones.
	Server *http.Server
	// CloseDependencies closes db, redis and rabbit connections. Runs after the
	// server has drained, so in-flight requests still have their pools.
	CloseDependencies func(context.Context) error
	// Shutdown is the function returned by SetupOTelSDK. Called last so spans
	// produced while closing dependencies are still exported.
	Shutdown func(context.Context) error
	// Timeout bounds the whole sequence. Kubernetes and Docker send SIGKILL
	// after their own grace period, so this must be shorter than that or the
	// flush never completes.
	Timeout time.Duration
}

// WaitForShutdown blocks until SIGTERM or SIGINT, then drains in this order:
//
//  1. stop accepting requests and finish in-flight ones
//  2. close db/redis/rabbit connections
//  3. flush the OTel exporters
//
// The order matters. Exporters are flushed last because draining and closing
// pools both produce spans, and anything recorded after the flush is lost -
// which is precisely the telemetry you want when a deploy goes wrong.
func WaitForShutdown(opts ShutdownOptions) error {
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	<-sigCtx.Done()
	return Drain(opts)
}

// Drain runs the shutdown sequence without waiting for a signal, for services
// that manage their own lifecycle.
func Drain(opts ShutdownOptions) error {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var errs error

	if opts.Server != nil {
		if err := opts.Server.Shutdown(ctx); err != nil {
			errs = errors.Join(errs, err)
		}
	}

	if opts.CloseDependencies != nil {
		if err := opts.CloseDependencies(ctx); err != nil {
			errs = errors.Join(errs, err)
		}
	}

	if opts.Shutdown != nil {
		if err := opts.Shutdown(ctx); err != nil {
			errs = errors.Join(errs, err)
		}
	}

	return errs
}

// NotifyShutdown returns a context cancelled on SIGTERM/SIGINT, plus its stop
// function, for services that want to drive shutdown themselves.
func NotifyShutdown() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
}

var _ = os.Interrupt
