package sentry

import (
	"net/http"

	sentrygo "github.com/getsentry/sentry-go"
)

// headerAllowlist is the only request header content that may leave the
// service. Everything else (Authorization, Cookie, Set-Cookie, X-API-Key,
// Referer — it echoes full URLs including query strings — and arbitrary
// custom headers carrying tokens) is removed by scrubEvent.
var headerAllowlist = map[string]struct{}{
	"Accept":          {},
	"Accept-Encoding": {},
	"Content-Length":  {},
	"Content-Type":    {},
	"User-Agent":      {},
}

// scrubEvent is the client-level BeforeSend hook. It enforces the privacy
// contract on every event regardless of which code path attached data:
// headers are filtered through an allowlist, cookies, query string and body
// are removed, the user object is zeroed unconditionally, and scope-set
// Tags/Contexts are dropped (they may carry PII; services must not treat
// them as a transport for sensitive values — sentry-go v0.49.0 has no Event
// Extra field at all). Only the SDK-managed "trace" context survives,
// preserving trace correlation.
func scrubEvent(event *sentrygo.Event, _ *sentrygo.EventHint) *sentrygo.Event {
	if event == nil {
		return nil
	}
	event.User = sentrygo.User{}
	event.Tags = nil
	for k := range event.Contexts {
		if k != "trace" {
			delete(event.Contexts, k)
		}
	}
	if event.Request != nil {
		event.Request = scrubbedSentryRequest(event.Request)
	}
	return event
}

// scrubbedSentryRequest strips a Sentry request object down to non-sensitive
// routing context: method, URL without query/fragment, and allowlisted
// headers. Cookies, body data and every other header are dropped.
func scrubbedSentryRequest(req *sentrygo.Request) *sentrygo.Request {
	if req == nil {
		return nil
	}
	clean := &sentrygo.Request{
		Method: req.Method,
		URL:    req.URL,
	}
	if clean.URL != "" {
		clean.URL = stripQuery(clean.URL)
	}
	var headers map[string]string
	for k, v := range req.Headers {
		if _, ok := headerAllowlist[k]; ok {
			if headers == nil {
				headers = make(map[string]string, 1)
			}
			headers[k] = v
		}
	}
	clean.Headers = headers
	// Cookies, Data (body), QueryString and Env are never copied.
	return clean
}

// attachScrubbedRequest installs a scope event processor that reports the
// request context as method + path only. The caller's real *http.Request
// (headers, body, query, cookies) is never handed to the SDK.
func attachScrubbedRequest(scope *sentrygo.Scope, r *http.Request) {
	req := &sentrygo.Request{Method: r.Method}
	if r.URL != nil && r.URL.Path != "" {
		req.URL = "http://" + r.URL.Path
	}
	scope.AddEventProcessor(func(event *sentrygo.Event, _ *sentrygo.EventHint) *sentrygo.Event {
		if event == nil {
			return nil
		}
		event.Request = req
		return event
	})
}

// stripQuery removes the query string and fragment from a URL string. The
// query can carry tokens and personal data (reset links, OAuth codes), so it
// is treated as sensitive and never reported.
func stripQuery(u string) string {
	for i := 0; i < len(u); i++ {
		switch u[i] {
		case '?', '#':
			return u[:i]
		}
	}
	return u
}

// dropSensitiveBreadcrumb removes breadcrumbs that carry HTTP request data:
// auto-captured transport breadcrumbs (category "http") may contain URLs
// with query strings and sensitive header data. Everything else passes.
func dropSensitiveBreadcrumb(bc *sentrygo.Breadcrumb, _ *sentrygo.BreadcrumbHint) *sentrygo.Breadcrumb {
	if bc == nil {
		return nil
	}
	switch bc.Category {
	case "http", "request", "response", "query":
		return nil
	default:
		return bc
	}
}
