// Package httpkit holds HTTP instrumentation primitives shared by the
// prc-otelkit middleware packages (metrics, tracing) so their bounded-value
// contracts — method labels, span-name segments — cannot drift apart.
package httpkit

import "net/http"

// allowedMethods is the fixed set of standard HTTP method tokens passed
// through verbatim. net/http accepts arbitrary method tokens on the wire, so
// anything outside this set — attacker-controlled or exotic — is collapsed
// into OtherMethod to keep metric-label and span-name cardinality bounded.
var allowedMethods = map[string]struct{}{
	http.MethodConnect: {},
	http.MethodDelete:  {},
	http.MethodGet:     {},
	http.MethodHead:    {},
	http.MethodOptions: {},
	http.MethodPatch:   {},
	http.MethodPost:    {},
	http.MethodPut:     {},
	http.MethodTrace:   {},
}

// OtherMethod is the bounded value every non-standard method token
// normalizes to.
const OtherMethod = "other"

// NormalizeMethod maps a raw method token onto its bounded label value:
// standard tokens verbatim, everything else OtherMethod. Matching is
// case-sensitive per the HTTP spec — lowercase "get" is not the standard
// token and normalizes too.
func NormalizeMethod(method string) string {
	if _, ok := allowedMethods[method]; ok {
		return method
	}
	return OtherMethod
}
