package proxy

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// beatMock is a reusable recording of POST /beat whose body's "at" is what
// a first cycle can learn as noise.
func beatMock(name string) *models.Mock {
	resp := `{"ok":true}`
	return &models.Mock{Version: "api.keploy.io/v1beta1", Name: name, Kind: models.HTTP, Spec: models.MockSpec{
		Metadata: map[string]string{"type": models.HTTPClient},
		HTTPReq: &models.HTTPReq{Method: "POST", URL: "http://deps/beat",
			Header: map[string]string{"Host": "deps", "Content-Type": "application/json"}, Body: `{"key":"k-1","at":"t-1"}`},
		HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json", "Content-Length": strconv.Itoa(len(resp))}, Body: resp},
		ReqTimestampMock: exactT0, ResTimestampMock: exactT0.Add(time.Millisecond),
	}, TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimeSession}}
}

// beatAt sends POST /beat with "at" at through the real HTTP matcher, and
// says what it was answered.
func beatAt(t *testing.T, mm *MockManager, at string) string {
	t.Helper()
	body := `{"key":"k-1","at":"` + at + `"}`
	resp, got, _ := serveOne(t, httpint.New(zap.NewNop()), mm, "POST /beat HTTP/1.1\r\nHost: deps\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body)
	if resp == nil {
		return "no answer"
	}
	return strconv.Itoa(resp.StatusCode) + " " + string(got)
}

// A test set's retry cycles are judged with what its first cycle learned on
// the recordings the set's staging put in the trees, not only a test's own:
// the pool's, between tests and, under the background rules, inside one,
// and the worker scope's. A call the first cycle answered only through
// learned drift (a heartbeat's changing "at") is answered so in the retry
// cycle, through the real HTTP matcher; it is not failed (a false red) for
// learning the retry cycle does not take. The staged recording itself is not
// written, and the learning is the set's only: the learn step taking up
// again (the next set's staging) takes it off.
func TestARetryCyclesStagedRecordingIsMatchedWithWhatTheFirstCycleLearned(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	kept := learn.NewKeptLearning([]models.MockState{{Name: "beat", ReqBodyNoise: map[string][]string{"body.at": {}}}})
	const answered = `200 {"ok":true}`
	for _, tc := range []struct {
		name  string
		serve func(mm *MockManager)
	}{
		{"the pool between tests", func(mm *MockManager) {
			mm.ServeExactPool("-", []string{"beat"})
		}},
		{"the pool in a test, under the background rules", func(mm *MockManager) {
			mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"beat"}, nil)
			mm.ServeExactTest("test-1", nil)
		}},
		{"the worker scope in a test", func(mm *MockManager) {
			mm.ServeExactWorker([]string{"beat"})
			mm.ServeExactTest("test-1", nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beat := beatMock("beat")
			mm := stagedManager(t, nil, []*models.Mock{beat})
			tc.serve(mm)
			if got := beatAt(t, mm, "t-2"); got == answered {
				t.Fatalf("precondition: a drifted call is answered with nothing learned: %s", got)
			}

			// The retry cycle's filter: the learn step ends, the kept
			// learning is loaded, and the filter's view goes live.
			mm.SetLearnEnded(true)
			mm.SetKeptLearning(kept)
			tc.serve(mm)
			if got := beatAt(t, mm, "t-2"); got != answered {
				t.Fatalf("the retry cycle missed a call the first cycle answered through what it learned (a false red): %s", got)
			}
			if beat.Spec.ReqBodyNoise != nil {
				t.Fatalf("the staged recording itself was written: %v", beat.Spec.ReqBodyNoise)
			}

			// The next set's staging takes the learn step up again, with
			// its own recordings.
			mm.SetLearnEnded(false)
			mm.SetMocksWithWindow(nil, []*models.Mock{beatMock("beat")}, models.BaseTime, time.Now())
			tc.serve(mm)
			if got := beatAt(t, mm, "t-4"); got == answered {
				t.Fatalf("the learning outlived the set's learn step: %s", got)
			}
			if got := beatAt(t, mm, "t-1"); got != answered {
				t.Fatalf("precondition: the recorded call is answered: %s", got)
			}
		})
	}
}

