package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// testSetStore is stagingStore's recording behind a real mock manager.
func testSetStore(t *testing.T) (*Agent, *proxyPkg.MockManager) {
	t.Helper()
	t.Setenv("KEPLOY_STRICT_MOCK_WINDOW", "1")
	mm := proxyPkg.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	a := diskAgent(t, &stageProxy{mm: mm},
		stageMock("boot-config", "/config", stageT0.Add(-20*time.Second)),
		stageMock("boot-auth", "/auth", stageT0.Add(-5*time.Second)),
		stageMock("test-a", "/q", stageT0.Add(time.Second)),
		stageMock("test-b", "/q", stageT0.Add(time.Minute)),
		stageMock("poll-later", "/poll", stageT0.Add(time.Hour)))
	return a, mm
}

func startupNames(t *testing.T, mm *proxyPkg.MockManager) []string {
	t.Helper()
	startup, err := mm.GetStartupMocks()
	if err != nil {
		t.Fatal(err)
	}
	return sortedNames(namesOf(startup))
}

// An agent docker compose starts again mid-set is begun again (the session
// re-sends the set's begin), then sent the running test's filter. The set's
// startup cutoff is its first recorded test, so only the app's startup
// traffic is filed as such. Sent the test's filter alone, as before the set
// had a begin of its own, the agent took the running test's start for the
// cutoff and filed every earlier test's mocks as app-startup traffic: test-a
// here, most of the recording on a long one.
func TestARestartedAgentBegunAgainFilesOnlyTheStartupTraffic(t *testing.T) {
	a, mm := testSetStore(t)
	ctx := context.Background()
	if err := a.BeginTestSet(ctx, models.TestSetBegin{TestSetID: "test-set-0", FirstRecordedTestStart: stageT0, StrictMockWindow: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.UpdateMockParams(ctx, models.MockFilterParams{
		AfterTime: stageT0.Add(time.Minute), BeforeTime: stageT0.Add(time.Minute + 2*time.Second), StrictMockWindow: true,
	}); err != nil {
		t.Fatal(err)
	}
	if got, want := startupNames(t, mm), []string{"boot-auth", "boot-config"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("filed %v as app-startup traffic; want only %v", got, want)
	}
	if got := mm.FirstTestWindowStart(); !got.Equal(stageT0) {
		t.Fatalf("startup cutoff %v; want the set's first recorded test %v", got, stageT0)
	}
}

// A set's end lets go of the mocks stored for it, the on-disk store's file
// included; what the proxy serves stays staged until the next set begins, so
// an application kept alive across the boundary is answered as before. A
// filter or a begin needs the next set's mocks stored.
func TestEndTestSetLetsGoOfTheStoredMocksOnly(t *testing.T) {
	a, mm := testSetStore(t)
	ctx := context.Background()
	storage, err := a.storedMocks()
	if err != nil {
		t.Fatal(err)
	}
	disk := storage.diskMocks
	if err := a.BeginTestSet(ctx, models.TestSetBegin{TestSetID: "test-set-0", FirstRecordedTestStart: stageT0, StrictMockWindow: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.EndTestSet(ctx, "test-set-0"); err != nil {
		t.Fatal(err)
	}
	if _, err := disk.LoadAll(); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("the ended set's on-disk store still reads: %v", err)
	}
	if stats, _ := a.MockStats(ctx); stats.Loaded != 0 {
		t.Fatalf("the agent reports %d mocks loaded after the set ended, want 0", stats.Loaded)
	}
	if got, want := startupNames(t, mm), []string{"boot-auth", "boot-config"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the proxy serves %v after the set ended; want the set's pool %v until the next begins", got, want)
	}
	if err := a.UpdateMockParams(ctx, models.MockFilterParams{AfterTime: stageT0, BeforeTime: stageT0.Add(time.Second)}); err == nil {
		t.Fatal("a test filter after the set ended read mocks no set stored")
	}
	if err := a.BeginTestSet(ctx, models.TestSetBegin{TestSetID: "test-set-1"}); err == nil {
		t.Fatal("a set began from mocks no set stored")
	}
	// Ending a set with nothing stored is no error: the replayer ends every
	// set it began, whatever the agent holds.
	if err := a.EndTestSet(ctx, "test-set-1"); err != nil {
		t.Fatal(err)
	}
}
