package proxy

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"testing"
)

// ---------------------------------------------------------------------------
// Wiring pin: how recordViaSupervisor derives the relay's upstream-TLS
// settings from record.upstreamTls.verify.
//
// Everything ELSE about upstream verification is covered by ordinary
// behavioural tests, at every layer:
//
//   - the flag defaults to off:            config/default_test.go,
//     cli/provider/upstream_tls_flags_test.go,
//     upstream_tls_precedence_test.go::TestDefaultOffLeavesSessionOptionsUntouched
//   - ClientTLSFirst=false really is the historic destination-first
//     handshake order, and true really inverts it:
//     relay/client_tls_first_test.go::TestDirectiveUpgradeTLS_HandshakeOrder
//   - the dial config the fn builds, on and off, with and without a
//     captured SNI: upstream_tls_test.go, upstream_tls_sni_test.go
//   - end to end, on real sockets, with the flag off AND on:
//     .github/workflows/sample_tls_pcap.yml (4 modes)
//
// The one link none of those can see is the assignment in the middle:
// that the relay.Config literal in recordViaSupervisor binds BOTH
// upstream-TLS settings to opts.UpstreamTLSVerify and to nothing else.
// The relay takes ClientTLSFirst as a plain input, so its tests drive
// both values directly and stay green no matter what this side derives;
// hardcoding `true` here, or reading a neighbouring option, would flip
// the TLS handshake order for every recording user who never opted in
// and no unit test in the tree would notice.
//
// There is no runtime seam left to assert that through — the literal is
// inline in recordViaSupervisor, where it belongs, because two of its
// fields close over that function's own state (lastForwardNanos, and
// startOrphanTracking with its `defer close(trackerStop)`). So the pin
// is on the source. It proves the wiring, not the behaviour; the tests
// listed above prove the behaviour.
// ---------------------------------------------------------------------------

// relayConfigLiteral returns the field name -> value expression map of the
// relay.Config composite literal inside recordViaSupervisor, plus the name of
// that function's models.OutgoingOptions parameter.
//
// The parameter name matters: pinning the text "opts.UpstreamTLSVerify" is
// only meaningful if `opts` is still the session's OutgoingOptions and not
// some local that shadowed it.
func relayConfigLiteral(t *testing.T) (fields map[string]string, optsParam string) {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "proxy_v2.go", nil, 0)
	if err != nil {
		t.Fatalf("parse proxy_v2.go: %v", err)
	}

	render := func(e ast.Expr) string {
		var buf bytes.Buffer
		if err := printer.Fprint(&buf, fset, e); err != nil {
			t.Fatalf("render expression: %v", err)
		}
		return buf.String()
	}

	var fn *ast.FuncDecl
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "recordViaSupervisor" {
			fn = fd
			break
		}
	}
	if fn == nil {
		t.Fatal("recordViaSupervisor not found in proxy_v2.go — the V2 record entry point moved; re-point this pin at it")
	}

	for _, p := range fn.Type.Params.List {
		if render(p.Type) != "models.OutgoingOptions" {
			continue
		}
		for _, n := range p.Names {
			optsParam = n.Name
		}
	}
	if optsParam == "" {
		t.Fatal("recordViaSupervisor no longer takes a models.OutgoingOptions parameter; upstream TLS settings cannot be per-session any more")
	}

	fields = map[string]string{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || render(lit.Type) != "relay.Config" {
			return true
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			fields[key.Name] = render(kv.Value)
		}
		return false
	})
	if len(fields) == 0 {
		t.Fatal("no relay.Config literal found in recordViaSupervisor — the relay is configured somewhere else now; re-point this pin at it")
	}
	return fields, optsParam
}

// ClientTLSFirst INVERTS the order of the two TLS handshakes on the relay
// path. Destination-first is what every release before upstream verification
// existed did, and a dest-side failure is survivable there (keploy has not
// terminated the client's TLS yet, so the supervisor can still fall through to
// raw passthrough and the application's connection lives). Client-first closes
// the connection instead. So the value must track record.upstreamTls.verify
// exactly.
func TestRelayConfig_ClientTLSFirstIsDerivedFromUpstreamTLSVerify(t *testing.T) {
	t.Parallel()

	fields, opts := relayConfigLiteral(t)

	got, ok := fields["ClientTLSFirst"]
	if !ok {
		t.Fatal("relay.Config in recordViaSupervisor no longer sets ClientTLSFirst; record.upstreamTls.verify would keep the destination-first order and every hostname-addressed upstream would fail verification against the IP eBPF reported")
	}
	if want := opts + ".UpstreamTLSVerify"; got != want {
		t.Fatalf("ClientTLSFirst is set from %q, want %q — with anything else the relay's TLS handshake order stops tracking record.upstreamTls.verify, and if it can be true while the flag is unset it changes the wire behaviour of every recording user", got, want)
	}
}

// The dest-side dial and the handshake order are two halves of one decision:
// client-first exists only so the application's SNI is captured before the
// verifying dial happens. If they can disagree — verification on with
// destination-first, or client-first with verification off — the opt-in either
// verifies against the wrong name or reorders handshakes for users who never
// asked for it.
func TestRelayConfig_TLSUpgradeFnVerifiesOnTheSameFlag(t *testing.T) {
	t.Parallel()

	fields, opts := relayConfigLiteral(t)

	got, ok := fields["TLSUpgradeFn"]
	if !ok {
		t.Fatal("relay.Config in recordViaSupervisor no longer sets TLSUpgradeFn; every KindUpgradeTLS directive would be acked with ErrNoTLSUpgrader and TLS traffic would go unrecorded")
	}
	// Positional: newProxyTLSUpgradeFn(logger, verify, rootCAs, srcConn).
	want := "newProxyTLSUpgradeFn(logger, " + opts + ".UpstreamTLSVerify, " + opts + ".UpstreamTLSRootCAs, srcConn)"
	if got != want {
		t.Fatalf("TLSUpgradeFn is built as %q, want %q — the dest-side dial must take its verify decision and its trust anchors from the same session options that drive ClientTLSFirst, and its srcConn is the only way it can recover the SNI the application sent", got, want)
	}
}

// The half-close grace is an operator knob whose whole point is being
// reachable without a rebuild, so the one line binding it into the relay
// is worth pinning: delete it and the field silently reverts to the
// relay's built-in default for everyone, including the operator who set
// it in keploy.yml precisely because the default was wrong for them.
//
// Behaviour is covered elsewhere — relay/half_close_test.go proves the
// grace bounds idle time, that a negative disables half-close, and that
// withDefaults resolves zero. None of that can see whether this side
// actually passes the configured value through.
func TestRelayConfig_HalfCloseGraceIsWiredFromTheRecordBuffer(t *testing.T) {
	t.Parallel()

	fields, _ := relayConfigLiteral(t)

	got, ok := fields["HalfCloseGrace"]
	if !ok {
		t.Fatal("relay.Config in recordViaSupervisor no longer sets HalfCloseGrace; " +
			"record.recordBuffer.halfCloseGrace and --half-close-grace would both become " +
			"dead config, and an operator who disabled half-close would silently keep it on")
	}
	if want := "p.recordBufferHalfCloseGrace"; got != want {
		t.Fatalf("HalfCloseGrace is set from %q, want %q — anything else stops it tracking "+
			"record.recordBuffer.halfCloseGrace", got, want)
	}
}
