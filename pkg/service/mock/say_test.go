package mock

import (
	"context"
	"os"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type summingStore struct {
	countingStore
	got []Summary
}

func (s *summingStore) Summarize(sum Summary) { s.got = append(s.got, sum) }

func marked(e observer.LoggedEntry, mark zap.Field) bool {
	for _, f := range e.Context {
		if f.Key == mark.Key {
			return true
		}
	}
	return false
}

func TestRecordHandsItsSummaryToAStoreThatPrintsTheEnd(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  models.AppError
		want Summary
	}{
		{name: "kept", run: models.AppError{AppErrorType: models.ErrAppStopped}, want: Summary{Calls: 1, Kept: true}},
		{name: "tests failed", run: models.AppError{AppErrorType: models.ErrCommandError, ExitCode: 1}, want: Summary{Calls: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := &runnerInstr{composeInstr: newInstr(t, agentUpFromSetup, false, tc.run), mocks: []*models.Mock{mockAt("mock-0", runnerT0)}}
			cfg := instrConfig(base.composeInstr, utils.Native, "go test ./...")
			cfg.Path = t.TempDir()
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			core, logs := observer.New(zap.InfoLevel)
			store := &summingStore{}
			if err := New(zap.New(core), base, stubMockDB{}, nil, store, nil, cfg).Record(context.Background()); err != nil {
				t.Fatalf("Record returned %v", err)
			}
			if len(store.got) != 1 || store.got[0] != tc.want {
				t.Fatalf("summaries = %+v, want [%+v]", store.got, tc.want)
			}
			for _, e := range logs.All() {
				if marked(e, log.Show) && e.Message != "recording: go test ./..." {
					t.Errorf("%q is shown on the console although the store prints the end", e.Message)
				}
				if e.Level >= zap.WarnLevel && !marked(e, log.Hide) {
					t.Errorf("%q reaches the console although the store prints the end", e.Message)
				}
			}
		})
	}
}

func TestMissLineNamesTheCallAndTheCommand(t *testing.T) {
	got := missLine("orders.TestPrice", "HTTP", "GET /price/vase", "127.0.0.1:9000", 2,
		[]models.MockFieldDiff{{Path: "url.path", Expected: "/price/lamp", Actual: "/price/vase"}})
	want := `not in the recording: orders.TestPrice called GET /price/vase on 127.0.0.1:9000 (2 times); nearest recorded call has url.path "/price/lamp", this call "/price/vase"; add it: ` + Again("--on-miss", "record")
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestMissLineSaysWhichSideHasAField(t *testing.T) {
	got := missLine("T", "HTTP", "POST /a", "", 1, []models.MockFieldDiff{
		{Path: "body.x", Kind: models.DiffKindMissingInLive, Expected: "1"},
		{Path: "body.y", Kind: models.DiffKindMissingInMock, Actual: "2"},
	})
	if !strings.Contains(got, "; this call has no body.x, the nearest recorded call has it; the nearest recorded call has no body.y, this call has it; ") || strings.Contains(got, `""`) {
		t.Fatalf("got %s", got)
	}
}

func TestAgainReplacesAFlagTheUserAlreadyPassed(t *testing.T) {
	prev := os.Args
	t.Cleanup(func() { os.Args = prev })
	os.Args = []string{"/bin/keploy", "--debug", "mock", "replay", "--on-miss", "fail", "--on-miss=passthrough", "-c", "go test", "--", "x"}
	if got, want := Again("--on-miss", "record"), "keploy --debug mock replay -c 'go test' --on-miss record -- x"; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestRefusalKindNamesWhyRecordStopped(t *testing.T) {
	if got := RefusalKind(repeatedScope([]models.ScopeWindow{{Name: "a.TestA"}, {Name: "a.TestA"}}, false)); got != RefusedRepeated {
		t.Fatalf("repeated test: %q", got)
	}
	if got := RefusalKind(context.Canceled); got != "" {
		t.Fatalf("other error: %q", got)
	}
}
