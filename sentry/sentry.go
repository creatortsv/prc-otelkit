// Package sentry provides Sentry error-capture integration for Position
// Review Copilot services: an explicit, scrubbing-first SDK bootstrap plus
// one-line capture helpers for service-level panic recoverers.
//
// Privacy contract (prc-docs/operations/observability.md, VENA-154):
//
//   - Automatic data collection is disabled entirely (cookies, HTTP
//     headers, bodies, query params, user info) via ClientOptions
//     .DataCollection. `send_default_pii` must never be enabled by any
//     service; this package never exposes a way to enable it.
//   - Request context reported to Sentry is built from the HTTP method and
//     the request path ONLY — no query string, no body, no headers, no
//     cookies. BeforeSend additionally scrubs any request data attached by
//     other code paths, so sensitive values cannot leak through the scope.
//   - Sensitive HTTP breadcrumbs (auto-captured request/response breadcrumbs
//     carrying URLs or sensitive header data) are dropped before send.
//   - Panic values captured via CapturePanic are reported verbatim (wrapped
//     as "panic: %v"). Never panic with values containing credentials or
//     PII — the recoverer cannot scrub what it cannot interpret.
//   - An empty DSN means disabled: every helper is a no-op, so services can
//     keep the same wiring in every environment (local keeps SENTRY_DSN
//     unset/empty; stage and prod receive the real DSN from a cluster
//     Secret, never from git).
package sentry

import (
	"context"
	"fmt"
	"net/http"
	"strings"
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
// disabled pattern used by the local environment. On init failure it also
// returns a no-op shutdown, so a caller that defers the shutdown regardless
// of the error never panics. DSN parse failures are reported without the
// DSN material itself (host and project id only).
func Init(cfg Config) (enabled bool, shutdown func(context.Context), err error) {
	if strings.TrimSpace(cfg.DSN) == "" {
		// Defense-in-depth: unbind any client a previous Init (or another
		// library) left on the global hub, so nothing can fire events in
		// the disabled mode.
		sentrygo.CurrentHub().BindClient(nil)
		return false, func(context.Context) {}, nil
	}
	timeout := cfg.FlushTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	noOp := func(context.Context) {}
	if err := sentrygo.Init(sentrygo.ClientOptions{
		Dsn:              cfg.DSN,
		Environment:      cfg.Environment,
		Release:          cfg.Release,
		ServerName:       cfg.ServiceName,
		AttachStacktrace: true,
		// Hard privacy guarantee: no automatic data collection at all. This
		// is intentionally stricter than the deprecated SendDefaultPII=false
		// and is not configurable through this package — only scrubbed,
		// explicit capture (method+path) contributes request data.
		DataCollection: &sentrygo.DataCollection{
			UserInfo: sentrygo.Set(false),
			Cookies:  &sentrygo.KeyValueCollectionBehavior{Mode: sentrygo.CollectionOff},
			HTTPHeaders: &sentrygo.HeaderCollectionConfig{
				Request:  &sentrygo.KeyValueCollectionBehavior{Mode: sentrygo.CollectionOff},
				Response: &sentrygo.KeyValueCollectionBehavior{Mode: sentrygo.CollectionOff},
			},
			HTTPBodies:  []sentrygo.BodyType{},
			QueryParams: &sentrygo.KeyValueCollectionBehavior{Mode: sentrygo.CollectionOff},
		},
		BeforeSend:       scrubEvent,
		BeforeBreadcrumb: dropSensitiveBreadcrumb,
	}); err != nil {
		// The SDK's DSN parse error embeds the raw DSN (public key AND
		// secret). Never surface that material: report host and project id
		// only.
		return false, noOp, fmt.Errorf("sentry: init: %w", sanitizeDSNError(cfg.DSN, err))
	}
	shutdown = func(ctx context.Context) {
		sentrygo.CurrentHub().Flush(timeout)
	}
	return true, shutdown, nil
}

// sanitizeDSNError replaces a DSN parse error with a message that carries no
// DSN material: the SDK's error embeds the raw DSN (public key and secret),
// so it is never surfaced — not even wrapped. Only host and project id are
// reported; if the DSN is too malformed to split those out, it reports
// nothing but the failure itself.
func sanitizeDSNError(dsn string, _ error) error {
	// Expected shape: scheme://publicKey[:secret]@host/projectId
	rest, _, ok := strings.Cut(dsn, "://")
	if !ok {
		return fmt.Errorf("invalid DSN")
	}
	rest, _, ok = strings.Cut(rest, "@") // drop public key and secret
	if !ok {
		return fmt.Errorf("invalid DSN")
	}
	host, projectID, ok := strings.Cut(rest, "/")
	if !ok || host == "" || projectID == "" {
		return fmt.Errorf("invalid DSN")
	}
	return fmt.Errorf("invalid DSN for host %q, project %q", host, projectID)
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
