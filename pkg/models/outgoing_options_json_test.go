package models

import (
	"crypto/x509"
	"encoding/json"
	"strings"
	"testing"
)

// OutgoingOptions is JSON-encoded on the CLI → agent /outgoing request, and
// x509.CertPool has only unexported fields. Without `json:"-"` a populated pool
// marshals to `{}` and decodes on the agent side into a NON-NIL EMPTY pool —
// a trust store that trusts nothing, which fails every upstream handshake and
// (because a dest-side handshake failure falls through to raw passthrough)
// silently drops mocks instead of erroring. The pool is built agent-side from
// UpstreamTLSCACert precisely because it cannot survive this trip; this test
// pins that it never tries to.
func TestOutgoingOptions_RootCAsDoNotCrossTheWire(t *testing.T) {
	t.Parallel()

	pool := x509.NewCertPool()
	opts := OutgoingOptions{
		UpstreamTLSVerify:  true,
		UpstreamTLSRootCAs: pool,
	}

	encoded, err := json.Marshal(opts)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(encoded), "UpstreamTLSRootCAs") {
		t.Fatalf("UpstreamTLSRootCAs is serialised; it must carry `json:\"-\"`. payload: %s", encoded)
	}

	var decoded OutgoingOptions
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if decoded.UpstreamTLSRootCAs != nil {
		t.Error("decoded UpstreamTLSRootCAs is non-nil; a non-nil empty pool means \"trust nothing\" and fails every handshake")
	}
	// The boolean, by contrast, MUST survive — it is how the CLI tells the
	// agent that record.upstreamTls.verify is on.
	if !decoded.UpstreamTLSVerify {
		t.Error("UpstreamTLSVerify did not survive the JSON round trip")
	}
}

// LearnTests crosses the CLI → agent /outgoing request as it was set: an
// empty list (a round that learns onto no test) stays an empty list, which the
// agent reads as "no test learns", and nil (a CLI that learns onto every
// test's recording) stays nil. Read as nil, an empty list would make every
// test of the round fresh to the agent, and a carried test begun after a newer
// one would stop the round's learning.
func TestOutgoingOptions_LearnTestsKeepItsEmptyListAcrossTheWire(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name string
		in   map[string]bool
	}{
		{"no test", map[string]bool{}},
		{"every test", nil},
		{"one test", map[string]bool{"o::TestA": true}},
	} {
		encoded, err := json.Marshal(OutgoingOptions{Learn: true, LearnTests: c.in})
		if err != nil {
			t.Fatalf("%s: json.Marshal: %v", c.name, err)
		}
		var decoded OutgoingOptions
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("%s: json.Unmarshal: %v", c.name, err)
		}
		if (decoded.LearnTests == nil) != (c.in == nil) || len(decoded.LearnTests) != len(c.in) {
			t.Fatalf("%s: LearnTests %#v crossed the wire as %#v (payload %s)", c.name, c.in, decoded.LearnTests, encoded)
		}
		for name := range c.in {
			if !decoded.LearnTests[name] {
				t.Fatalf("%s: %s did not cross the wire", c.name, name)
			}
		}
	}
}
