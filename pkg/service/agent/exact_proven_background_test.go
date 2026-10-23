package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// testQueue is a kind of this test's own, so no parser's registration is
// read: its recordings are keyed by their metadata's key, and the ones whose
// metadata says idle answer idle.
const testQueue models.Kind = "S12TestQueue"

func registerTestQueue(t *testing.T) {
	t.Helper()
	t.Cleanup(models.RegisterExactKey(testQueue, func(m *models.Mock) (string, bool) {
		k := m.Spec.Metadata["key"]
		return k, k != ""
	}))
	t.Cleanup(models.RegisterIdleAnswer(testQueue, func(m *models.Mock) bool { return m.Spec.Metadata["idle"] == "true" }))
}

func queueMock(name, key string, idle bool) *models.Mock {
	md := map[string]string{"key": key}
	if idle {
		md["idle"] = "true"
	}
	return &models.Mock{Name: name, Kind: testQueue, Spec: models.MockSpec{Metadata: md, ReqTimestampMock: time.Unix(1, 0)}}
}

// A worker-scope recording whose kind keys it, and that no other recording
// of the set's background (its worker scope and its pool) shares the key of,
// has no identical sibling; identical ones, one the kind cannot key and one
// not read all have (the no-sibling rule).
func TestBackgroundOfTellsTheRecordingsWithNoSibling(t *testing.T) {
	registerTestQueue(t)
	byName := map[string]*models.Mock{
		"delete-1": queueMock("delete-1", "DeleteMessage rh-1", false),
		"poll-a":   queueMock("poll-a", "ReceiveMessage q", false),
		"poll-b":   queueMock("poll-b", "ReceiveMessage q", true),
		"pool-dup": queueMock("pool-dup", "DeleteMessage rh-2", false),
		"w-dup":    queueMock("w-dup", "DeleteMessage rh-2", false),
		"unkeyed":  queueMock("unkeyed", "", false),
	}
	worker := []models.MockEntry{{Name: "delete-1", Place: "post-url-1"}, {Name: "poll-a"}, {Name: "poll-b", Place: "post-url-1"}, {Name: "w-dup", Place: "post-url-1"}, {Name: "unkeyed", Place: "post-url-1"}, {Name: "unread", Place: "post-url-1"}}
	bg := backgroundOf(worker, []string{"pool-dup"}, byName)
	var no []string
	for _, e := range worker {
		if bg.noSibling[e.Name] {
			no = append(no, e.Name)
		}
	}
	if strings.Join(no, ",") != "delete-1" {
		t.Fatalf("the worker-scope recordings with no sibling are %v; want delete-1 alone (a pool recording is a sibling too)", no)
	}
	if !bg.idle["poll-b"] || bg.idle["poll-a"] {
		t.Fatalf("idle answers %v; want poll-b's alone", bg.idle)
	}
	// Nothing read: every recording stays gated.
	if bg := backgroundOf(worker, nil, nil); len(bg.noSibling) != 0 {
		t.Fatalf("with nothing read, %v have no sibling", bg.noSibling)
	}
}

// Today's rules reach a worker-scope recording from its place on, in recorded
// order. S12's reach one with no sibling from the set's start, wherever it is
// placed, keep identical siblings gated by their places, and order what they
// reach by place (the set's start, then each test in the order it began),
// within a place a delivering answer before an idle one, then recorded order.
func TestReachableWorkerGatesSiblingsAndOrdersByPlace(t *testing.T) {
	worker := []models.MockEntry{
		{Name: "boot-idle"},
		{Name: "boot-poll"},
		{Name: "t2-poll", Place: "T2"},
		{Name: "t1-idle", Place: "T1"},
		{Name: "t1-poll", Place: "T1"},
		{Name: "t3-delete", Place: "T3"},
		{Name: "t3-sibling", Place: "T3"},
	}
	begun := map[string]bool{"T1": true, "T2": true}
	begunAt := map[string]int{"T2": 1, "T1": 2} // T2 began first
	if got := strings.Join(reachableWorker(worker, begun, begunAt, nil), ","); got != "boot-idle,boot-poll,t2-poll,t1-idle,t1-poll" {
		t.Fatalf("today's rules reach %s; want the begun places' in recorded order", got)
	}
	bg := &testBackground{
		noSibling: map[string]bool{"t3-delete": true},
		idle:      map[string]bool{"boot-idle": true, "t1-idle": true},
	}
	got := strings.Join(reachableWorker(worker, begun, begunAt, bg), ",")
	if want := "boot-poll,t3-delete,boot-idle,t2-poll,t1-poll,t1-idle"; got != want {
		t.Fatalf("S12's rules reach %s; want %s", got, want)
	}
	if strings.Contains(got, "t3-sibling") {
		t.Fatal("an identical sibling was reached before its place")
	}
}

