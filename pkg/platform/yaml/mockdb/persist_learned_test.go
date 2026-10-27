package mockdb

import (
	"context"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// What the learn step learned is written onto the recording, said per mock,
// read back, and never added twice, in every storage format: noise in every
// part of a request, a variable's use, and its bind at the earlier call
// (carried by the later call's state), which turns that call's echo into the
// variable's bind and drops the noise learned at the place before it was
// known to flow on.
func TestPersistLearnedWritesNoiseAndVariables(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, f := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(f), func(t *testing.T) {
			dir := t.TempDir()
			ys := NewWithFormat(zap.NewNop(), dir, "mocks", f)
			ctx := context.Background()
			set := "test-set-0"
			if err := ys.InsertMock(ctx, noiseTestMock(`{"key":"k-1"}`), set); err != nil {
				t.Fatal(err)
			}
			if err := ys.InsertMock(ctx, noiseTestMock(`{"other":"o"}`), set); err != nil {
				t.Fatal(err)
			}
			if err := ys.Close(); err != nil {
				t.Fatal(err)
			}
			states := map[string]models.MockState{
				"mock-0": {Name: "mock-0",
					ReqBodyNoise: map[string][]string{"header.x-trace": {}, "url.path.1": {}, "body.key": {}},
					Variables:    models.MockVariables{"key": {Recorded: "k-1", Bind: "body.key", Answer: []string{"body.idempotency_key"}, Echo: true}}},
				"mock-1": {Name: "mock-1",
					Variables: models.MockVariables{"key": {Recorded: "k-1", Use: []string{"url.query.key"}}},
					Binds:     map[string]models.MockVariables{"mock-0": {"key": {Recorded: "k-1", Bind: "body.key"}}}},
			}
			added, err := ys.PersistLearned(ctx, set, states)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string][]string{
				"mock-0": {"body.key (variable key: bind)", "header.x-trace (noise)", "url.path.1 (noise)"},
				"mock-1": {"url.query.key (variable key: use)"},
			}
			if !reflect.DeepEqual(added, want) {
				t.Fatalf("added %v, want %v", added, want)
			}
			byName := map[string]*models.Mock{}
			mocks, err := ys.GetUnFilteredMocks(ctx, set, models.BaseTime, models.BaseTime.AddDate(100, 0, 0), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			filtered, err := ys.GetFilteredMocks(ctx, set, models.BaseTime, models.BaseTime.AddDate(100, 0, 0), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range append(mocks, filtered...) {
				byName[m.Name] = m
			}
			m0, m1 := byName["mock-0"], byName["mock-1"]
			if m0 == nil || m1 == nil {
				t.Fatalf("mocks read back %v", byName)
			}
			for _, m := range []*models.Mock{m0, m1} {
				if s := models.VariableAboutState("key", m.Spec.Variables["key"]); s != models.AboutLearned {
					t.Fatalf("%s's learned variable reads back as %v", m.Name, s)
				}
			}
			if want := (models.MockVariables{"key": {Recorded: "k-1", Bind: "body.key", Answer: []string{"body.idempotency_key"}}}); !reflect.DeepEqual(withoutAbout(m0.Spec.Variables), want) {
				t.Fatalf("mock-0's variables read back %+v: another call uses the value, so it is no echo", m0.Spec.Variables)
			}
			if !reflect.DeepEqual(m0.Spec.ReqBodyNoise, map[string][]string{"header.x-trace": {}, "url.path.1": {}}) {
				t.Fatalf("mock-0's noise read back %v: a variable's place is never noise", m0.Spec.ReqBodyNoise)
			}
			if want := (models.MockVariables{"key": {Recorded: "k-1", Use: []string{"url.query.key"}}}); !reflect.DeepEqual(withoutAbout(m1.Spec.Variables), want) {
				t.Fatalf("mock-1's variables read back %+v", m1.Spec.Variables)
			}
			again, err := ys.PersistLearned(ctx, set, states)
			if err != nil || len(again) != 0 {
				t.Fatalf("what a mock already carries is not added again: %v %v", again, err)
			}
		})
	}
}

