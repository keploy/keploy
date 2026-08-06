package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// TestResolveUpstreamTLSConfig_Precedence pins that record.upstreamTls resolves
// flag > yaml > default, and that it resolves IDENTICALLY whether the agent is
// native (it reads the orchestrator's keploy.yml through --config-path, so BOTH
// sources are populated) or containerised (only argv arrives).
//
// FAILS BEFORE THE FIX: the agent computed
// `opts.Agent.UpstreamTLSVerify || opts.Record.UpstreamTLS.Verify`. An OR can
// only ever ADD verification, so the "native: explicit false over yaml true"
// row came back true while the byte-identical docker invocation came back
// false. One config, two behaviours — for the off switch of a check that
// breaks application connections.
func TestResolveUpstreamTLSConfig_Precedence(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// yaml — what the agent read out of keploy.yml (record:). Empty on a
		// containerised agent, populated on a native one.
		yamlVerify bool
		yamlCACert string
		// argv — what the orchestrator forwarded. Present iff …Set.
		argvSet    bool
		argvVerify bool
		argvCACert string
		argvCASet  bool

		wantVerify bool
		wantCACert string
	}{
		{
			name:       "nothing configured anywhere stays off",
			wantVerify: false,
		},
		{
			name:       "yaml alone turns it on (hand-started agent, no argv)",
			yamlVerify: true,
			yamlCACert: "/etc/corp/ca.pem",
			wantVerify: true,
			wantCACert: "/etc/corp/ca.pem",
		},
		{
			// The MEDIUM finding, native shape: the orchestrator resolved
			// false from an explicit --upstream-tls-verify=false and forwarded
			// it, but the agent ALSO sees the keploy.yml that says true.
			name:       "explicit argv false beats yaml true (native)",
			yamlVerify: true,
			yamlCACert: "/etc/corp/ca.pem",
			argvSet:    true,
			argvVerify: false,
			argvCASet:  true,
			argvCACert: "",
			wantVerify: false,
			wantCACert: "",
		},
		{
			// The same invocation under docker: the container never sees
			// keploy.yml, so only argv is populated. It MUST land on the same
			// answer as the row above.
			name:       "explicit argv false, no yaml visible (docker)",
			argvSet:    true,
			argvVerify: false,
			argvCASet:  true,
			argvCACert: "",
			wantVerify: false,
			wantCACert: "",
		},
		{
			name:       "argv true is forwarded to a container with no yaml",
			argvSet:    true,
			argvVerify: true,
			argvCASet:  true,
			argvCACert: "/certs/ca.pem",
			wantVerify: true,
			wantCACert: "/certs/ca.pem",
		},
		{
			// Per-flag markers: a hand-started `keploy agent
			// --upstream-tls-verify` (no --upstream-tls-ca-cert) must still
			// pick the CA up from its own keploy.yml.
			name:       "verify from argv, caCert still from yaml",
			yamlCACert: "/etc/corp/ca.pem",
			argvSet:    true,
			argvVerify: true,
			wantVerify: true,
			wantCACert: "/etc/corp/ca.pem",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{}
			cfg.Record.UpstreamTLS.Verify = tc.yamlVerify
			cfg.Record.UpstreamTLS.CACert = tc.yamlCACert
			cfg.Agent.UpstreamTLSVerify = tc.argvVerify
			cfg.Agent.UpstreamTLSVerifySet = tc.argvSet
			cfg.Agent.UpstreamTLSCACert = tc.argvCACert
			cfg.Agent.UpstreamTLSCACertSet = tc.argvCASet

			gotVerify, gotCACert := resolveUpstreamTLSConfig(cfg)
			if gotVerify != tc.wantVerify {
				t.Errorf("verify = %v, want %v", gotVerify, tc.wantVerify)
			}
			if gotCACert != tc.wantCACert {
				t.Errorf("caCert = %q, want %q", gotCACert, tc.wantCACert)
			}
		})
	}
}

