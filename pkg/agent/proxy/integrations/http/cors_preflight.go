package http

import (
	"net/http"

	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A browser asks a server whether it may make a cross-origin call before it
// makes it: a CORS preflight (Fetch standard, "CORS-preflight request"): an
// OPTIONS carrying Origin and Access-Control-Request-Method. It keeps the
// server's yes for as long as the answer's Access-Control-Max-Age says, five
// seconds in Chromium when the server sets none, so how many preflights a
// test sends follows how long the run took, not what the test does: a slower
// replay preflights a call again where its record did not, a faster one skips
// one the record made.
//
// So under the proven switch a preflight the server allowed (a 2xx answer) is
// recorded as a phase of the call it guards, as a handshake is: tagged config
// and stated session lifetime, it is protocol-phase (models.Mock.
// IsProtocolPhase). A replay serves it to any test, any number of times,
// through the reusable door; no test owns it, so none must make it again, and
// a preflight made more often than recorded is no extra call. It answers only
// a preflight it matches, as any recording does (the guarded call's path,
// method and headers are the preflight's). The guarded call itself stays the
// test's data, consumed once and required.
//
// A preflight the server refused is no phase of a call: the browser never
// made the call, so the refusal is what the test got from the dependency, and
// it stays the test's data. Without the switch every exchange is recorded as
// before.

// isCORSPreflight reports whether req is a CORS preflight: an OPTIONS with
// Origin and Access-Control-Request-Method. An OPTIONS an app sends itself
// carries no Access-Control-Request-Method.
func isCORSPreflight(req *http.Request) bool {
	return req.Method == http.MethodOptions &&
		req.Header.Get("Origin") != "" && req.Header.Get("Access-Control-Request-Method") != ""
}

// corsPreflightPhase reports whether an exchange of req answered with status
// is recorded as a phase of the call it guards: a CORS preflight the server
// allowed, under the proven switch.
func corsPreflightPhase(req *http.Request, status int) bool {
	return provenswitch.On() && isCORSPreflight(req) && status/100 == 2
}