// A clock variable's reads (models.MockVariable.Clock: the instant and
// resolution its recording says, read where F saw it move by the lead) are
// written with its bind and read back as they were, in every storage format:
// a replay compares them to let two reads made in one second share a live
// value (FD-3).
func TestPersistLearnedKeepsAClockVariablesReads(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	reads := []models.ClockRead{
		{At: time.Date(2026, 10, 14, 10, 0, 0, 250e6, time.UTC), Res: time.Millisecond},
		{At: time.Date(0, 1, 1, 10, 0, 41, 0, time.UTC), Res: time.Second, Day: true},
	}
	for _, f := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(f), func(t *testing.T) {
			dir := t.TempDir()
			ys := NewWithFormat(zap.NewNop(), dir, "mocks", f)
			ctx := context.Background()
			set := "test-set-0"
			if err := ys.InsertMock(ctx, noiseTestMock(`{"at":"2026-10-14T10:00:00.250Z 10:00:41"}`), set); err != nil {
				t.Fatal(err)
			}
			if err := ys.Close(); err != nil {
				t.Fatal(err)
			}
			states := map[string]models.MockState{"mock-0": {Name: "mock-0",
				Variables: models.MockVariables{"at": {Recorded: "2026-10-14T10:00:00.250Z 10:00:41", Bind: "body.at", Clock: reads}}}}
			if _, err := ys.PersistLearned(ctx, set, states); err != nil {
				t.Fatal(err)
			}
			mocks, err := ys.GetUnFilteredMocks(ctx, set, models.BaseTime, models.BaseTime.AddDate(100, 0, 0), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			filtered, err := ys.GetFilteredMocks(ctx, set, models.BaseTime, models.BaseTime.AddDate(100, 0, 0), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			var got []models.ClockRead
			for _, m := range append(mocks, filtered...) {
				if m.Name == "mock-0" {
					got = m.Spec.Variables["at"].Clock
				}
			}
			if len(got) != len(reads) {
				t.Fatalf("the variable's reads read back as %v, want %v", got, reads)
			}
			for i := range reads {
				if !got[i].At.Equal(reads[i].At) || got[i].Res != reads[i].Res || got[i].Day != reads[i].Day {
					t.Errorf("read %d read back as %+v, want %+v", i, got[i], reads[i])
				}
			}
		})
	}
}

// Two declarations of one variable in one call are one: a bind from either,
// every use and answer, and an echo only when both say so.
func TestMergeVariable(t *testing.T) {
	got := mergeVariable(
		models.MockVariable{Recorded: "v", Bind: "body.id", Answer: []string{"body.id"}, Echo: true},
		models.MockVariable{Recorded: "v", Use: []string{"url.path[1]"}, Answer: []string{"header.x-id", "body.id"}},
	)
	want := models.MockVariable{Recorded: "v", Bind: "body.id", Use: []string{"url.path[1]"}, Answer: []string{"body.id", "header.x-id"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged %+v, want %+v", got, want)
	}
	if got := mergeVariable(models.MockVariable{}, models.MockVariable{Recorded: "v", Bind: "body.id", Echo: true}); !got.Echo {
		t.Fatal("an echo merged into nothing stays an echo")
	}
	// A clock read goes with its bind (models.MergeVariables).
	at := []models.ClockRead{{At: time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC), Res: time.Second}}
	if got := mergeVariable(models.MockVariable{Recorded: "v", Use: []string{"url.query.since"}}, models.MockVariable{Recorded: "v", Bind: "body.at", Clock: at}); !reflect.DeepEqual(got.Clock, at) {
		t.Fatalf("a clock read's bind merged into a use lost its reads: %v", got.Clock)
	}
	if got := mergeVariable(models.MockVariable{Recorded: "v", Bind: "body.at", Clock: at}, models.MockVariable{Recorded: "v", Bind: "body.at"}); got.Clock != nil {
		t.Fatal("a variable two binds declare is a clock read though one says it is not")
	}
	if got := mergeVariable(models.MockVariable{Recorded: "v", Bind: "body.at"}, models.MockVariable{Recorded: "v", Bind: "body.at", Clock: at}); got.Clock != nil {
		t.Fatal("a variable two binds declare is a clock read though the first says it is not")
	}
	if got := mergeVariable(models.MockVariable{Recorded: "v", Bind: "body.at"}, models.MockVariable{Recorded: "v", Use: []string{"url.query.since"}, Clock: at}); got.Clock != nil {
		t.Fatal("a use's mark made a bind a clock read")
	}
}

// With the switch off the noise writers are what they were: a mock that
// carries a variables block keeps every noise path learned, and the noise it
// had (the round-3 reviewer's repro).
func TestPersistMockNoiseSwitchOffKeepsNoiseAtAVariablePlace(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "")
	ctx := context.Background()
	set := "test-set-0"
	m := noiseTestMock(`{"key":"k-1","other":"o"}`)
	m.Name = "mock-0"
	m.Spec.Variables = models.MockVariables{"key": {Recorded: "k-1", Bind: "body.key"}}
	m.Spec.ReqBodyNoise = map[string][]string{"body.key": {}}
	if err := ys.InsertMock(ctx, m, set); err != nil {
		t.Fatal(err)
	}
	if err := ys.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ys.PersistMockNoise(ctx, set, map[string]models.MockState{
		"mock-0": {Name: "mock-0", ReqBodyNoise: map[string][]string{"body.other": {}}},
	}); err != nil {
		t.Fatal(err)
	}
	mocks, err := ys.GetUnFilteredMocks(ctx, set, models.BaseTime, models.BaseTime.AddDate(100, 0, 0), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := ys.GetFilteredMocks(ctx, set, models.BaseTime, models.BaseTime.AddDate(100, 0, 0), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string][]string
	for _, mk := range append(mocks, filtered...) {
		if mk.Name == "mock-0" {
			got = mk.Spec.ReqBodyNoise
		}
	}
	if want := map[string][]string{"body.key": {}, "body.other": {}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("switch off: noise written %v, want %v", got, want)
	}
}

// withoutAbout is vs with no variable's about: the decisions alone.
func withoutAbout(vs models.MockVariables) models.MockVariables {
	out := vs.Clone()
	for n, v := range out {
		v.About = nil
		out[n] = v
	}
	return out
}
