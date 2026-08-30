// Package sentry provides Sentry error-capture integration for Position
// Review Copilot services: an explicit, scrubbing-first SDK bootstrap plus
// one-line capture helpers for service-level panic recoverers.
//
// Privacy contract (prc-docs/operations/observability.md, VENA-154):
//
//   - SendDefaultPII is always false and must never be set to true by any
//     service. This package never exposes a way to enable it.
//   - Request context reported to Sentry is built from the HTTP method and
//     the request path ONLY — no query string, no body, no headers, no
//     cookies. BeforeSend additionally scrubs any request data attached by
//     other code paths, so sensitive values cannot leak through the scope.
//   - Sensitive HTTP breadcrumbs (auto-captured request/response breadcrumbs
//     carrying URLs or sensitive header data) are dropped before send.
//   - An empty DSN means disabled: every helper is a no-op, so services can
//     keep the same wiring in every environment (local keeps SENTRY_DSN
//     unset/empty; stage and prod receive the real DSN from a cluster
//     Secret, never from git).
package sentry

import (
	"context"
	"fmt"
	"net/http"
	"time"

	sentrygo "github.com/getsentry/sentry-go"
)

// Config controls SDK bootstrap. All fields except DSN are optional but
// strongly recommended (Environment and Release drive Sentry's release
// health; Release should carry the deployed image tag).
type Config struct {
	// DSN from the per-environment cluster Secret. Empty means disabled.
	DSN string
	// Environment, e.g. "local", "stage", "prod".
	Environment string
	// Release identifier; convention is the deployed image tag
	// (e.g. "prc-auth:1.4.2").
	Release string
	// ServiceName becomes the Sentry server_name attribute.
	ServiceName string
	// FlushTimeout bounds the shutdown flush. Defaults to 5s.
	FlushTimeout time.Duration
}

// Init bootstraps the global hub. It returns whether the SDK is enabled and
// a shutdown func that flushes buffered events (safe to defer in main).
// With an empty DSN it returns enabled=false and a no-op shutdown — the
// disabled pattern used by the local environment.
func Init(cfg Config) (enabled bool, shutdown func(context.Context), err error) {
	if cfg.DSN == "" {
		return false, func(context.Context) {}, nil
	}
	timeout := cfg.FlushTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if err := sentrygo.Init(sentrygo.ClientOptions{
		Dsn:              cfg.DSN,
		Environment:      cfg.Environment,
		Release:          cfg.Release,
		ServerName:       cfg.ServiceName,
		AttachStacktrace: true,
		// Hard privacy guarantee: never send PII by default. This flag is
		// intentionally not configurable through this package.
		SendDefaultPII:   false,
		BeforeSend:       scrubEvent,
		BeforeBreadcrumb: dropSensitiveBreadcrumb,
	}); err != nil {
		return false, nil, fmt.Errorf("sentry: init: %w", err)
	}
	shutdown = func(ctx context.Context) {
		sentrygo.CurrentHub().Flush(timeout)
	}
	return true, shutdown, nil
}

// CapturePanic reports a recovered panic value from a service-level recoverer
// middleware. It is a no-op when the SDK is disabled. The request contributes
// method and path only — sensitive request data never reaches the event.
func CapturePanic(rec any, r *http.Request) {
	hub := sentrygo.CurrentHub()
	if hub.Client() == nil {
		return
	}
	hub.WithScope(func(scope *sentrygo.Scope) {
		if r != nil {
			attachScrubbedRequest(scope, r)
		}
		hub.CaptureException(fmt.Errorf("panic: %v", rec))
	})
}

// CaptureError reports a non-nil error. It is a no-op when the SDK is
// disabled or err is nil.
func CaptureError(err error, r *http.Request) {
	if err == nil {
		return
	}
	hub := sentrygo.CurrentHub()
	if hub.Client() == nil {
		return
	}
	hub.WithScope(func(scope *sentrygo.Scope) {
		if r != nil {
			attachScrubbedRequest(scope, r)
		}
		hub.CaptureException(err)
	})
}
