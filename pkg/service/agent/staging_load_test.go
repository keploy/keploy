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

// The set's staging call, sent before the app starts with the window BaseTime to
// now, reads from the on-disk store only the mocks recorded before the set's
// first test. The proxy files whatever it is staged with as app-startup traffic,
// so reading the whole window brought every mock of the recording into the
// agent's memory: 81,738 of them OOM-killed a 1200Mi agent on a prod recording.
func TestStagingReadsOnlyTheStartupMocksFromDisk(t *testing.T) {
	a, p, logs := stagingStore(t)
	err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
		AfterTime:              models.BaseTime,
		BeforeTime:             stageT0.Add(2 * time.Hour),
		FirstRecordedTestStart: stageT0,
		StrictMockWindow:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := loadedFromDisk(t, logs); !reflect.DeepEqual(got, []int64{2}) {
		t.Fatalf("the staging call read %v mocks from disk; want the 2 recorded before the first test", got)
	}
	if len(p.windowed) != 1 {
		t.Fatalf("staged %d times, want once", len(p.windowed))
	}
	if got, want := sortedNames(p.windowed[0][0]), []string{"boot-auth", "boot-config"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the staging call staged %v; want the app-startup mocks %v", got, want)
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

// A set whose tests carry no request time has no first test to cut at, so its
// staging call reads the window it names.
func TestStagingWithoutARecordedTestTimeReadsItsWindow(t *testing.T) {
	a, p, logs := stagingStore(t)
	err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
		AfterTime:        models.BaseTime,
		BeforeTime:       stageT0.Add(2 * time.Hour),
		StrictMockWindow: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := loadedFromDisk(t, logs); !reflect.DeepEqual(got, []int64{5}) {
		t.Fatalf("the staging call read %v mocks from disk; want all 5 in its window", got)
	}
	if len(p.windowed[0][0]) != 5 {
		t.Fatalf("the staging call staged %v; want all 5", p.windowed[0][0])
	}
}
