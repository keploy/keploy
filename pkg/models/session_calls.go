package models

import (
	"net"
	"net/url"
	"strings"
	"sync/atomic"
)

// SessionCallRule declares an HTTP or HTTP/2 call that every test may be
// served from any test's recording of it: a call several tests need though
// only one recorded it (a token the app fetches once and caches, a shared
// configuration read). A recording whose recorder stated its HTTP calls
// per-test (MetadataLifetimeKey) serves each test its own calls only, and
// such a call misses in every test but the one that made it at record time;
// declaring it in keploy.yaml (sessionCalls) serves it to every test,
// without recording again.
//
// Under the proven switch (KEPLOY_PROVEN) a test is served, of the calls a
// rule declares, only a recording made outside every test (in TestMain or at
// start-up), and only of a call the rule names exactly: Method set and
// PathPrefix the call's whole path (names). Another test's recording of it
// stays that test's.
//
// Host is required; Method and PathPrefix narrow the rule when set.
type SessionCallRule struct {
	// Host is the upstream's host, compared case-insensitively. Without a port
	// it matches the host on any port; with one, only that port.
	Host string `json:"host" yaml:"host" mapstructure:"host"`
	// Method, when set, restricts the rule to one HTTP method
	// (case-insensitive).
	Method string `json:"method,omitempty" yaml:"method,omitempty" mapstructure:"method"`
	// PathPrefix, when set, restricts the rule to the paths it begins, on
	// segment boundaries: "/oauth" matches "/oauth" and "/oauth/token", not
	// "/oauthx". A PathPrefix that ends with "/" matches anything under it.
	PathPrefix string `json:"pathPrefix,omitempty" yaml:"pathPrefix,omitempty" mapstructure:"pathPrefix"`
}

// Problem says why the rule can match no call, or "" when it can: it names no
// host, or names a URL or a path where the host belongs. ValidateFlags warns
// of it, as a rule that matches nothing is otherwise silent.
func (r SessionCallRule) Problem() string {
	switch {
	case strings.TrimSpace(r.Host) == "":
		return "it names no host"
	case strings.Contains(r.Host, "://") || strings.ContainsAny(r.Host, "/?#"):
		return "its host is a URL or a path; name the host, and a port if it has one, as the request's Host header does"
	}
	return ""
}

// matches reports whether the rule covers a call of method to host and path.
func (r SessionCallRule) matches(method, host, path string) bool {
	if r.Host == "" {
		return false
	}
	if r.Method != "" && !strings.EqualFold(r.Method, method) {
		return false
	}
	if !sessionCallHostMatches(r.Host, host) {
		return false
	}
	if r.PathPrefix == "" {
		return true
	}
	if strings.HasSuffix(r.PathPrefix, "/") {
		return strings.HasPrefix(path, r.PathPrefix)
	}
	return path == r.PathPrefix || strings.HasPrefix(path, r.PathPrefix+"/")
}

// names reports whether the rule names a call of method to host and path
// exactly: it states the method, and its PathPrefix is the call's whole path,
// on a host it matches. Under the proven switch only such a rule serves a
// test a recording made outside every test (Mock.DeclaredSessionCall).
func (r SessionCallRule) names(method, host, path string) bool {
	return r.Method != "" && r.PathPrefix == path && r.matches(method, host, path)
}

func sessionCallHostMatches(rule, host string) bool {
	if strings.EqualFold(rule, host) {
		return true
	}
	if _, _, err := net.SplitHostPort(rule); err == nil {
		// The rule names a port: only that host and port match.
		return false
	}
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	return strings.EqualFold(rule, h)
}

var sessionCalls atomic.Pointer[[]SessionCallRule]

// sessionCallsOn is whether the rules SetSessionCalls installed last are any.
var sessionCallsOn atomic.Bool

// SetSessionCalls installs the process's declared session calls (keploy.yaml
// sessionCalls), which DeriveLifetime applies to every HTTP and HTTP/2 mock
// it derives per-test from then on. The keploy CLI installs them from its
// configuration before it reads a mock, and hands the agent each mock with
// the lifetime it derived.
func SetSessionCalls(rules []SessionCallRule) {
	cp := append([]SessionCallRule(nil), rules...)
	sessionCalls.Store(&cp)
	sessionCallsOn.Store(len(cp) > 0)
	mockRulesChanged()
}

