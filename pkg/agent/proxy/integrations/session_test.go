package integrations_test

import (
	"io"
	"net"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.uber.org/zap"
)

// readerOnlyRecordConn satisfies integrations.RecordConn but deliberately
// not net.Conn — no Close, no deadline setters. It exists so the
// "does not satisfy net.Conn" branch of recordNetConn is reachable from
// the test without depending on any production type.
type readerOnlyRecordConn struct{}

func (readerOnlyRecordConn) Read(_ []byte) (int, error)  { return 0, io.EOF }
func (readerOnlyRecordConn) Write(_ []byte) (int, error) { return 0, io.EOF }
func (readerOnlyRecordConn) RemoteAddr() net.Addr        { return nil }
func (readerOnlyRecordConn) LocalAddr() net.Addr         { return nil }

// TestRecordSessionConnAccessors locks in the (net.Conn, error) contract
// for IngressConn / EgressConn introduced when the helpers stopped
// nil-deref panicking on V2-only sessions.
func TestRecordSessionConnAccessors(t *testing.T) {
	// Real net.Conn for the happy path. SafeConn satisfies net.Conn so
	// the type assertion in recordNetConn succeeds.
	srv, cli := net.Pipe()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = cli.Close()
	})
	safeIngress := util.NewSafeConn(srv, zap.NewNop())
	safeEgress := util.NewSafeConn(cli, zap.NewNop())

	tests := []struct {
		name        string
		session     *integrations.RecordSession
		wantIngress string // substring expected in the IngressConn error, "" = no error
		wantEgress  string // substring expected in the EgressConn error, "" = no error
	}{
		{
			name:        "nil session",
			session:     nil,
			wantIngress: "record session ingress: nil session",
			wantEgress:  "record session egress: nil session",
		},
		{
			name:        "nil ingress and egress",
			session:     &integrations.RecordSession{},
			wantIngress: "connection not initialized",
			wantEgress:  "connection not initialized",
		},
		{
			name: "RecordConn that does not satisfy net.Conn",
			session: &integrations.RecordSession{
				Ingress: readerOnlyRecordConn{},
				Egress:  readerOnlyRecordConn{},
			},
			wantIngress: "does not satisfy net.Conn",
			wantEgress:  "does not satisfy net.Conn",
		},
		{
			name: "happy path with SafeConn",
			session: &integrations.RecordSession{
				Ingress: safeIngress,
				Egress:  safeEgress,
			},
			wantIngress: "",
			wantEgress:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.session.IngressConn()
			checkConnResult(t, "IngressConn", got, err, tc.wantIngress)
			// Error message should reference the public method name so
			// callers see the API surface, not the lowercase internal label.
			if tc.wantIngress != "" && err != nil &&
				tc.wantIngress == "connection not initialized" &&
				!strings.Contains(err.Error(), "IngressConn") {
				t.Errorf("IngressConn error %q should reference method name", err.Error())
			}

			got, err = tc.session.EgressConn()
			checkConnResult(t, "EgressConn", got, err, tc.wantEgress)
			if tc.wantEgress != "" && err != nil &&
				tc.wantEgress == "connection not initialized" &&
				!strings.Contains(err.Error(), "EgressConn") {
				t.Errorf("EgressConn error %q should reference method name", err.Error())
			}
		})
	}
}

func checkConnResult(t *testing.T, label string, got net.Conn, err error, wantSubstr string) {
	t.Helper()
	if wantSubstr == "" {
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", label, err)
		}
		if got == nil {
			t.Fatalf("%s: expected non-nil net.Conn on happy path", label)
		}
		return
	}
	if err == nil {
		t.Fatalf("%s: expected error containing %q, got nil", label, wantSubstr)
	}
	if got != nil {
		t.Fatalf("%s: expected nil net.Conn on error, got %T", label, got)
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Fatalf("%s: error %q does not contain %q", label, err.Error(), wantSubstr)
	}
}