// newProxyForUpstreamTLS builds a Proxy through the real New() so the test
// exercises the same resolution path the agent does.
func newProxyForUpstreamTLS(t *testing.T, mutate func(*config.Config)) (*Proxy, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	cfg := &config.Config{}
	if mutate != nil {
		mutate(cfg)
	}
	return New(zap.New(core), nil, cfg), logs
}

// TestUpstreamTLSTrustAnchorsAreNotLoadedUntilRecord is the fix for the agent
// logging "upstream TLS certificate verification is enabled" — and LogError-ing
// a bad caCert — during `keploy test`, which never verifies anything.
//
// proxy.New runs once per agent process regardless of mode, and a native replay
// agent reads the very same keploy.yml as the recording CLI. Proxy.Mock
// deliberately never stamps these options, so nothing in MODE_TEST consumes
// them; the load therefore belongs on the record path.
//
// FAILS BEFORE THE FIX: New() read the PEM and logged, so `logs.Len()` was
// non-zero here with a caCert that does not exist.
func TestUpstreamTLSTrustAnchorsAreNotLoadedUntilRecord(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.pem")

	p, logs := newProxyForUpstreamTLS(t, func(cfg *config.Config) {
		cfg.Record.UpstreamTLS.Verify = true
		cfg.Record.UpstreamTLS.CACert = missing
	})

	if n := logs.Len(); n != 0 {
		t.Fatalf("New() emitted %d log entries for a record-only setting; a replay agent must stay silent about it: %v", n, logs.All())
	}
	if p.upstreamTLSLoadFailed {
		t.Fatal("New() already attempted the CA load")
	}
	if p.upstreamTLSRootCAs != nil {
		t.Fatal("New() already built a root pool")
	}

	// The record path is what consumes it — and there the misconfiguration
	// must be loud.
	opts := models.OutgoingOptions{}
	p.applyUpstreamTLSOptions(&opts)

	if !p.upstreamTLSLoadFailed {
		t.Fatal("the unreadable caCert did not surface as a load failure on the record path")
	}
	if opts.UpstreamTLSVerify {
		t.Fatal("verification stayed on with no trust anchors; a failed dest handshake would drop mocks silently")
	}
	if logs.FilterMessageSnippet("failed to load upstream TLS trust anchors").Len() == 0 {
		t.Fatalf("the record path did not report the unreadable caCert: %v", logs.All())
	}
}