// SessionCallsDeclared reports whether the process declares any session call
// (SetSessionCalls).
func SessionCallsDeclared() bool { return sessionCallsOn.Load() }

// DeclaredSessionCall reports whether m is an HTTP or HTTP/2 call its
// recorder stated per-test that a declared session call (SetSessionCalls,
// keploy.yaml sessionCalls) names exactly, by its method and its whole path
// (SessionCallRule.names). Under the proven switch a test is served such a
// call from a recording made outside every test (exactView.sharesRest); a
// rule that covers it only by a path prefix, or with no method, does not
// serve it there, so that no recording is served to a path or method no
// one declared. DeriveLifetime gives a stated per-test call any rule covers
// LifetimeSession, but it stays a data-plane recording (IsProtocolPhase
// reads the stated lifetime).
func (m *Mock) DeclaredSessionCall() bool {
	if m == nil {
		return false
	}
	lt, ok := statedLifetime(m)
	return ok && lt == LifetimePerTest && namedSessionCall(m)
}

// SessionCallDeclared says how the declared session calls (SetSessionCalls)
// declare a call of method to host and path, a live call, as a miss's hint
// reads it: covered, when a rule covers it (by a path prefix too), and
// named, when a rule names it exactly (SessionCallRule.names), which alone
// serves it to a test under the proven switch. An empty path is the root,
// "/", as httpCallOf reads a recording's.
func SessionCallDeclared(method, host, path string) (covered, named bool) {
	rules := sessionCalls.Load()
	if rules == nil {
		return false, false
	}
	if path == "" {
		path = "/"
	}
	return rulesCover(*rules, method, host, path), rulesName(*rules, method, host, path)
}

// rulesName reports whether one of rules names a call of method to host and
// path exactly (SessionCallRule.names).
func rulesName(rules []SessionCallRule, method, host, path string) bool {
	for _, r := range rules {
		if r.names(method, host, path) {
			return true
		}
	}
	return false
}

// rulesCover reports whether one of rules covers a call of method to host and
// path.
func rulesCover(rules []SessionCallRule, method, host, path string) bool {
	for _, r := range rules {
		if r.matches(method, host, path) {
			return true
		}
	}
	return false
}

// declaredSessionCall reports whether m is an HTTP or HTTP/2 call a declared
// session call covers (SetSessionCalls).
func declaredSessionCall(m *Mock) bool {
	rules := sessionCalls.Load()
	if rules == nil || len(*rules) == 0 {
		return false
	}
	method, host, path, ok := httpCallOf(m)
	return ok && rulesCover(*rules, method, host, path)
}

// namedSessionCall reports whether m is an HTTP or HTTP/2 call a declared
// session call names exactly (SessionCallRule.names).
func namedSessionCall(m *Mock) bool {
	rules := sessionCalls.Load()
	if rules == nil {
		return false
	}
	method, host, path, ok := httpCallOf(m)
	return ok && rulesName(*rules, method, host, path)
}

// httpCallOf is the method, upstream host and path of an HTTP or HTTP/2 mock's
// request. The recorder writes an HTTP request's URL as the client sent it,
// usually a path, with the upstream in the Host header.
func httpCallOf(m *Mock) (method, host, path string, ok bool) {
	switch {
	case m.Kind == HTTP && m.Spec.HTTPReq != nil:
		method = string(m.Spec.HTTPReq.Method)
		u, err := url.Parse(m.Spec.HTTPReq.URL)
		if err != nil {
			return "", "", "", false
		}
		host, path = u.Host, u.Path
		if host == "" {
			for k, v := range m.Spec.HTTPReq.Header {
				if strings.EqualFold(k, "Host") {
					host = v
					break
				}
			}
		}
	case m.Kind == HTTP2 && m.Spec.HTTP2Req != nil:
		method = string(m.Spec.HTTP2Req.Method)
		host = m.Spec.HTTP2Req.Authority
		u, err := url.Parse(m.Spec.HTTP2Req.URL)
		if err != nil {
			return "", "", "", false
		}
		path = u.Path
		if host == "" {
			host = u.Host
		}
	default:
		return "", "", "", false
	}
	if path == "" {
		path = "/"
	}
	return method, host, path, host != ""
}
