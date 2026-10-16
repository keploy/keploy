package replay

import (
	"context"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func recordedSets(context.Context) ([]string, error) {
	return []string{"test-set-2", "test-set-1", "test-set-10"}, nil
}

// A test set the run is asked for that is not recorded is refused, naming it,
// and the run exits non-zero (P189): under the proven switch whoever named it,
// and with the switch off when the command line named it (-t). It was dropped
// silently: -t "test-set-1,test-set-3" ran test-set-1 alone and passed, and a
// selection naming no recorded set at all ran every set.
func TestATestSetThatIsNotRecordedIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name            string
		switchOn        bool
		fromCommandLine bool
	}{
		{"switch on, -t", true, true},
		{"switch on, keploy.yml", true, false},
		{"switch off, -t", false, true},
	} {
		if tc.switchOn {
			t.Setenv(provenswitch.Env, "1")
		} else {
			t.Setenv(provenswitch.Env, "")
		}
		for _, selected := range [][]string{{"test-set-1", "test-set-3"}, {"test-set-3"}} {
			conf := &config.Config{}
			config.SetSelectedTests(conf, selected)
			conf.Test.SelectedFromCommandLine = tc.fromCommandLine
			r := NewReplayer(zap.NewNop(), startTestDB{list: recordedSets}, nil, nil, nil, nil, startTelemetry{}, startInstrumentation{}, nil, conf).(*Replayer)
			sets, err := r.GetSelectedTestSets(context.Background())
			if err == nil || !strings.Contains(err.Error(), "test-set-3") {
				t.Fatalf("%s, %q: selected %q, err %v; want test-set-3 refused by name", tc.name, selected, sets, err)
			}

			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			if err := r.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "test-set-3") {
				t.Fatalf("%s, %q: Start returned %v; want test-set-3 refused by name", tc.name, selected, err)
			}
			if utils.ErrCode != 1 {
				t.Fatalf("%s, %q: exit code %d, want 1", tc.name, selected, utils.ErrCode)
			}
		}
	}
}

// With the switch off, a name keploy.yml's test.selectedTests inherited that
// is not recorded is left out, with a Warn naming it, so a stale keploy.yml
// does not turn a run that passed red on upgrade: the rest of the selection is
// replayed, and every recorded set when it names none.
func TestAStaleKeployYmlSelectionIsWarnedAndLeftOut(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	for _, tc := range []struct {
		selected []string
		want     []string
	}{
		{[]string{"test-set-1", "test-set-3"}, []string{"test-set-1"}},
		{[]string{"test-set-3"}, []string{"test-set-1", "test-set-2", "test-set-10"}},
	} {
		core, logs := observer.New(zap.WarnLevel)
		conf := &config.Config{}
		conf.Test.SelectedTests = map[string][]string{}
		for _, name := range tc.selected {
			conf.Test.SelectedTests[name] = nil
		}
		r := NewReplayer(zap.New(core), startTestDB{list: recordedSets}, nil, nil, nil, nil, startTelemetry{}, startInstrumentation{}, nil, conf).(*Replayer)
		got, err := r.GetSelectedTestSets(context.Background())
		if err != nil || !slices.Equal(got, tc.want) {
			t.Fatalf("keploy.yml %q: %q, %v; want %q", tc.selected, got, err, tc.want)
		}
		warned := logs.FilterMessageSnippet("not recorded").All()
		if len(warned) != 1 || !slices.Equal(warned[0].ContextMap()["notRecorded"].([]interface{}), []interface{}{"test-set-3"}) {
			t.Fatalf("keploy.yml %q: warned %v, want one Warn naming test-set-3", tc.selected, logs.All())
		}
	}
}

// The test sets named, all of them, in their natural order; none named is
// every recorded set.
func TestTheSelectedTestSetsAreTheOnesNamed(t *testing.T) {
	for _, tc := range []struct {
		selected []string
		want     []string
	}{
		{[]string{"test-set-10", "test-set-1"}, []string{"test-set-1", "test-set-10"}},
		{nil, []string{"test-set-1", "test-set-2", "test-set-10"}},
	} {
		conf := &config.Config{}
		config.SetSelectedTests(conf, tc.selected)
		r := NewReplayer(zap.NewNop(), startTestDB{list: recordedSets}, nil, nil, nil, nil, startTelemetry{}, startInstrumentation{}, nil, conf).(*Replayer)
		got, err := r.GetSelectedTestSets(context.Background())
		if err != nil || !slices.Equal(got, tc.want) {
			t.Fatalf("-t %q: %q, %v; want %q", tc.selected, got, err, tc.want)
		}
	}
}

// keploy.yml's test.selectedTests reaches the replay with its keys lowercased
// (viper): {MySet: [test-1]} is {myset: [test-1]}. It names the recorded set
// MySet, and the selection is keyed by it again; a name two recorded sets
// could be names neither.
func TestAKeployYmlNameInAnotherCaseNamesTheRecordedSet(t *testing.T) {
	conf := &config.Config{}
	conf.Test.SelectedTests = map[string][]string{"myset": {"test-1"}}
	list := func(context.Context) ([]string, error) { return []string{"MySet", "test-set-1"}, nil }
	r := NewReplayer(zap.NewNop(), startTestDB{list: list}, nil, nil, nil, nil, startTelemetry{}, startInstrumentation{}, nil, conf).(*Replayer)
	if got, err := r.GetSelectedTestSets(context.Background()); err != nil || !slices.Equal(got, []string{"MySet"}) {
		t.Fatalf("keploy.yml {MySet: [test-1]}: %q, %v; want [MySet]", got, err)
	}
	if got := conf.Test.SelectedTests["MySet"]; !slices.Equal(got, []string{"test-1"}) {
		t.Fatalf("the selection is not keyed by the recorded set: %q", conf.Test.SelectedTests)
	}

	conf.Test.SelectedTests = map[string][]string{"myset": {}}
	conf.Test.SelectedFromCommandLine = true
	list = func(context.Context) ([]string, error) { return []string{"MySet", "MYSET"}, nil }
	r = NewReplayer(zap.NewNop(), startTestDB{list: list}, nil, nil, nil, nil, startTelemetry{}, startInstrumentation{}, nil, conf).(*Replayer)
	if got, err := r.GetSelectedTestSets(context.Background()); err == nil {
		t.Fatalf("a name two recorded sets could be selected %q", got)
	}
}
