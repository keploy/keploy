package mock

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// --strict fails a replay on a miss that counts against a test
// (models.UnmatchedCall.CountsAgainstTest), never on one that counts against
// none by itself: a connection whose client sent nothing (a MySQL client
// waiting at a port keploy could not tie to the recording, or a dependency's
// TCP health check), or a DNS lookup's, which the proxy answers itself.
func TestStrictFailsOnlyOnAMissThatCountsAgainstATest(t *testing.T) {
	silent := models.UnmatchedCall{Protocol: "MySQL", ActualSummary: "connection phase: server greeting",
		Destination: "172.17.0.1:3307", MatchPhase: models.MatchPhaseNoRequest}
	dns := models.UnmatchedCall{Protocol: "DNS", ActualSummary: "A dep.example"}
	call := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /a", Destination: "dep:80"}
	for _, tc := range []struct {
		name     string
		misses   []models.UnmatchedCall
		wantExit int
	}{
		{"only misses that count against no test", []models.UnmatchedCall{silent, dns}, 0},
		{"a call that missed", []models.UnmatchedCall{silent, dns, call}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
			instr.consumedMocks = []models.MockState{{Name: "mock-0"}}
			instr.mockErrors = tc.misses
			cfg := instrConfig(instr, utils.Native, "pytest -q")
			cfg.Mock.Strict = true
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })

			if err := New(zap.New(core), instr, loadedMockDB{n: 1}, nil, nil, nil, cfg).Replay(context.Background()); err != nil {
				t.Fatalf("Replay returned an unexpected error: %v", err)
			}
			if utils.ErrCode != tc.wantExit {
				t.Errorf("exit code %d, want %d", utils.ErrCode, tc.wantExit)
			}
			var strict []map[string]any
			for _, e := range logs.All() {
				if strings.Contains(e.Message, "replay failed under --strict") {
					strict = append(strict, e.ContextMap())
				}
			}
			if tc.wantExit == 0 && len(strict) > 0 {
				t.Fatalf("--strict failed the run: %v", strict)
			}
			// It is --strict that failed it, on the one miss that counts.
			if tc.wantExit == 1 && (len(strict) != 1 || strict[0]["missed"] != int64(1)) {
				t.Fatalf("--strict failures %v; want one, of 1 missed call", strict)
			}
		})
	}
}

// A replay whose only misses count against no test proved isolation: a DNS
// lookup's miss is the proxy's own answer, and a connection whose client sent
// nothing made no call, so neither reached a real service. Its receipt says
// so, as --strict passing it does.
func TestAMissThatCountsAgainstNoTestLeavesTheRunIsolated(t *testing.T) {
	silent := models.UnmatchedCall{Protocol: "MySQL", Destination: "172.17.0.1:3307", MatchPhase: models.MatchPhaseNoRequest}
	dns := models.UnmatchedCall{Protocol: "DNS", ActualSummary: "A dep.example"}
	for _, tc := range []struct {
		name     string
		misses   []models.UnmatchedCall
		isolated bool
	}{
		{"only misses that count against no test", []models.UnmatchedCall{silent, dns}, true},
		{"a call that missed", []models.UnmatchedCall{silent, dns, {Protocol: "HTTP", ActualSummary: "GET /a"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
			instr.consumedMocks = []models.MockState{{Name: "mock-0"}}
			instr.mockErrors = tc.misses
			cfg := instrConfig(instr, utils.Native, "pytest -q")
			cfg.Mock.OnMiss = string(models.MissFail)
			cfg.Path = t.TempDir()
			if err := os.MkdirAll(filepath.Join(cfg.Path, "set"), 0o755); err != nil {
				t.Fatal(err)
			}
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			if err := New(zap.NewNop(), instr, loadedMockDB{n: 1}, nil, nil, nil, cfg).Replay(context.Background()); err != nil {
				t.Fatalf("Replay: %v", err)
			}
			r, err := ReadReceipt(cfg.Path, "set")
			if err != nil {
				t.Fatal(err)
			}
			if r.Isolated != tc.isolated || r.Missed != len(tc.misses) {
				t.Errorf("receipt isolated %v (%q), missed %d; want isolated %v, missed %d", r.Isolated, r.IsolationNote, r.Missed, tc.isolated, len(tc.misses))
			}
		})
	}
}