// What a first cycle's matcher filed onto a recording the staged trees keep
// (a reusable one, updated in place: UpdateUnFilteredMock, with what the
// learn step learned on its copy) is the first cycle's, not the round's: the
// round keeps only what it can tie to a test (an unread request's learning,
// an unpinned share's, an unresolved one's are not kept). Once the learn step
// has ended, the retry cycles are served the staged recording with exactly
// the kept learning, as the next keploy test reads it once the learning is
// persisted, whether the round keeps nothing at all, other recordings'
// learning, or other learning of this one. Otherwise a retry cycle answers a
// call through learning the round never writes: a false green.
func TestARetryCycleIsNotServedWhatTheFirstCycleFiledOnATreesRecording(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const answered = `200 {"ok":true}`
	for _, tc := range []struct {
		name string
		kept *learn.KeptLearning
	}{
		{"the round keeps no learning", nil},
		{"the round keeps another recording's", learn.NewKeptLearning([]models.MockState{{Name: "other", ReqBodyNoise: map[string][]string{"body.key": {}}}})},
		{"the round keeps other learning of it", learn.NewKeptLearning([]models.MockState{{Name: "beat", ReqBodyNoise: map[string][]string{"body.key": {}}}})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beat := beatMock("beat")
			mm := stagedManager(t, nil, []*models.Mock{beat, beatMock("other")})
			mm.ServeExactPool("-", []string{"beat", "other"})
			// The first cycle: a matcher files its copy of the recording with
			// what the learn step learned on it.
			var tree *models.Mock
			un, _ := mm.GetUnFilteredMocks()
			for _, mk := range un {
				if mk.Name == "beat" {
					tree = mk
				}
			}
			if tree == nil {
				t.Fatal("precondition: the pool holds no beat")
			}
			filed := *tree
			filed.TestModeInfo.IsFiltered = false
			filed.Spec.ReqBodyNoise = map[string][]string{"body.at": {}}
			if !mm.UpdateUnFilteredMock(tree, &filed) {
				t.Fatal("precondition: the matcher's copy is not filed")
			}
			if got := beatAt(t, mm, "t-2"); got != answered {
				t.Fatalf("precondition: the first cycle answers through what its matcher filed: %s", got)
			}

			// The retry cycle's filter: the learn step ends and the round's
			// kept learning is loaded (none is sent when it keeps none).
			mm.SetLearnEnded(true)
			if tc.kept != nil {
				mm.SetKeptLearning(tc.kept)
			}
			mm.ServeExactPool("-", []string{"beat", "other"})
			if got := beatAt(t, mm, "t-3"); got == answered {
				t.Fatalf("a retry cycle answered a call through learning the round does not keep (a false green): %s", got)
			}
			if got := beatAt(t, mm, "t-1"); got != answered {
				t.Fatalf("the retry cycle misses the recorded call: %s", got)
			}
		})
	}
}

// What a matcher filed in one staging is that staging's: the base a filed
// copy is handed out on once the learn step has ended (filedBases) is never
// another staging's. Mock names restart in every set, so the next set's
// recording of the same name, which its first cycle filed nothing over, is
// served in its retry cycles as that set staged it, never as the previous
// set's recording, with the noise and variables that set recorded on it: a
// call only those answer would be answered through them (a false green).
func TestARetryCycleIsNotServedWhatAnEarlierStagingFiled(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const answered = `200 {"ok":true}`
	// The set before's recording of beat ignores "at" (recorded noise).
	before := beatMock("beat")
	before.Spec.ReqBodyNoise = map[string][]string{"body.at": {}}
	mm := stagedManager(t, nil, []*models.Mock{before})
	mm.ServeExactPool("-", []string{"beat"})
	var tree *models.Mock
	un, _ := mm.GetUnFilteredMocks()
	for _, mk := range un {
		if mk.Name == "beat" {
			tree = mk
		}
	}
	if tree == nil {
		t.Fatal("precondition: the pool holds no beat")
	}
	filed := *tree
	filed.TestModeInfo.IsFiltered = false
	if !mm.UpdateUnFilteredMock(tree, &filed) {
		t.Fatal("precondition: the set before's matcher's copy is not filed")
	}
	if got := beatAt(t, mm, "t-2"); got != answered {
		t.Fatalf("precondition: the set before answers a drifted at through its recorded noise: %s", got)
	}

	// The next set, staged afresh with the learn step taken up again: its
	// first cycle files nothing over its own beat, which records no noise.
	mm.SetLearnEnded(false)
	mm.SetMocksWithWindow(nil, []*models.Mock{beatMock("beat")}, models.BaseTime, time.Now())
	mm.ServeExactPool("-", []string{"beat"})
	// Its retry cycle (a call its first cycle answers would file a copy).
	mm.SetLearnEnded(true)
	mm.ServeExactPool("-", []string{"beat"})
	if got := beatAt(t, mm, "t-3"); got == answered {
		t.Fatalf("a retry cycle answered a drifted call through the noise the set before recorded on its recording of the same name (a false green): %s", got)
	}
	if got := beatAt(t, mm, "t-1"); got != answered {
		t.Fatalf("the retry cycle misses the recorded call: %s", got)
	}
}

