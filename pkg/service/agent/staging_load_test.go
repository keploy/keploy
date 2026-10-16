package agent

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// stagingStore is a recording whose per-test mocks sit in the agent's on-disk
// store: two from the app's startup, before the set's first test at stageT0,
// the two tests' own, and a poll recorded an hour into the recording.
func stagingStore(t *testing.T) (*Agent, *recordingProxy, *observer.ObservedLogs) {
	t.Helper()
	t.Setenv("KEPLOY_STRICT_MOCK_WINDOW", "1")
	p := &recordingProxy{}
	a := diskAgent(t, p,
		stageMock("boot-config", "/config", stageT0.Add(-20*time.Second)),
		stageMock("boot-auth", "/auth", stageT0.Add(-5*time.Second)),
		stageMock("test-a", "/q", stageT0.Add(time.Second)),
		stageMock("test-b", "/q", stageT0.Add(time.Minute)),
		stageMock("poll-later", "/poll", stageT0.Add(time.Hour)))
	core, logs := observer.New(zap.DebugLevel)
	a.logger = zap.New(core)
	return a, p, logs
}

func sortedNames(names []string) []string {
	out := append([]string(nil), names...)
	sort.Strings(out)
	return out
}

func loadedFromDisk(t *testing.T, logs *observer.ObservedLogs) []int64 {
	t.Helper()
	var out []int64
	for _, e := range logs.FilterMessage("agent mock residency: loaded per-test set from disk").All() {
		n, _ := e.ContextMap()["loadedFromDisk"].(int64)
		out = append(out, n)
	}
	return out
}

// A set begins, before the app starts, with only the mocks recorded before the
// set's first test read from the on-disk store: the app's startup traffic. A
// set used to be staged with the window 2000-01-01 to now, and reading that
// window brought every mock of the recording into the agent's memory only to
// file it as startup traffic: 81,738 of them OOM-killed a 1200Mi agent on a
// prod recording.
func TestBeginTestSetReadsOnlyTheStartupMocksFromDisk(t *testing.T) {
	a, p, logs := stagingStore(t)
	err := a.BeginTestSet(context.Background(), models.TestSetBegin{
		FirstRecordedTestStart: stageT0,
		StrictMockWindow:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := loadedFromDisk(t, logs); !reflect.DeepEqual(got, []int64{2}) {
		t.Fatalf("the set's begin read %v mocks from disk; want the 2 recorded before the first test", got)
	}
	if len(p.begun) != 1 {
		t.Fatalf("began %d times, want once", len(p.begun))
	}
	if got, want := sortedNames(p.begun[0][0]), []string{"boot-auth", "boot-config"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the set began with %v; want the app-startup mocks %v", got, want)
	}
	if !p.firsts[0].Equal(stageT0) {
		t.Fatalf("the set began with cutoff %v, want its first recorded test %v", p.firsts[0], stageT0)
	}
}

// The mapping's startup mocks are the set's too, wherever they were recorded.
func TestBeginTestSetReadsTheMappingsStartupMocks(t *testing.T) {
	a, p, logs := stagingStore(t)
	err := a.BeginTestSet(context.Background(), models.TestSetBegin{
		FirstRecordedTestStart: stageT0,
		StartupMockNames:       []string{"test-a", "boot-auth"},
		StrictMockWindow:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := loadedFromDisk(t, logs); !reflect.DeepEqual(got, []int64{3}) {
		t.Fatalf("the set's begin read %v mocks from disk; want the 2 recorded before the first test and test-a", got)
	}
	if got, want := sortedNames(p.begun[0][0]), []string{"boot-auth", "boot-config", "test-a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the set began with %v; want %v", got, want)
	}
}

// A test's own call is unchanged: its window plus what was recorded before the
// set's first test.
func TestATestStillReadsItsWindowAndTheStartupMocks(t *testing.T) {
	a, p, logs := stagingStore(t)
	err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
		AfterTime:        stageT0,
		BeforeTime:       stageT0.Add(2 * time.Second),
		StrictMockWindow: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := loadedFromDisk(t, logs); !reflect.DeepEqual(got, []int64{3}) {
		t.Fatalf("the test's call read %v mocks from disk; want its own and the 2 startup mocks", got)
	}
	if got, want := sortedNames(p.windowed[0][0]), []string{"boot-auth", "boot-config", "test-a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the test's call staged %v; want %v", got, want)
	}
}

// A set whose tests carry no request time has no first test to cut at, so it
// begins with every mock; and so does a set replayed with the lax window, whose
// tests may be served any mock of the recording.
func TestBeginTestSetWithoutACutReadsEveryMock(t *testing.T) {
	for _, tc := range []struct {
		name  string
		begin models.TestSetBegin
	}{
		{"no recorded test time", models.TestSetBegin{StrictMockWindow: true}},
		{"lax", models.TestSetBegin{FirstRecordedTestStart: stageT0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, p, logs := stagingStore(t)
			if tc.name == "lax" {
				t.Setenv("KEPLOY_STRICT_MOCK_WINDOW", "0")
			}
			if err := a.BeginTestSet(context.Background(), tc.begin); err != nil {
				t.Fatal(err)
			}
			if got := loadedFromDisk(t, logs); !reflect.DeepEqual(got, []int64{5}) {
				t.Fatalf("the set's begin read %v mocks from disk; want all 5", got)
			}
			if len(p.begun[0][0]) != 5 {
				t.Fatalf("the set began with %v; want all 5", p.begun[0][0])
			}
		})
	}
}
