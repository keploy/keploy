package mock

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func startupRuleRun(t *testing.T, mappingsYAML string, windows []models.ScopeWindow, consumed []models.MockState) (int, *Receipt, *observer.ObservedLogs) {
	t.Helper()
	provenOn(t)
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Mongo\nname: mock-0\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte(mappingsYAML), 0o644))
	core, logs := observer.New(zapcore.DebugLevel)
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	instr.consumedMocks, instr.windows = consumed, windows
	cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	_ = New(zap.New(core), instr, named("mock-0", "mock-1", "mock-create-idx", "mock-list-idx"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background())
	return utils.ErrCode, mustReceipt(t, keployDir), logs
}

// TestStartupBackgroundRuleRequiredOncePerRunAndSummaryLine proves Part (2b)'s
// replay contract:
//  1. A declared startup-capable background WRITE (createIndexes, Place: "",
//     Optional: false) is required once per run (not tied to a specific test):
//     if absent from the run, replay fails closed with FailedByRuleNotMade /
//     RULE NOT MADE.
//  2. When seen once in the run (even if TestA did not run and only TestB ran),
//     replay passes and emits legible RULE USED and RULES summary lines.
//  3. A declared startup-capable background READ (listIndexes, Read: true) is
//     served, not required, and reported as served-only in RULE USED.
func TestStartupBackgroundRuleRequiredOncePerRunAndSummaryLine(t *testing.T) {
	const mappings = `version: api.keploy.io/v1beta1
kind: TestMocksMapping
test_set_id: set
tests:
    - id: o::TestA
      dir: o
      mocks:
        - name: mock-0
    - id: o::TestB
      dir: o
      mocks:
        - name: mock-1
worker:
    - name: mock-create-idx
      rule: startup-indexes@12345678
      series: startup-indexes/-/1
      once: true
    - name: mock-list-idx
      rule: startup-indexes@12345678
      series: startup-indexes/-/2
      read: true
`
	at := time.Now()
	onlyTestB := []models.ScopeWindow{
		{Name: "o::TestB", Start: at, End: at.Add(time.Second), Outcome: models.ScopeOutcomePassed},
	}

	t.Run("missing_declared_background_write_fails_closed", func(t *testing.T) {
		code, r, logs := startupRuleRun(t, mappings, onlyTestB, []models.MockState{{Name: "mock-1"}})
		require.NotEqual(t, 0, code, "missing declared startup background write must fail closed")
		require.Equal(t, FailedByRuleNotMade, r.FailedBy)
		require.Len(t, r.RequiredNotMade, 1)
		require.Equal(t, "startup-indexes/-/1", r.RequiredNotMade[0].Series)
		require.Equal(t, "startup-indexes@12345678", r.RequiredNotMade[0].Rule)
		require.Empty(t, r.RequiredNotMade[0].Trails, "background write is run-level, not tied to a test")
		var sawNotMade, sawRuleUsed bool
		for _, entry := range logs.All() {
			if strings.Contains(entry.Message, "RULE NOT MADE") && strings.Contains(entry.Message, "startup-indexes/-/1") {
				sawNotMade = true
			}
			if strings.Contains(entry.Message, "RULE USED startup-indexes@12345678") && strings.Contains(entry.Message, "not made 1") {
				sawRuleUsed = true
			}
		}
		require.True(t, sawNotMade, "expected RULE NOT MADE error line")
		require.True(t, sawRuleUsed, "expected RULE USED summary line showing not made 1")
	})

	t.Run("declared_background_write_seen_once_passes_and_shows_in_summary", func(t *testing.T) {
		// Only TestB ran; createIndexes was seen once; read-only listIndexes was not called.
		code, r, logs := startupRuleRun(t, mappings, onlyTestB, []models.MockState{
			{Name: "mock-1"},
			{Name: "mock-create-idx"},
		})
		require.Equal(t, 0, code, "declared startup background write seen once must pass even when TestA did not run")
		require.Empty(t, r.FailedBy)
		require.Empty(t, r.RequiredNotMade)
		var sawRuleUsed, sawRules bool
		for _, entry := range logs.All() {
			if strings.Contains(entry.Message, "RULE USED startup-indexes@12345678") &&
				strings.Contains(entry.Message, "required 1: made 1, not made 0") &&
				strings.Contains(entry.Message, "served, not required 1") {
				sawRuleUsed = true
			}
			if strings.HasPrefix(entry.Message, "RULES:") && strings.Contains(entry.Message, "required series not made: 0") {
				sawRules = true
			}
		}
		require.True(t, sawRuleUsed, "run summary must include legible RULE USED line for startup-indexes")
		require.True(t, sawRules, "run summary must include RULES line")
	})
}