// A staged recording read twice in a retry cycle is the same learned copy,
// pooled so matchers copy it rather than edit it, through every read exact
// serving hands staged recordings out by: the tiers, a key's stateless
// list, a connection's own. The next set's generation drops the learning
// (dropPreviousSetConsumes), and so does a nil one.
func TestKeptLearningIsOneCopyPerStagedRecordingAndTheSetsOnly(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	beat := beatMock("beat")
	other := beatMock("other")
	conn := beatMock("conn")
	conn.Spec.Metadata = map[string]string{"type": "connection", "connID": "c-1"}
	conn.TestModeInfo.Lifetime = models.LifetimeConnection
	mm := stagedManager(t, nil, []*models.Mock{beat, other, conn})
	mm.ServeExactPool("-", []string{"beat", "other"})
	mm.SetLearnEnded(true)
	mm.SetKeptLearning(learn.NewKeptLearning([]models.MockState{
		{Name: "beat", ReqBodyNoise: map[string][]string{"body.at": {}}},
		{Name: "conn", ReqBodyNoise: map[string][]string{"body.at": {}}},
	}))
	read := func() map[string]*models.Mock {
		out := map[string]*models.Mock{}
		un, _ := mm.GetUnFilteredMocks()
		for _, mk := range un {
			out[mk.Name] = mk
		}
		return out
	}
	first, again := read(), read()
	if first["beat"] == nil || first["beat"] == beat {
		t.Fatalf("the pool's recording is not served a learned copy: %+v", first["beat"])
	}
	if _, ok := first["beat"].Spec.ReqBodyNoise["body.at"]; !ok {
		t.Fatalf("the learned copy lacks the learning: %v", first["beat"].Spec.ReqBodyNoise)
	}
	if again["beat"] != first["beat"] {
		t.Fatal("a staged recording read again is another copy")
	}
	if !first["beat"].Pooled() {
		t.Fatal("the learned copy is not pooled: a matcher may edit it")
	}
	if first["other"] != other {
		t.Fatal("a recording nothing was learned on is not served as staged")
	}
	if _, stateless := mm.GetStatelessMocks(models.HTTP, "beat"); len(stateless) != 1 || stateless[0] != first["beat"] {
		t.Fatalf("a key's stateless list is not served the learned copy: %+v", stateless)
	}
	byConn, err := mm.GetConnectionMocks("c-1")
	if err != nil || len(byConn) != 1 || byConn[0] == conn {
		t.Fatalf("a connection's own recording is not served a learned copy: %+v %v", byConn, err)
	}
	if _, ok := byConn[0].Spec.ReqBodyNoise["body.at"]; !ok {
		t.Fatalf("the connection's learned copy lacks the learning: %v", byConn[0].Spec.ReqBodyNoise)
	}

	mm.ArmSetFence()
	mm.swapMu.Lock()
	mm.dropPreviousSetConsumes()
	mm.swapMu.Unlock()
	if got := read()["beat"]; got != beat {
		t.Fatalf("the next set's generation is served the previous set's learning: %+v", got)
	}
	mm.SetKeptLearning(learn.NewKeptLearning([]models.MockState{{Name: "beat", ReqBodyNoise: map[string][]string{"body.at": {}}}}))
	mm.SetKeptLearning(nil)
	if got := read()["beat"]; got != beat {
		t.Fatal("a nil kept learning leaves the learning on")
	}
}

