package proxy

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/starts"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// selfSource is how the sink names this test's own process (a call from its
// pid): its comm, and its program as keploy names one (starts.ProcessOf: a
// test binary the go tool built by its go test, else argv[0], redacted),
// where kernel pids are keploy's own (utils.KernelPidsAreLocal); else no
// name, why, and the pid. The comm is read here from /proc; how a program is
// named is starts' to pin (TestAGoTestBinaryIsNamedByItsGoTest), and this
// row pins only that the sink names the call's process.
func selfSource(t *testing.T) models.Origin {
	t.Helper()
	pid := uint32(os.Getpid())
	if !utils.KernelPidsAreLocal() {
		return models.Origin{Unnamed: models.UnnamedPIDNamespace, PID: pid}
	}
	comm, err := os.ReadFile("/proc/self/comm")
	if err != nil {
		t.Skipf("no /proc/self/comm: %v", err)
	}
	_, exe, ok := starts.Default.ProcessOf(pid)
	if !ok || exe == "" {
		t.Fatalf("keploy names no program of this process (pid %d)", pid)
	}
	return models.Origin{Comm: strings.TrimSpace(string(comm)), Exe: exe}
}

// The sink names the process that made each call it is told of, a miss and
// an answered call alike (MockMismatchReport.Source), from the
// call's pid, under the proven switch only: with the switch off a miss names
// no process, so a report keploy test writes without the switch is as it was.
// A report whose parser named its process keeps that name.
func TestTheSinkNamesTheProcessOfEachCallUnderTheSwitchOnly(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads this process's /proc entry")
	}
	self := uint32(os.Getpid())
	want := selfSource(t)
	for _, on := range []bool{true, false} {
		if on {
			t.Setenv(provenswitch.Env, "1")
		} else {
			t.Setenv(provenswitch.Env, "")
		}
		p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 8)}
		p.BeginTestErrorCapture()
		ctx := models.WithMockMismatchReporter(context.Background(), func(err error) { p.sendMockNotFoundErrorFrom(err, self) })
		if !models.ReportMockMismatch(ctx, models.NewMockMismatchError(models.ErrNoMockMatched, &models.MockMismatchReport{Protocol: "MongoDB", ActualSummary: "insert on events"})) {
			t.Fatal("the miss was not reported")
		}
		named := &models.Origin{Comm: "parser-named", Exe: "./parser-named"}
		if !models.ReportMockMismatch(ctx, models.NewMockMismatchError(models.ErrNoMockMatched, &models.MockMismatchReport{Protocol: "HTTP", ActualSummary: "GET /x", Source: named})) {
			t.Fatal("the named miss was not reported")
		}
		if on && !models.ReportAnswer(ctx, &models.MockMismatchReport{Protocol: "MongoDB", ActualSummary: "insert on events", Answer: models.AnswerByKind, WriteKind: "Mongo insert keploy.events"}) {
			t.Fatal("the answer was not reported")
		}
		got, err := p.GetMockErrors(context.Background())
		if err != nil && !errors.Is(err, models.ErrUnmatchedCallsUnread) {
			t.Fatal(err)
		}
		calls := 2
		if on {
			calls = 3
		}
		if len(got) != calls {
			t.Fatalf("switch=%v: %d calls %+v; want %d", on, len(got), got, calls)
		}
		for _, c := range got {
			switch {
			case c.ActualSummary == "GET /x":
				if c.Source == nil || *c.Source != *named {
					t.Errorf("switch=%v: the parser's name became %+v", on, c.Source)
				}
			case !on:
				if c.Source != nil {
					t.Errorf("without the switch a miss names the process %+v", c.Source)
				}
			case c.Source == nil || *c.Source != want:
				t.Errorf("answer=%q: Source = %+v; want %+v", c.Answer, c.Source, want)
			}
		}
	}
}
