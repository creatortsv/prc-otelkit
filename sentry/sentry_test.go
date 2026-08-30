package sentry

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sentrygo "github.com/getsentry/sentry-go"
)

// envelopeServer is a minimal fake Sentry ingestion endpoint: it accepts
// POSTs of envelope payloads (DSN store path /api/42/envelope/) and records
// the serialized body.
type envelopeServer struct {
	srv  *httptest.Server
	dsn  string
	body chan string
}

func newEnvelopeServer(t *testing.T) *envelopeServer {
	t.Helper()
	e := &envelopeServer{body: make(chan string, 8)}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read envelope: %v", err)
		}
		select {
		case e.body <- string(b):
		default:
			t.Error("unexpected extra envelope")
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(e.srv.Close)
	// The DSN carries a secret component on purpose: the canary below is
	// only meaningful if the test DSN contains secret material that the
	// envelope must never carry.
	e.dsn = fmt.Sprintf("http://pubkey:%s@%s/42", dsnSecret, strings.TrimPrefix(e.srv.URL, "http://"))
	return e
}

// dsnSecret is the canary baked into the test DSN.
const dsnSecret = "canary-dsn-secret-7f3a"

func (e *envelopeServer) nextBody(t *testing.T) string {
	t.Helper()
	select {
	case b := <-e.body:
		return b
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for envelope")
		return ""
	}
}

// mustInit boots the SDK against the fake endpoint and returns a shutdown
// func that flushes buffered events before the server is torn down.
func mustInit(t *testing.T, e *envelopeServer) func() {
	t.Helper()
	enabled, shutdown, err := Init(Config{
		DSN:         e.dsn,
		Environment: "test",
		Release:     "prc-auth:1.2.3",
		ServiceName: "prc-sentry-test",
	})
	if err != nil || !enabled {
		t.Fatalf("Init: enabled=%v err=%v", enabled, err)
	}
	return func() { shutdown(context.Background()) }
}

func TestDisabledByEmptyDSN(t *testing.T) {
	enabled, shutdown, err := Init(Config{})
	if enabled || err != nil {
		t.Fatalf("empty DSN must disable: enabled=%v err=%v", enabled, err)
	}
	defer shutdown(context.Background())
	// Helpers must be no-op safe with no client installed.
	sentrygo.CurrentHub().BindClient(nil)
	defer sentrygo.CurrentHub().BindClient(nil)
	CapturePanic(nil, nil)
	CaptureError(nil, nil)
}

func TestCapturePanicEnvelopeScrubbed(t *testing.T) {
	e := newEnvelopeServer(t)
	shutdown := mustInit(t, e)
	defer shutdown()

	// A handler that panics through a recoverer.
	handler := func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				CapturePanic(rec, r)
			}
		}()
		panic("boom")
	}

	req := httptest.NewRequest(http.MethodPost, "/reset-password?token=super-secret-token&email=user%40example.com", nil)
	req.Header.Set("Authorization", "Bearer top-secret-bearer")
	req.Header.Set("Cookie", "session=secret-cookie-value")
	req.Header.Set("X-API-Key", "key-should-not-leak")
	req.Header.Set("User-Agent", "prc-test-agent")
	handler(httptest.NewRecorder(), req)

	body := e.nextBody(t)

	mustContain := []string{
		`"environment":"test"`,
		`"release":"prc-auth:1.2.3"`,
		"/reset-password", // method+path context survives
		`"method":"POST"`,
	}
	for _, want := range mustContain {
		if !strings.Contains(body, want) {
			t.Errorf("envelope missing %q\nbody: %s", want, body)
		}
	}

	never := []string{
		"super-secret-token",  // query string dropped
		"token=",              // no query keys either
		"top-secret-bearer",   // Authorization dropped
		"Authorization",       // header not even named
		"secret-cookie-value", // cookies dropped
		"session=",            // no cookie content
		"key-should-not-leak", // custom header dropped
		"X-API-Key",
		"ip_address", // no user IP
		"query_string",
	}
	for _, bad := range never {
		if strings.Contains(body, bad) {
			t.Errorf("envelope leaks %q\nbody: %s", bad, body)
		}
	}

	// Envelope header carries the DSN per protocol (auth material for the
	// ingestion endpoint, TLS-protected) — with a secret-bearing test DSN it
	// is expected there. The REAL scrubbing boundary is the event item
	// payload: it must never embed the DSN or its secret material.
	header := body[:strings.IndexByte(body, '\n')]
	if !strings.Contains(header, "public_key\":\"pubkey") {
		t.Errorf("envelope header does not authenticate the test DSN: %s", header)
	}
	eventItem := body[strings.LastIndexByte(body, '\n')+1:]
	if strings.Contains(eventItem, dsnSecret) {
		t.Errorf("event payload carries the DSN secret component: %s", eventItem)
	}
	if strings.Contains(eventItem, `"dsn"`) {
		t.Errorf("event item embeds DSN material: %s", eventItem)
	}
}