// The proxy puts the kept learning on its mock manager's staged recordings
// (agent.KeptLearningSetter); with no mock manager there is nothing to put
// it on, and learning to put is refused, never dropped.
func TestTheProxyPutsTheKeptLearningOnItsMockManager(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	kept := learn.NewKeptLearning([]models.MockState{{Name: "beat", ReqBodyNoise: map[string][]string{"body.at": {}}}})
	p := &Proxy{logger: zap.NewNop()}
	if err := p.SetKeptLearning(kept); err == nil {
		t.Fatal("a proxy with no mock manager took learning it cannot serve")
	}
	if err := p.SetKeptLearning(nil); err != nil {
		t.Fatalf("a proxy with no mock manager refused to take nothing: %v", err)
	}
	beat := beatMock("beat")
	mm := stagedManager(t, nil, []*models.Mock{beat})
	p.setMockManager(mm)
	mm.ServeExactPool("-", []string{"beat"})
	if err := p.SetKeptLearning(kept); err != nil {
		t.Fatal(err)
	}
	un, _ := mm.GetUnFilteredMocks()
	if len(un) != 1 || un[0] == beat {
		t.Fatalf("the proxy's mock manager serves the staged recording without the kept learning: %+v", un)
	}
}

// A consume that carries learning the copy exact serving handed out did not,
// once the set's learn step has ended, says so on its state
// (MockState.LearnedAfterEnd): a parser that ignores the end (one that keeps
// the learn tracker for a connection's life) served its answer through
// learning nothing keeps, so the replay gives its consumers no verdict. A
// consume that carries only the kept learning, or one made while the learn
// step runs, does not; and within one drain a later clean consume does not
// hide an earlier one that learned.
func TestAConsumeThatLearnsAfterTheLearnStepEndedSaysSo(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	kept := learn.NewKeptLearning([]models.MockState{{Name: "beat", ReqBodyNoise: map[string][]string{"body.at": {}}}})
	read := func(mm *MockManager) *models.Mock {
		un, _ := mm.GetUnFilteredMocks()
		for _, mk := range un {
			if mk.Name == "beat" {
				return mk
			}
		}
		t.Fatal("precondition: the pool holds no beat")
		return nil
	}
	file := func(mm *MockManager, noise map[string][]string, vars models.MockVariables) {
		t.Helper()
		got := read(mm)
		c := *got
		c.Spec.ReqBodyNoise = noise
		if vars != nil {
			c.Spec.SetLearned(vars, nil)
		}
		if !mm.UpdateUnFilteredMock(got, &c) {
			t.Fatal("precondition: the copy is not filed")
		}
	}
	state := func(mm *MockManager) models.MockState {
		for _, st := range mm.GetConsumedMocks() {
			if st.Name == "beat" {
				return st
			}
		}
		t.Fatal("beat's consume is not reported")
		return models.MockState{}
	}
	learned := models.MockVariables{"at": {}}
	for _, tc := range []struct {
		name  string
		ended bool
		noise map[string][]string
		vars  models.MockVariables
		want  bool
	}{
		{"the learn step runs, and learns", false, map[string][]string{"body.at": {}, "body.key": {}}, learned, false},
		{"ended, the kept learning only", true, map[string][]string{"body.at": {}}, nil, false},
		{"ended, a noise path beyond it", true, map[string][]string{"body.at": {}, "body.key": {}}, nil, true},
		{"ended, a learned variable", true, map[string][]string{"body.at": {}}, learned, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mm := stagedManager(t, nil, []*models.Mock{beatMock("beat")})
			mm.ServeExactPool("-", []string{"beat"})
			if tc.ended {
				mm.SetLearnEnded(true)
				mm.SetKeptLearning(kept)
				mm.ServeExactPool("-", []string{"beat"})
			}
			file(mm, tc.noise, tc.vars)
			if got := state(mm).LearnedAfterEnd; got != tc.want {
				t.Fatalf("LearnedAfterEnd = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("a later clean consume in the drain", func(t *testing.T) {
		mm := stagedManager(t, nil, []*models.Mock{beatMock("beat")})
		mm.SetLearnEnded(true)
		mm.SetKeptLearning(kept)
		mm.ServeExactPool("-", []string{"beat"})
		file(mm, map[string][]string{"body.at": {}, "body.key": {}}, nil)
		file(mm, map[string][]string{"body.at": {}}, nil)
		if !state(mm).LearnedAfterEnd {
			t.Fatal("a later clean consume hid one that learned after the end")
		}
		file(mm, map[string][]string{"body.at": {}}, nil)
		if state(mm).LearnedAfterEnd {
			t.Fatal("the next drain's clean consume says it learned after the end")
		}
	})
}

// A consume carries the clock reads the learn step of record's full round
// learned on the copy a parser consumed (MockSpec.Clocks) to the replay, as
// it carries variables: under the proven switch, and nothing without it.
// Two pairings of one recording carry the Meet of their reads, whichever
// came first, so a plain read met with one plus thirty minutes is Empty and
// refuses every value there. Once the set's learn step has ended, a copy
// whose clock reads the kept learning holds learns nothing beyond it; one
// with another offset, or another place, does (LearnedAfterEnd).
func TestAConsumeCarriesItsClockReadsAndSaysWhenTheyComeAfterTheLearnStep(t *testing.T) {
	plain := models.ClockOffset{Res: time.Millisecond}
	ttl := models.ClockOffset{Min: 30 * time.Minute, Max: 30*time.Minute + time.Millisecond, Res: time.Millisecond}
	read := func(mm *MockManager) *models.Mock {
		un, _ := mm.GetUnFilteredMocks()
		for _, mk := range un {
			if mk.Name == "beat" {
				return mk
			}
		}
		t.Fatal("precondition: the pool holds no beat")
		return nil
	}
	file := func(mm *MockManager, clocks map[string]models.ClockOffset) {
		t.Helper()
		got := read(mm)
		c := *got
		c.Spec.Clocks = clocks
		if !mm.UpdateUnFilteredMock(got, &c) {
			t.Fatal("precondition: the copy is not filed")
		}
	}
	state := func(mm *MockManager) models.MockState {
		for _, st := range mm.GetConsumedMocks() {
			if st.Name == "beat" {
				return st
			}
		}
		t.Fatal("beat's consume is not reported")
		return models.MockState{}
	}
	t.Run("switch off", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "")
		mm := stagedManager(t, nil, []*models.Mock{beatMock("beat")})
		mm.ServeExactPool("-", []string{"beat"})
		file(mm, map[string]models.ClockOffset{"body.at": plain})
		if got := state(mm).Clocks; got != nil {
			t.Fatalf("switch off: the consume carries clock reads %+v", got)
		}
	})
	for _, order := range [][]models.ClockOffset{{plain, ttl}, {ttl, plain}} {
		t.Run("met "+order[0].String()+" first", func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			mm := stagedManager(t, nil, []*models.Mock{beatMock("beat")})
			mm.ServeExactPool("-", []string{"beat"})
			file(mm, map[string]models.ClockOffset{"body.at": order[0], "body.only": plain})
			if got := state(mm).Clocks; !reflect.DeepEqual(got, map[string]models.ClockOffset{"body.at": order[0], "body.only": plain}) {
				t.Fatalf("the first consume carries clock reads %+v", got)
			}
			file(mm, map[string]models.ClockOffset{"body.at": order[1]})
			got := state(mm).Clocks
			if want := (map[string]models.ClockOffset{"body.at": plain.Meet(ttl), "body.only": plain}); !reflect.DeepEqual(got, want) || !got["body.at"].Empty() {
				t.Fatalf("two pairings carry clock reads %+v, want %+v (an Empty body.at)", got, want)
			}
		})
	}
	kept := learn.NewKeptLearning([]models.MockState{{Name: "beat", Clocks: map[string]models.ClockOffset{"body.at": ttl}}})
	for _, tc := range []struct {
		name   string
		clocks map[string]models.ClockOffset
		want   bool
	}{
		{"the kept learning's clock read", map[string]models.ClockOffset{"body.at": ttl}, false},
		{"another offset there", map[string]models.ClockOffset{"body.at": {Min: 31 * time.Minute, Max: 31*time.Minute + time.Millisecond, Res: time.Millisecond}}, true},
		{"a clock read at another place", map[string]models.ClockOffset{"body.at": ttl, "body.key": plain}, true},
	} {
		t.Run("ended, "+tc.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			mm := stagedManager(t, nil, []*models.Mock{beatMock("beat")})
			mm.SetLearnEnded(true)
			mm.SetKeptLearning(kept)
			mm.ServeExactPool("-", []string{"beat"})
			if got := read(mm).Spec.Clocks; !reflect.DeepEqual(got, map[string]models.ClockOffset{"body.at": ttl}) {
				t.Fatalf("precondition: a retry cycle is served beat with clock reads %+v, the kept learning's", got)
			}
			file(mm, tc.clocks)
			if got := state(mm).LearnedAfterEnd; got != tc.want {
				t.Fatalf("LearnedAfterEnd = %v, want %v", got, tc.want)
			}
		})
	}
}

