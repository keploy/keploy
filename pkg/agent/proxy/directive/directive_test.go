package directive

import (
	"crypto/tls"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

func TestKindString(t *testing.T) {
	t.Parallel()
	cases := map[Kind]string{
		KindUpgradeTLS:        "upgrade-tls",
		KindPauseDir:          "pause",
		KindResumeDir:         "resume",
		KindAbortMock:         "abort-mock",
		KindFinalizeMock:      "finalize-mock",
		KindResumePreDispatch: "resume-pre-dispatch",
		Kind(99):              "unknown",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", k, got, want)
		}
	}
}

func TestUpgradeTLSHelper(t *testing.T) {
	t.Parallel()
	dest := &tls.Config{ServerName: "d"}
	client := &tls.Config{ServerName: "c"}
	d := UpgradeTLS(dest, client, "postgres sslrequest")
	if d.Kind != KindUpgradeTLS {
		t.Errorf("Kind = %v, want KindUpgradeTLS", d.Kind)
	}
	if d.TLS == nil || d.TLS.DestTLSConfig != dest || d.TLS.ClientTLSConfig != client {
		t.Errorf("TLS params not plumbed through: %+v", d.TLS)
	}
	if d.Reason != "postgres sslrequest" {
		t.Errorf("Reason = %q", d.Reason)
	}
}

func TestSimpleHelpers(t *testing.T) {
	t.Parallel()
	a := AbortMock("decode fail")
	if a.Kind != KindAbortMock || a.Reason != "decode fail" {
		t.Errorf("AbortMock = %+v", a)
	}
	f := FinalizeMock("ok")
	if f.Kind != KindFinalizeMock {
		t.Errorf("FinalizeMock kind = %v", f.Kind)
	}
	p := Pause(fakeconn.FromClient, "tls upgrade")
	if p.Kind != KindPauseDir || p.Dir != fakeconn.FromClient {
		t.Errorf("Pause = %+v", p)
	}
	r := Resume(fakeconn.FromDest, "resume")
	if r.Kind != KindResumeDir || r.Dir != fakeconn.FromDest {
		t.Errorf("Resume = %+v", r)
	}
	rpd := ResumePreDispatch("parser-decided-no-tls")
	if rpd.Kind != KindResumePreDispatch {
		t.Errorf("ResumePreDispatch kind = %v, want %v", rpd.Kind, KindResumePreDispatch)
	}
	if rpd.Reason != "parser-decided-no-tls" {
		t.Errorf("ResumePreDispatch reason = %q, want %q", rpd.Reason, "parser-decided-no-tls")
	}
	// ResumePreDispatch does not target a direction (the relay drains
	// both directions' stashes regardless), so Dir should be the zero
	// value.
	if rpd.Dir != fakeconn.Direction(0) {
		t.Errorf("ResumePreDispatch Dir = %v, want zero value (directive applies to both directions)", rpd.Dir)
	}
}
