package proxy

import (
	"net/netip"
	"os"
	"strings"

	"go.uber.org/zap"
)

// Destinations that are relayed without interception.
//
// Keploy MITMs egress TLS so it can capture mocks, and during RECORDING there
// is deliberately no pin bypass — the Go/JSSE/OpenSSL bypasses are gated to
// observe-replay. That is fine for application dependencies, whose clients can
// be pointed at keploy's CA through SSL_CERT_FILE and friends. It is not fine
// for the Kubernetes API server.
//
// An in-cluster client builds its trust pool from exactly one file:
//
//	rootCAFile = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
//	tlsClientConfig.CAFile = rootCAFile          // client-go rest/config.go
//
// Setting CAFile explicitly means RootCAs is built from that file ALONE, so
// none of the CA env vars the injector sets can reach it. The pod's own
// kubelet-mounted trust is also not ours to rewrite. So a recorded app that
// talks to its API server — every operator, every controller, anything using
// client-go — fails every call for the length of the recording, and an app
// that treats that as fatal crash-loops instead of serving traffic.
//
// Not intercepting is also the better recording: API server responses carry
// resourceVersions and watch streams, so mocks of them are worth little and
// age badly.
//
// Expressed in userspace rather than as a kernel bypass on purpose. The eBPF
// side can only express PORTS (ClientInfo carries PassThroughPorts [10]int32
// and nothing else), and bypassing a port would mean bypassing all of 443.
type noInterceptSet struct {
	addrs  map[netip.Addr]struct{}
	prefix []netip.Prefix
}

// newNoInterceptSet seeds from KUBERNETES_SERVICE_HOST, which the kubelet sets
// in every pod, so the common case needs no configuration at all.
//
// KEPLOY_NO_INTERCEPT_HOSTS adds further IPs or CIDRs, comma-separated, for
// anything else that pins a trust store keploy cannot reach.
// KEPLOY_DISABLE_APISERVER_BYPASS=true turns the seeding off, matching the
// shape of the existing KEPLOY_DISABLE_TLS_PIN_BYPASS kill switch.
func newNoInterceptSet(logger *zap.Logger) *noInterceptSet {
	s := &noInterceptSet{addrs: map[netip.Addr]struct{}{}}

	if !strings.EqualFold(strings.TrimSpace(os.Getenv("KEPLOY_DISABLE_APISERVER_BYPASS")), "true") {
		s.add(os.Getenv("KUBERNETES_SERVICE_HOST"), "KUBERNETES_SERVICE_HOST", logger)
	}
	for _, e := range strings.Split(os.Getenv("KEPLOY_NO_INTERCEPT_HOSTS"), ",") {
		s.add(e, "KEPLOY_NO_INTERCEPT_HOSTS", logger)
	}

	if n := len(s.addrs) + len(s.prefix); n > 0 && logger != nil {
		logger.Info("egress destinations that will not be intercepted",
			zap.Int("count", n), zap.String("reason", "the client pins a trust store keploy cannot add its CA to"))
	}
	return s
}

func (s *noInterceptSet) add(raw, source string, logger *zap.Logger) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	// A bare IP first: KUBERNETES_SERVICE_HOST is one, and it is the case
	// that matters most.
	if a, err := netip.ParseAddr(raw); err == nil {
		s.addrs[a.Unmap()] = struct{}{}
		return
	}
	if p, err := netip.ParsePrefix(raw); err == nil {
		s.prefix = append(s.prefix, p)
		return
	}
	// Deliberately not resolved: a name would have to be resolved per
	// connection, and the value this guards is reached by IP anyway.
	if logger != nil {
		logger.Warn("ignoring an unparseable no-intercept entry; expected an IP or a CIDR",
			zap.String("value", raw), zap.String("source", source))
	}
}

// matches reports whether host — a bare host, no port and no brackets — is a
// destination to relay untouched.
func (s *noInterceptSet) matches(host string) bool {
	if s == nil || host == "" {
		return false
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	a = a.Unmap()
	if _, ok := s.addrs[a]; ok {
		return true
	}
	for _, p := range s.prefix {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