// Every door a parser consumes a recording through says, once the set's
// learn step has ended, whether the copy it names carries learning beyond
// what exact serving handed out of the staged recording of that name
// (MockState.LearnedAfterEnd): the doors that name only the caller's copy
// (a spend for the staging, MarkMockAsUsed, a tree's delete door, the
// carry-over tier) read it against the staged recording, with its recorded
// noise, so a copy that carries only that is not flagged, and a copy that
// learned a noise path is. None says so while the learn step runs.
func TestEveryConsumeDoorSaysWhenItLearnedAfterTheLearnStepEnded(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	// Each staging's manager logs to lg, which each case sets: a door whose
	// recording the staging holds says nothing of one it does not.
	var lg *zap.Logger
	unstaged := func(logs *observer.ObservedLogs) []observer.LoggedEntry {
		return logs.FilterMessageSnippet("the test set's staging does not hold").All()
	}
	recorded := map[string][]string{"body.key": {}}
	// What the round keeps of the first cycle is on the recording as exact
	// serving hands it out: a copy carrying it learned nothing more.
	asServed := map[string][]string{"body.key": {}, "body.kept": {}}
	learnedNoise := map[string][]string{"body.key": {}, "body.kept": {}, "body.at": {}}
	find := func(t *testing.T, got []*models.Mock, name string) *models.Mock {
		t.Helper()
		for _, mk := range got {
			if mk.Name == name {
				return mk
			}
		}
		t.Fatalf("precondition: the read holds no %s", name)
		return nil
	}
	// Each stages its recording, ends the learn step when ended (the agent
	// does before the retry cycle's views are served), and reads the
	// recording as a parser would.
	type staging func(t *testing.T, ended bool) (*MockManager, *models.Mock)
	end := func(mm *MockManager, ended bool, name string) {
		if ended {
			mm.SetLearnEnded(true)
			mm.SetKeptLearning(learn.NewKeptLearning([]models.MockState{{Name: name, ReqBodyNoise: map[string][]string{"body.kept": {}}}}))
		}
	}
	pool := func(exact bool) staging {
		return func(t *testing.T, ended bool) (*MockManager, *models.Mock) {
			beat := beatMock("beat")
			beat.Spec.ReqBodyNoise = recorded
			mm := stagedManagerLogged(t, lg, nil, []*models.Mock{beat})
			end(mm, ended, "beat")
			if exact {
				mm.ServeExactPool("-", []string{"beat"})
			}
			un, _ := mm.GetUnFilteredMocks()
			return mm, find(t, un, "beat")
		}
	}
	own := func(exact bool) staging {
		return func(t *testing.T, ended bool) (*MockManager, *models.Mock) {
			beat := beatMock("beat")
			beat.Spec.ReqBodyNoise = recorded
			beat.TestModeInfo.Lifetime = models.LifetimePerTest
			mm := stagedManagerLogged(t, lg, []*models.Mock{beat}, nil)
			if !exact {
				// In the per-test tier, not the start-up one: staged
				// after a start-up cutoff before it, in its window.
				mm = NewMockManager(nil, nil, lg)
				t.Cleanup(mm.Close)
				mm.ResetForReplaySession()
				mm.SeedStartupCutoff(exactT0.Add(-time.Minute))
				mm.SetMocksWithWindow([]*models.Mock{beat}, nil, exactT0.Add(-time.Second), exactT0.Add(time.Second))
			}
			end(mm, ended, "beat")
			if exact {
				mm.ServeExactPool("-", nil)
				mm.ServeExactTest("T", []*models.Mock{beat})
			}
			if !exact {
				// Exact serving off: the per-test tree as staged.
				return mm, find(t, mm.treeValues(mm.filtered, ""), "beat")
			}
			got, _ := mm.GetFilteredMocks()
			return mm, find(t, got, "beat")
		}
	}
	// A per-test recording staged before every window: the start-up tier.
	startup := func(t *testing.T, ended bool) (*MockManager, *models.Mock) {
		beat := beatMock("beat")
		beat.Spec.ReqBodyNoise = recorded
		beat.TestModeInfo.Lifetime = models.LifetimePerTest
		mm := stagedManagerLogged(t, lg, []*models.Mock{beat}, nil)
		end(mm, ended, "beat")
		return mm, find(t, mm.treeValues(mm.startup, ""), "beat")
	}
	carry := func(t *testing.T, ended bool) (*MockManager, *models.Mock) {
		registerSends(t)
		send := brokerMockAt("send-gap", pulsarKind, "SEND", 1500)
		send.Spec.ResTimestampMock = pms(2050)
		send.Spec.ReqBodyNoise = recorded
		mm := NewMockManager(nil, nil, lg)
		t.Cleanup(mm.Close)
		mm.ResetForReplaySession()
		mm.SeedStartupCutoff(winStart(1))
		mm.SeedRecordedWindows(windowsOf("test-1", "test-2"))
		mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
		mm.SetMocksWithWindow([]*models.Mock{inFlightMock("own-2", 2010, 2011), send}, nil, winStart(2), winEnd(2))
		end(mm, ended, "send-gap")
		got, _ := mm.GetCarryOverMocks()
		return mm, find(t, got, "send-gap")
	}
	mark := func(mm *MockManager) func(models.Mock) bool { return mm.MarkMockAsUsed }
	delUn := func(mm *MockManager) func(models.Mock) bool { return mm.DeleteUnFilteredMock }
	delF := func(mm *MockManager) func(models.Mock) bool { return mm.DeleteFilteredMock }
	for _, door := range []struct {
		name    string
		staged  staging
		consume func(mm *MockManager) func(models.Mock) bool
	}{
		{"MarkMockAsUsed, a pool recording", pool(true), mark},
		{"DeleteUnFilteredMock, spent with no test open", pool(true), delUn},
		{"DeleteFilteredMock, the open test's own", own(true), delF},
		{"MarkMockAsUsed, exact serving off", pool(false), mark},
		{"DeleteUnFilteredMock, exact serving off", pool(false), delUn},
		{"DeleteFilteredMock, exact serving off", own(false), delF},
		{"DeleteFilteredMock, the start-up tier", startup, delF},
		{"DeleteFilteredMock, the carry-over tier", carry, delF},
	} {
		for _, tc := range []struct {
			name  string
			ended bool
			noise map[string][]string
			want  bool
		}{
			{"the learn step runs, and learns", false, learnedNoise, false},
			{"ended, the recorded and kept noise only", true, asServed, false},
			{"ended, the recorded noise only", true, recorded, false},
			{"ended, a learned noise path", true, learnedNoise, true},
		} {
			t.Run(door.name+"/"+tc.name, func(t *testing.T) {
				core, logs := observer.New(zap.WarnLevel)
				lg = zap.New(core)
				mm, got := door.staged(t, tc.ended)
				c := *got
				c.Spec.ReqBodyNoise = tc.noise
				if !door.consume(mm)(c) {
					t.Fatal("precondition: the door did not consume the copy")
				}
				var st *models.MockState
				for _, s := range mm.GetConsumedMocks() {
					if s.Name == got.Name {
						s := s
						st = &s
					}
				}
				if st == nil {
					t.Fatal("the consume is not reported")
				}
				if st.LearnedAfterEnd != tc.want {
					t.Fatalf("LearnedAfterEnd = %v, want %v", st.LearnedAfterEnd, tc.want)
				}
				if said := unstaged(logs); len(said) != 0 {
					t.Fatalf("the door read %s as a recording the staging does not hold: %v", got.Name, said[0].ContextMap())
				}
			})
		}
	}
	// A recording the staging does not hold (none of its trees has its
	// name), consumed after the learn step ended: every noise path it
	// carries reads as learned then, its recorded noise too (fail closed),
	// and a Warn names it, once in the staging.
	t.Run("a recording the staging does not hold", func(t *testing.T) {
		core, logs := observer.New(zap.WarnLevel)
		lg = zap.New(core)
		mm, _ := pool(false)(t, true)
		ghost := *beatMock("ghost")
		ghost.Spec.ReqBodyNoise = recorded
		for i := 0; i < 2; i++ {
			if !mm.MarkMockAsUsed(ghost) {
				t.Fatal("precondition: the door did not consume the copy")
			}
			var st *models.MockState
			for _, s := range mm.GetConsumedMocks() {
				if s.Name == "ghost" {
					s := s
					st = &s
				}
			}
			if st == nil || !st.LearnedAfterEnd {
				t.Fatalf("consume %d: %+v; want it to say it learned after the end", i+1, st)
			}
		}
		said := unstaged(logs)
		if len(said) != 1 || said[0].ContextMap()["mock"] != "ghost" {
			t.Fatalf("the Warns %v; want one, naming ghost", said)
		}
	})
}