// TestApplyUpstreamTLSOptions covers the /outgoing stamping contract.
func TestApplyUpstreamTLSOptions(t *testing.T) {
	t.Run("wire-only verify is refused, loudly", func(t *testing.T) {
		// A manually started `keploy agent` in a container with no keploy.yml
		// and no argv flags, while the recording CLI has
		// record.upstreamTls.verify: true. Honouring the request would verify
		// against Go's default roots with the operator's private CA absent —
		// the dest handshake fails, the supervisor falls through to raw
		// passthrough, and the mock is dropped with no user-visible cause.
		//
		// FAILS BEFORE THE FIX: applyUpstreamTLSOptions did
		// `opts.UpstreamTLSVerify = opts.UpstreamTLSVerify || p.upstreamTLSVerify`,
		// so the wire flag alone switched verification on with a nil pool.
		p, logs := newProxyForUpstreamTLS(t, nil)

		opts := models.OutgoingOptions{UpstreamTLSVerify: true}
		p.applyUpstreamTLSOptions(&opts)

		if opts.UpstreamTLSVerify {
			t.Fatal("a wire-only verify request was honoured by an agent that resolved no trust anchors")
		}
		if opts.UpstreamTLSRootCAs != nil {
			t.Fatal("a root pool appeared out of nowhere")
		}
		if logs.FilterMessageSnippet("ignoring the recording session's request to verify").Len() == 0 {
			t.Fatalf("the refusal was silent; the operator gets no clue why nothing is verified: %v", logs.All())
		}
	})

	t.Run("an agent that resolved verify off stamps off", func(t *testing.T) {
		// The end state of `keploy record --upstream-tls-verify=false` over a
		// keploy.yml `verify: true`: argv said false, so the agent stamps
		// false, and the /outgoing request cannot put it back.
		p, _ := newProxyForUpstreamTLS(t, func(cfg *config.Config) {
			cfg.Record.UpstreamTLS.Verify = true
			cfg.Agent.UpstreamTLSVerify = false
			cfg.Agent.UpstreamTLSVerifySet = true
		})

		opts := models.OutgoingOptions{UpstreamTLSVerify: true}
		p.applyUpstreamTLSOptions(&opts)

		if opts.UpstreamTLSVerify {
			t.Fatal("--upstream-tls-verify=false did not switch verification off")
		}
	})

	t.Run("an agent that resolved verify on stamps on", func(t *testing.T) {
		// No caCert: LoadUpstreamRootCAs returns (nil, nil) — "use Go's
		// default roots" — which is a valid, populated trust store and NOT
		// "trust nothing".
		p, logs := newProxyForUpstreamTLS(t, func(cfg *config.Config) {
			cfg.Record.UpstreamTLS.Verify = true
		})

		opts := models.OutgoingOptions{}
		p.applyUpstreamTLSOptions(&opts)

		if !opts.UpstreamTLSVerify {
			t.Fatal("record.upstreamTls.verify: true did not reach the session options")
		}
		if logs.FilterMessageSnippet("upstream TLS certificate verification is enabled").Len() == 0 {
			t.Fatalf("the record path did not announce that verification is on (the e2e greps for this line): %v", logs.All())
		}
	})

	t.Run("the trust anchors are loaded at most once", func(t *testing.T) {
		caPath := writeTestCAPEM(t)
		p, logs := newProxyForUpstreamTLS(t, func(cfg *config.Config) {
			cfg.Record.UpstreamTLS.Verify = true
			cfg.Record.UpstreamTLS.CACert = caPath
		})

		for i := 0; i < 3; i++ {
			opts := models.OutgoingOptions{}
			p.applyUpstreamTLSOptions(&opts)
			if !opts.UpstreamTLSVerify || opts.UpstreamTLSRootCAs == nil {
				t.Fatalf("session %d did not receive the resolved trust material", i)
			}
		}
		// One PEM read, ~150 root parses — per session would be waste, per
		// connection would be a hot-path regression.
		if n := logs.FilterMessageSnippet("loaded extra trust anchors").Len(); n != 1 {
			t.Fatalf("the CA bundle was loaded %d times across 3 record sessions; want exactly 1", n)
		}
	})
}

// TestDefaultOffLeavesSessionOptionsUntouched is the headline guarantee: with
// nothing configured, every dial site sees exactly the zero values it saw
// before record.upstreamTls existed.
func TestDefaultOffLeavesSessionOptionsUntouched(t *testing.T) {
	p, logs := newProxyForUpstreamTLS(t, nil)

	opts := models.OutgoingOptions{}
	p.applyUpstreamTLSOptions(&opts)

	if opts.UpstreamTLSVerify {
		t.Fatal("UpstreamTLSVerify is true with nothing configured")
	}
	if opts.UpstreamTLSRootCAs != nil {
		t.Fatal("UpstreamTLSRootCAs is non-nil with nothing configured")
	}
	if n := logs.Len(); n != 0 {
		t.Fatalf("the default path emitted %d log entries: %v", n, logs.All())
	}
}

// writeTestCAPEM writes a throwaway self-signed CA to a temp file and returns
// its path. Generated rather than hard-coded so the bundle is genuinely
// parseable by x509 — a fixture that failed to parse would make
// "loaded exactly once" pass for the wrong reason.
func writeTestCAPEM(t *testing.T) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(11),
		Subject:               pkix.Name{CommonName: "keploy-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	return path
}