// backgroundExactProxy is a workerExactProxy that serves S12's background
// rules (coreAgent.ExactBackgroundServer): it keeps each set of rules it is
// told, with its pool and worker scope.
type backgroundExactProxy struct {
	workerExactProxy
	rules []string
}

func (p *backgroundExactProxy) ServeExactBackground(rules string, pool, worker []string) error {
	p.rules = append(p.rules, rules+"|"+strings.Join(pool, ",")+"|"+strings.Join(worker, ","))
	return nil
}

func (p *backgroundExactProxy) SetRecordedOfKind(func(models.Kind, func(string, func() (*models.Mock, error)) bool) error) error {
	return nil
}

// A recording two of its kind's exact keys claim has no key: it counts as
// having siblings, so it stays gated by its place, and the background says
// why (models.ExactKeyErr), naming the recording. It is a sibling under each
// key it was given, so a recording one of those keys alone gives the same key
// stays gated too. A recording one key owns alone, with no sibling, keeps its
// no-sibling reach.
func TestBackgroundOfGatesARecordingTwoExactKeysClaim(t *testing.T) {
	registerTestQueue(t)
	t.Cleanup(models.RegisterExactKey(testQueue, func(m *models.Mock) (string, bool) {
		if k := m.Spec.Metadata["also"]; k != "" {
			return "also " + k, true
		}
		return "", false
	}))
	claimed := queueMock("claimed", "DeleteMessage rh-1", false)
	claimed.Spec.Metadata["also"] = "DeleteMessage rh-1"
	byName := map[string]*models.Mock{
		"claimed": claimed,
		"twin":    queueMock("twin", "DeleteMessage rh-1", false),
		"owned":   queueMock("owned", "ReceiveMessage q", false),
	}
	worker := []models.MockEntry{{Name: "claimed", Place: "post-url-1"}, {Name: "twin", Place: "post-url-1"}, {Name: "owned", Place: "post-url-1"}}
	bg := backgroundOf(worker, nil, byName)
	if bg.noSibling["claimed"] || bg.noSibling["twin"] || !bg.noSibling["owned"] {
		t.Fatalf("no sibling: claimed %v, twin %v, owned %v; want the claimed one and its twin gated, the owned one reached", bg.noSibling["claimed"], bg.noSibling["twin"], bg.noSibling["owned"])
	}
	if len(bg.keyErrs) != 1 || !strings.Contains(bg.keyErrs[0], "recording claimed") {
		t.Fatalf("the background says %q; want one error naming the claimed recording", bg.keyErrs)
	}
}

// Two parsers' exact keys are two namespaces: a recording one parser keys
// and one another keys with an equal string are no siblings of each other,
// so neither gates the other; two recordings one parser keys alike still
// are.
func TestBackgroundOfReadsEachExactKeyInItsOwnRegistration(t *testing.T) {
	registerTestQueue(t)
	t.Cleanup(models.RegisterExactKey(testQueue, func(m *models.Mock) (string, bool) {
		k := m.Spec.Metadata["other"]
		return k, k != ""
	}))
	other := func(name, key string) *models.Mock {
		return &models.Mock{Name: name, Kind: testQueue, Spec: models.MockSpec{Metadata: map[string]string{"other": key}, ReqTimestampMock: time.Unix(1, 0)}}
	}
	byName := map[string]*models.Mock{
		"queue":  queueMock("queue", "DeleteMessage rh-1", false),
		"parser": other("parser", "DeleteMessage rh-1"),
		"twin-a": other("twin-a", "ReceiveMessage q"),
		"twin-b": other("twin-b", "ReceiveMessage q"),
	}
	worker := []models.MockEntry{{Name: "queue", Place: "post-url-1"}, {Name: "parser", Place: "post-url-1"}, {Name: "twin-a", Place: "post-url-1"}, {Name: "twin-b", Place: "post-url-1"}}
	bg := backgroundOf(worker, nil, byName)
	if !bg.noSibling["queue"] || !bg.noSibling["parser"] {
		t.Fatalf("no sibling: queue %v, parser %v; want both reached, an equal key of another parser's being no sibling", bg.noSibling["queue"], bg.noSibling["parser"])
	}
	if bg.noSibling["twin-a"] || bg.noSibling["twin-b"] {
		t.Fatalf("no sibling: twin-a %v, twin-b %v; want both gated, siblings under one parser's key", bg.noSibling["twin-a"], bg.noSibling["twin-b"])
	}
	if len(bg.keyErrs) != 0 {
		t.Fatalf("the background says %q; want no error", bg.keyErrs)
	}
}

