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

// SetSessionCalls installs the process's declared session calls (keploy.yaml
// sessionCalls), which DeriveLifetime applies to every HTTP and HTTP/2 mock
// it derives per-test from then on. The keploy CLI installs them from its
// configuration before it reads a mock, and hands the agent each mock with
// the lifetime it derived.
func SetSessionCalls(rules []SessionCallRule) {
	cp := append([]SessionCallRule(nil), rules...)
	sessionCalls.Store(&cp)
}

// declaredSessionCall reports whether m is an HTTP or HTTP/2 call a declared
// session call covers (SetSessionCalls).
func declaredSessionCall(m *Mock) bool {
	rules := sessionCalls.Load()
	if rules == nil || len(*rules) == 0 {
		return false
	}
	method, host, path, ok := httpCallOf(m)
	if !ok {
		return false
	}
	for _, r := range *rules {
		if r.matches(method, host, path) {
			return true
		}
	}
	return false
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