// TestScrubEventHeaderAllowlist pins the client-level defense in depth: even
// request data attached by code paths outside this package is stripped to
// the allowlist before send.
func TestScrubEventHeaderAllowlist(t *testing.T) {
	event := &sentrygo.Event{
		Request: &sentrygo.Request{
			Method:      http.MethodGet,
			URL:         "http://svc/reset?one-time-code=leak-me",
			QueryString: "one-time-code=leak-me",
			Cookies:     "session=leak-me",
			Data:        "body-should-go",
			Env:         map[string]string{"REMOTE_ADDR": "10.0.0.1"},
			Headers: map[string]string{
				"Authorization": "Bearer leak-me",
				"Cookie":        "session=leak-me",
				"X-API-Key":     "leak-me",
				"Content-Type":  "application/json",
				"User-Agent":    "keep-me",
				"Referer":       "https://evil.example/reset?one-time-code=leak-me",
			},
		},
		User: sentrygo.User{IPAddress: "10.0.0.1"},
	}
	clean := scrubEvent(event, nil)
	if clean == nil {
		t.Fatal("scrubEvent dropped a valid event")
	}
	if clean.User.IPAddress != "" {
		t.Error("user IP not cleared")
	}
	req := clean.Request
	if req.QueryString != "" || req.Cookies != "" || req.Data != "" || req.Env != nil {
		t.Errorf("query/cookies/body/env survived: %+v", req)
	}
	for k := range req.Headers {
		if _, ok := headerAllowlist[k]; !ok {
			t.Errorf("non-allowlisted header survived: %s", k)
		}
	}
	if req.Headers["User-Agent"] != "keep-me" {
		t.Error("allowlisted User-Agent dropped")
	}
	if req.URL != "http://svc/reset" {
		t.Errorf("query not stripped from URL: %s", req.URL)
	}
}

// TestScrubEventDropsReferer pins Referer removal: its value echoes full
// URLs and can smuggle query strings carrying tokens or personal data.
func TestScrubEventDropsReferer(t *testing.T) {
	event := &sentrygo.Event{
		Request: &sentrygo.Request{
			Method:  http.MethodGet,
			URL:     "http://svc/page",
			Headers: map[string]string{"Referer": "https://mail.example/inbox?token=referer-secret"},
		},
	}
	clean := scrubEvent(event, nil)
	if clean.Request.Headers["Referer"] != "" {
		t.Errorf("Referer survived scrubbing: %q", clean.Request.Headers["Referer"])
	}
	if strings.Contains(clean.Request.URL, "referer-secret") {
		t.Error("referer secret leaked through URL")
	}
}

// TestScrubEventZeroesUserWithoutIP: a scope-set user without an IP address
// must be zeroed too — email/username/id are PII regardless of IP presence.
func TestScrubEventZeroesUserWithoutIP(t *testing.T) {
	event := &sentrygo.Event{
		User: sentrygo.User{Email: "user@example.com", Username: "pii-user", ID: "42"},
	}
	clean := scrubEvent(event, nil)
	if clean.User.ID != "" || clean.User.Email != "" || clean.User.Username != "" || clean.User.IPAddress != "" {
		t.Errorf("IP-less user survived: %+v", clean.User)
	}
}

// TestScrubEventDropsScopeData: scope-set Tags and Contexts never reach
// Sentry (they may carry PII); only the SDK-managed trace context survives
// for correlation. sentry-go v0.49.0 has no Event Extra field.
func TestScrubEventDropsScopeData(t *testing.T) {
	event := &sentrygo.Event{
		Tags: map[string]string{"tenant": "acme-corp"},
		Contexts: map[string]sentrygo.Context{
			"trace":          {"trace_id": "abc"},
			"scope-injected": {"email": "user@example.com"},
		},
	}
	clean := scrubEvent(event, nil)
	if clean.Tags != nil {
		t.Errorf("tags survived: %v", clean.Tags)
	}
	if _, ok := clean.Contexts["trace"]; !ok {
		t.Error("SDK-managed trace context must survive")
	}
	for k := range clean.Contexts {
		if k != "trace" {
			t.Errorf("scope-set context survived: %s", k)
		}
	}
}

// TestInitErrorReturnsNoOpShutdown: on a malformed DSN the returned shutdown
// must be callable (the documented defer pattern), and the error must never
// carry DSN secret material.
func TestInitErrorReturnsNoOpShutdown(t *testing.T) {
	const secretDSN = "https://pubkey:leaky-secret@bad host path/42"
	enabled, shutdown, err := Init(Config{DSN: secretDSN})
	if enabled || err == nil {
		t.Fatalf("malformed DSN must fail init: enabled=%v err=%v", enabled, err)
	}
	shutdown(context.Background()) // must not panic
	if strings.Contains(err.Error(), "leaky-secret") || strings.Contains(err.Error(), "pubkey") {
		t.Errorf("init error embeds DSN material: %v", err)
	}
}

func TestSensitiveBreadcrumbsDropped(t *testing.T) {
	e := newEnvelopeServer(t)
	shutdown := mustInit(t, e)
	defer shutdown()

	sentrygo.CurrentHub().AddBreadcrumb(&sentrygo.Breadcrumb{
		Category: "http",
		Message:  "GET http://internal/db?password=bread-secret",
	}, nil)
	sentrygo.CurrentHub().AddBreadcrumb(&sentrygo.Breadcrumb{
		Category: "app",
		Message:  "digest scheduled",
	}, nil)

	CaptureError(fmt.Errorf("with-breadcrumbs"), httptest.NewRequest(http.MethodGet, "/jobs", nil))
	body := e.nextBody(t)

	if strings.Contains(body, "bread-secret") || strings.Contains(body, "internal/db") {
		t.Errorf("http breadcrumb leaked\nbody: %s", body)
	}
	if !strings.Contains(body, "digest scheduled") {
		t.Errorf("app breadcrumb dropped unexpectedly\nbody: %s", body)
	}
}

func TestCaptureErrorNoopOnNil(t *testing.T) {
	CaptureError(nil, nil) // must not panic with SDK enabled or disabled
}