// TestHasDeclaredStartupRuleIgnoresPlainStartupEntries proves B3 at the
// mapping reader: plain recorded startup: entries, trailing rules, and
// non-startup declared background rules (Place != "") return false (so
// parallel-worker replay is never refused), while a worker: entry placed at
// set start by a declared startup: true rule (Rule != "" && Trails == "" &&
// Place == "") returns true.
func TestHasDeclaredStartupRuleIgnoresPlainStartupEntries(t *testing.T) {
	require.False(t, hasDeclaredStartupRule(nil))
	require.False(t, hasDeclaredStartupRule(&models.Mapping{
		Startup: []models.MockEntry{{Name: "mock-boot-1"}, {Name: "mock-boot-2"}},
	}), "plain recorded startup: entries must not trigger declared startup rule refusal")
	require.False(t, hasDeclaredStartupRule(&models.Mapping{
		Startup: []models.MockEntry{{Name: "mock-boot-1"}},
		Worker:  []models.MockEntry{{Name: "mock-bg-1", Rule: "bg-poll@12345678", Series: "bg-poll/-/1", Place: ".::TestA"}},
	}), "non-startup declared background rule (bound to Place) must not trigger declared startup rule refusal")
	require.False(t, hasDeclaredStartupRule(&models.Mapping{
		Worker: []models.MockEntry{{Name: "mock-tr-1", Rule: "tr-clean@12345678", Series: "tr-clean/TestA/1", Trails: ".::TestA"}},
	}), "declared trailing rule must not trigger declared startup rule refusal")
	require.False(t, hasDeclaredStartupRule(&models.Mapping{
		Worker: []models.MockEntry{{Name: "mock-w-1", Rule: "", Place: ""}},
	}), "unmarked worker entry without a declared Rule must not trigger declared startup rule refusal")
	require.True(t, hasDeclaredStartupRule(&models.Mapping{
		Worker: []models.MockEntry{{Name: "mock-idx-1", Rule: "startup-index-builds-create@ac330c15", Series: "startup-index-builds-create/-/1", Place: ""}},
	}), "declared startup: true rule entry in worker: (Place empty) must report true")
}

// TestWithoutSourceBoundWorkerSeriesOmitsClockChainsInUnsetMode proves R2's
// unset-mode contract: worker: series carrying declared dynamic Sources
// (kind: clock / kind: minted, evaluated only under KEPLOY_PROVEN=1) are
// omitted whole (every sibling in the Series, such as a once: GET->GET->PUT
// chain) from unset-mode background routing so unset replay never serves
// earlier chain steps only to fail closed on live clock fields, while
// source-free worker: rules and all startup: entries remain active.
func TestWithoutSourceBoundWorkerSeriesOmitsClockChainsInUnsetMode(t *testing.T) {
	mp := &models.Mapping{
		Startup: []models.MockEntry{
			{Name: "mock-boot-1"},
		},
		Worker: []models.MockEntry{
			{Name: "mock-idx-list", Rule: "startup-index-builds-list@11111111", Series: "startup-index-builds-list/-/1"},
			{Name: "mock-idx-create", Rule: "startup-index-builds-create@22222222", Series: "startup-index-builds-create/-/1"},
			{Name: "mock-ui-del", Rule: "app-ui-joins-delete-emptied@33333333", Series: "app-ui-joins-delete-emptied/-/1"},
			// 3-step once: chain whose PUT carries declared clock Sources.
			{Name: "mock-pat-get1", Rule: "pat-last-use-touch@44444444", Series: "pat-last-use-touch/-/1", Once: true},
			{Name: "mock-pat-get2", Rule: "pat-last-use-touch@44444444", Series: "pat-last-use-touch/-/1", Once: true},
			{
				Name:    "mock-pat-put",
				Rule:    "pat-last-use-touch@44444444",
				Series:  "pat-last-use-touch/-/1",
				Once:    true,
				Sources: []models.DeclaredSource{{Kind: "clock", Field: "body.data.lastUsedAt"}},
			},
			// Standalone source-bearing worker entry with no Series.
			{
				Name:    "mock-poll-clock",
				Rule:    "ptw-reconcile-poll@55555555",
				Sources: []models.DeclaredSource{{Kind: "clock", Field: "body.cmd.update.$set.claimedAt.$numberLong"}},
			},
		},
	}
	filtered := withoutSourceBoundWorkerSeries(mp)
	require.Len(t, filtered.Startup, 1)
	require.Equal(t, "mock-boot-1", filtered.Startup[0].Name)

	var kept []string
	for _, e := range filtered.Worker {
		kept = append(kept, e.Name)
	}
	require.Equal(t, []string{"mock-idx-list", "mock-idx-create", "mock-ui-del"}, kept,
		"source-bound worker series and standalone source-bearing worker entries must be omitted in unset mode")
	// Original mapping must not be mutated.
	require.Len(t, mp.Worker, 7)
}
