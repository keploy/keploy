package proxy

import (
	"context"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// A name the test made, sent first by one call and answered whole by a later
// call that sends nothing of it, through the real mock manager: the later
// call is answered with this run's name, and its consumed state carries the
// variable it answers and its bind at the first call, which the replay
// writes onto both recordings.
func TestALearnStepAnswersAValueTheTestMadeAtALaterCall(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	p.ContinueTestErrorCapture()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	now := time.Now()
	perTest := func(m *models.Mock) *models.Mock {
		m.TestModeInfo.Lifetime = models.LifetimePerTest
		return m
	}
	mm.SetMocksWithWindow([]*models.Mock{
		perTest(r3Mock("mock-a", now, "POST", "/query", `{"op":"createBranch","input":{"name":"e2e-1111-aaaa"}}`, `{"ok":true}`)),
		perTest(r3Mock("mock-b", now.Add(time.Second), "POST", "/query", `{"op":"branches"}`, `{"branches":[{"name":"main"},{"name":"e2e-1111-aaaa"}]}`)),
	}, nil, now.Add(-time.Hour), now.Add(time.Hour))
	post := func(body string) string {
		return "POST /query HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	}
	opts := models.OutgoingOptions{Learn: true}
	if code, _ := r3Send(t, p, mm, opts, post(`{"op":"createBranch","input":{"name":"e2e-9999-zzzz"}}`)); code != http.StatusOK {
		t.Fatalf("the create got %d", code)
	}
	if code, got := r3Send(t, p, mm, opts, post(`{"op":"branches"}`)); code != http.StatusOK || got != `{"branches":[{"name":"main"},{"name":"e2e-9999-zzzz"}]}` {
		t.Fatalf("the list got %d %s", code, got)
	}
	if misses, _ := p.GetMockErrors(context.Background()); len(misses) != 0 {
		t.Fatalf("misses %+v", misses)
	}
	states := r3States(mm)
	if want := (models.MockVariables{"name": {Recorded: "e2e-1111-aaaa", Answer: []string{"body.branches[1].name"}}}); !reflect.DeepEqual(states["mock-b"].Variables, want) {
		t.Fatalf("mock-b's state carries %+v", states["mock-b"].Variables)
	}
	if want := (map[string]models.MockVariables{"mock-a": {"name": {Recorded: "e2e-1111-aaaa", Bind: "body.input.name"}}}); !reflect.DeepEqual(states["mock-b"].Binds, want) {
		t.Fatalf("mock-b's state binds %+v", states["mock-b"].Binds)
	}
	if _, ok := states["mock-a"].ReqBodyNoise["body.input.name"]; !ok {
		t.Fatalf("mock-a's state %+v: the name is noise there until the bind is written over it", states["mock-a"])
	}
}