// The background is settled by S12's rules only under the proven switch: a
// staging that names them reaching an agent without it (one run with the
// switch off, whatever its client sent) settles none, and every worker-scope
// recording keeps today's gating.
func TestSettleBackgroundNeedsTheSwitch(t *testing.T) {
	registerTestQueue(t)
	a := &Agent{logger: zap.NewNop()}
	ex := &models.ExactServing{Stage: true, Background: models.RecordedWithS12Routing}
	t.Setenv("KEPLOY_PROVEN", "")
	if bg := a.settleBackground(ex); bg != nil {
		t.Fatalf("without the switch the background is settled: %+v", bg)
	}
	t.Setenv("KEPLOY_PROVEN", "1")
	if bg := a.settleBackground(ex); bg == nil {
		t.Fatal("under the switch a staging naming S12's rules settles no background")
	}
}

// A staging that names S12's rules (models.ExactServing.Background) tells the
// proxy so, with the set's pool and whole worker scope, after withdrawing the
// previous set's; and it reaches a worker-scope recording with no identical
// sibling from the set's start, though it is placed after a test not begun. A
// staging that names none is today's: the previous set's rules are withdrawn,
// none told, and the placed recording waits for its test. A proxy that cannot
// serve S12's rules is staged by today's, and the agent says so. Without the
// switch nothing is staged exactly.
func TestTheS12StagingTellsTheProxyItsRulesAndReachesANoSiblingRecording(t *testing.T) {
	defer models.RegisterExactKey(models.HTTP, func(m *models.Mock) (string, bool) {
		return m.Spec.HTTPReq.URL, true
	})()
	for _, tc := range []struct {
		name      string
		on        bool
		rules     string
		wantRules []string
		reached   []string
	}{
		{"S12's rules", true, models.RecordedWithS12Routing,
			[]string{"||", models.RecordedWithS12Routing + "|boot-config|w-delete,w-poll"}, []string{"w-delete", "w-poll"}},
		{"today's rules", true, "", []string{"||"}, []string{"w-delete"}},
		{"S12's rules, a proxy that cannot serve them", true, models.RecordedWithS12Routing, nil, []string{"w-delete"}},
		// Without the switch nothing is staged exactly: nothing is told.
		{"S12's rules named without the switch", false, models.RecordedWithS12Routing, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.on {
				t.Setenv("KEPLOY_PROVEN", "1")
			} else {
				t.Setenv("KEPLOY_PROVEN", "")
			}
			t.Setenv("KEPLOY_STRICT_MOCK_WINDOW", "1")
			p := &backgroundExactProxy{}
			var proxy coreAgent.Proxy = p
			cannot := strings.Contains(tc.name, "cannot")
			if cannot {
				proxy = &p.workerExactProxy
			}
			a := diskAgent(t, proxy,
				stageMock("boot-config", "/config", stageT0.Add(-20*time.Second)),
				stageMock("test-a", "/q", stageT0.Add(time.Second)),
				stageMock("w-delete", "/delete", stageT0.Add(30*time.Second)),
				stageMock("w-poll", "/poll", stageT0.Add(time.Hour)))
			err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
				AfterTime:              models.BaseTime,
				BeforeTime:             stageT0.Add(2 * time.Hour),
				FirstRecordedTestStart: stageT0,
				StrictMockWindow:       true,
				Exact: &models.ExactServing{Stage: true, Pool: []string{"boot-config"}, Background: tc.rules,
					Worker: []models.MockEntry{{Name: "w-delete"}, {Name: "w-poll", Place: "test-a"}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if want := map[bool]string{true: models.RecordedWithS12Routing}[tc.rules != "" && tc.on && !cannot]; a.ServedBackground() != want {
				t.Fatalf("the agent says it serves the set by %q; want %q", a.ServedBackground(), want)
			}
			if !reflect.DeepEqual(p.rules, tc.wantRules) {
				t.Fatalf("the proxy was told the rules %q; want %q", p.rules, tc.wantRules)
			}
			if tc.reached == nil {
				if len(p.workers) != 0 {
					t.Fatalf("without the switch the proxy was told the worker scopes %v", p.workers)
				}
				return
			}
			workers := p.workers
			if cannot {
				workers = p.workerExactProxy.workers
			}
			if len(workers) == 0 || !reflect.DeepEqual(workers[len(workers)-1], tc.reached) {
				t.Fatalf("the proxy was told the worker scopes %v; want the last %v", workers, tc.reached)
			}
		})
	}
}