// Two stagings that both file a matcher's learned copy of a tree recording
// of the same name, which holds different recordings in each (mock names
// repeat across sets, mock-0..N): the second staging's retry cycle is served
// its own recording as staged (the round keeps no learning here), never the
// copy its first cycle filed, which carries first-cycle learning the round
// does not write, nor the first staging's recording, whose file carries
// noise the second's does not (each a false green). A staging's filings
// replace an earlier staging's bases (filedBases.note's per-staging reset),
// so the base the retry cycle restores is the second staging's own: the row
// fails when an earlier staging's base survives the staging's move.
func TestASecondStagingsFilingIsNotServedInItsRetryCycle(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	beatAnswering := func(resp string) *models.Mock {
		m := beatMock("beat")
		m.Spec.HTTPResp.Body = resp
		m.Spec.HTTPResp.Header["Content-Length"] = strconv.Itoa(len(resp))
		return m
	}
	file := func(mm *MockManager) {
		t.Helper()
		var tree *models.Mock
		un, _ := mm.GetUnFilteredMocks()
		for _, mk := range un {
			if mk.Name == "beat" {
				tree = mk
			}
		}
		if tree == nil {
			t.Fatal("precondition: the pool holds no beat")
		}
		filed := *tree
		filed.TestModeInfo.IsFiltered = false
		filed.Spec.ReqBodyNoise = map[string][]string{"body.at": {}}
		if !mm.UpdateUnFilteredMock(tree, &filed) {
			t.Fatal("precondition: the matcher's copy is not filed")
		}
	}
	const two = `200 {"set":"two"}`
	// Staging 1: its beat answers "one", and its file declares the "at"
	// noise; its first cycle files a learned copy.
	one := beatAnswering(`{"set":"one"}`)
	one.Spec.ReqBodyNoise = map[string][]string{"body.at": {}}
	mm := stagedManager(t, nil, []*models.Mock{one})
	mm.ServeExactPool("-", []string{"beat"})
	file(mm)
	// Staging 2: the learn step taken up again, the set staged afresh with a
	// beat of the same name that answers "two" and declares no noise; its
	// first cycle files a learned copy too.
	mm.SetLearnEnded(false)
	mm.SetMocksWithWindow(nil, []*models.Mock{beatAnswering(`{"set":"two"}`)}, models.BaseTime, time.Now())
	mm.ServeExactPool("-", []string{"beat"})
	file(mm)
	if got := beatAt(t, mm, "t-2"); got != two {
		t.Fatalf("precondition: staging 2's first cycle answers through what its matcher filed: %s", got)
	}
	// Staging 2's retry cycle, the round keeping no learning.
	mm.SetLearnEnded(true)
	mm.ServeExactPool("-", []string{"beat"})
	if got := beatAt(t, mm, "t-3"); strings.HasPrefix(got, "200 ") {
		t.Fatalf("staging 2's retry cycle answered a drifted call %s through noise its own recording does not carry (a false green)", got)
	}
	if got := beatAt(t, mm, "t-1"); got != two {
		t.Fatalf("staging 2's retry cycle answered the recorded call %s; want staging 2's own recording %s", got, two)
	}
}
