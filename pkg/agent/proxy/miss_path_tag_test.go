package proxy

import (
	"context"
	"strings"
	"testing"

	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg/models"
)

// The agent's miss line names an HTTP call by its shape and its path tag,
// made where the line is logged; the call it hands the CLI (GetMockErrors),
// which the console and the report print whole, carries the same tag. Two
// calls of one shape (POST /{}/{}) have two tags, and the line's advice
// names a path it quotes by pointing to the console's Hint with the tag. The
// tag is made under the session the connection's context carries
// (models.LogKeySession). A session that begins before the CLI reads the
// misses (a test set's staging while an earlier set's call is decided) has a
// key of its own: the calls read keep the tags their lines named, and the new
// session gives the same path another.
func TestTheMissLineAndTheCallItReportsCarryOneTag(t *testing.T) {
	logger, logs := logleak.Observe()
	p := &Proxy{logger: logger, errChannel: make(chan error, 4)}
	p.BeginTestErrorCapture()
	paths := []string{"/oauth/token", "/oauth/revoke"}
	session := models.StartLogKeySession(context.Background())
	var made []string
	for _, path := range paths {
		made = append(made, models.PathTag(session, "POST", path))
		p.sendMockNotFoundErrorFrom(session, models.NewMockMismatchError(models.ErrNoMockMatched, &models.MockMismatchReport{
			Protocol: "HTTP", Destination: "127.0.0.1:9096", ActualSummary: "POST " + path + "?client=web",
			NextSteps: `declare it (sessionCalls: [{host: "127.0.0.1:9096", method: "POST", pathPrefix: "` + path + `"}])`,
		}), 0, 0, &models.ConditionalDstCfg{Addr: "127.0.0.1:9096"})
	}
	next := models.StartLogKeySession(context.Background())
	got, err := p.GetMockErrors(next)
	if err != nil || len(got) != len(paths) {
		t.Fatalf("read %+v, %v; want both misses", got, err)
	}
	lines := logs.FilterMessage("mock mismatch: no matching mock for outgoing call").All()
	if len(lines) != len(paths) {
		t.Fatalf("logged %d miss lines, want %d", len(lines), len(paths))
	}
	for i, path := range paths {
		tag := made[i]
		if now := models.PathTag(next, "POST", path); now == tag {
			t.Errorf("POST %s has tag %s in the new session too", path, tag)
		}
		if process := models.PathTag(context.Background(), "POST", path); process == tag {
			t.Errorf("POST %s was tagged %s, under the process's key, not its session's", path, tag)
		}
		if got[i].PathTag != tag || got[i].ActualSummary != "POST "+path+"?client=web" {
			t.Errorf("the reported call is %q with tag %q, want its summary whole and tag %s", got[i].ActualSummary, got[i].PathTag, tag)
		}
		f := lines[i].ContextMap()
		next, _ := f["next_step"].(string)
		if f["call"] != "POST /{}/{}" || f["path_tag"] != tag || !strings.Contains(next, "pathPrefix: <the path keploy's console Hint and report name for [path tag "+tag+"]>") {
			t.Errorf("the miss line is %v, want the call's shape, path_tag %s and the advice by that tag", f, tag)
		}
		for k, v := range f {
			if s, ok := v.(string); ok && strings.Contains(s, "oauth") {
				t.Errorf("the miss line's %s names the call's path: %q", k, s)
			}
		}
	}
	if got[0].PathTag == got[1].PathTag {
		t.Fatal("two calls of one shape share a tag")
	}
}

// The miss line names the call's destination by its labels' shape beside its
// host tag, made in the connection's session; the call it hands the CLI
// carries that tag (HostTag) beside the destination whole, for the console,
// the report and the misses file to print. An address has no tag. A DNS
// question no mock answers carries its name's tag too (dns_logleak_test.go).
func TestTheMissLineAndTheCallItReportsCarryOneHostTag(t *testing.T) {
	logger, logs := logleak.Observe()
	p := &Proxy{logger: logger, errChannel: make(chan error, 4)}
	p.BeginTestErrorCapture()
	keys := models.NewLogKeySession()
	session := models.WithLogKeySession(context.Background(), keys)
	for _, dest := range []string{"api.tenant.example:443", "10.0.0.7:443"} {
		p.sendMockNotFoundErrorFrom(session, models.NewMockMismatchError(models.ErrNoMockMatched, &models.MockMismatchReport{
			Protocol: "HTTP", Destination: dest, ActualSummary: "GET /v1/items",
		}), 0, 0, &models.ConditionalDstCfg{Addr: "10.0.0.7:443"})
	}
	got, err := p.GetMockErrors(models.StartLogKeySession(context.Background()))
	if err != nil || len(got) != 2 {
		t.Fatalf("read %+v, %v; want both misses", got, err)
	}
	if want := keys.HostTag("api.tenant.example"); got[0].HostTag != want || got[0].Destination != "api.tenant.example:443" {
		t.Errorf("the reported call's destination is %q with host tag %q, want it whole beside %s", got[0].Destination, got[0].HostTag, want)
	}
	if got[1].HostTag != "" {
		t.Errorf("an address was given host tag %q", got[1].HostTag)
	}
	lines := logs.FilterMessage("mock mismatch: no matching mock for outgoing call").All()
	if len(lines) != 2 || lines[0].ContextMap()["destination"] != "{}.{}.{}:443 "+got[0].HostTag {
		t.Fatalf("the miss lines are %+v, want the destination by its shape beside the tag the call carries", lines)
	}
}
