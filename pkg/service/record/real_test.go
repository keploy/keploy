package record

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/realdeps"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// realRunInst is an agent client that reports the run's mock.real.
type realRunInst struct {
	Instrumentation
	run models.RealRun
	err error
}

func (f realRunInst) PushRealDeps(context.Context, []string) error { return nil }

func (f realRunInst) RealRun(context.Context) (models.RealRun, error) { return f.run, f.err }

// As a recording ends, what keploy could not capture is said at ERROR, once
// per destination and reason with its count: no test case holds what those
// connections carried, so the recording is incomplete and the run fails, as
// for a capture loss. A read that fails is said and fails the run too;
// without the switch or a list nothing is asked.
func TestReportRealRunSaysWhatKeployCouldNotCapture(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	why := "list the address in mock.real"
	inc := models.CaptureIncomplete{Class: models.CaptureIncompleteRealName, Destination: "10.0.0.7:443", Why: why, HostsListed: true}
	rec := func(inst Instrumentation, real ...string) (*observer.ObservedLogs, bool) {
		core, logs := observer.New(zapcore.WarnLevel)
		cfg := &config.Config{}
		cfg.Mock.Real = real
		r := &Recorder{logger: zap.New(core), config: cfg, instrumentation: inst}
		r.reportRealRun(context.Background())
		return logs, r.realUncaptured.Load() || r.realUnread.Load() != nil
	}
	logs, failed := rec(realRunInst{run: models.RealRun{Incomplete: []models.CaptureIncomplete{inc, inc, {Destination: "10.0.0.8:443", Why: "x", HostsListed: true}}}}, "db:5432")
	require.True(t, failed, "a recording keploy could not capture whole fails")
	require.Equal(t, 2, logs.FilterLevelExact(zapcore.ErrorLevel).Len())
	first := logs.All()[0].ContextMap()
	require.Equal(t, "10.0.0.7:443", first["destination"])
	require.EqualValues(t, 2, first["connections"])
	require.Contains(t, first["next_step"], why)

	logs, failed = rec(realRunInst{err: errors.New("agent gone")}, "db:5432")
	require.True(t, failed, "a recording keploy cannot tell is whole fails")
	require.Equal(t, 1, logs.FilterLevelExact(zapcore.ErrorLevel).Len())

	logs, failed = rec(realRunInst{run: models.RealRun{Reached: []string{"db:5432"}}}, "db:5432")
	require.False(t, failed)
	require.Zero(t, logs.Len())

	logs, failed = rec(realRunInst{run: models.RealRun{Incomplete: []models.CaptureIncomplete{inc}}})
	require.Zero(t, logs.Len(), "no list, nothing asked")
	require.False(t, failed)
	t.Setenv(provenswitch.Env, "")
	logs, failed = rec(realRunInst{run: models.RealRun{Incomplete: []models.CaptureIncomplete{inc}}}, "db:5432")
	require.Zero(t, logs.Len(), "no switch, nothing asked")
	require.False(t, failed)
}

// Where a later pass judges each test case (JudgeLossesLater), what keploy
// could not capture of mock.real, and a read of it that failed, are each a
// WARN, not an ERROR: the recording is kept, and they are handed over
// (finish), the failed read as that, not as connections lost.
func TestReportRealRunJudgedLaterIsAWarn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	judgeLater(t)
	inc := models.CaptureIncomplete{Class: models.CaptureIncompleteRealName, Destination: "10.0.0.7:443", Why: "list the address in mock.real"}
	for _, inst := range []realRunInst{{run: models.RealRun{Incomplete: []models.CaptureIncomplete{inc}}}, {err: errors.New("agent gone")}} {
		core, logs := observer.New(zapcore.WarnLevel)
		cfg := &config.Config{}
		cfg.Mock.Real = []string{"db:5432"}
		r := &Recorder{logger: zap.New(core), config: cfg, instrumentation: inst}
		r.reportRealRun(context.Background())
		require.Zero(t, logs.FilterLevelExact(zapcore.ErrorLevel).Len(), "an ERROR where a later pass judges: %v", logs.All())
		require.Equal(t, 1, logs.FilterMessageSnippet("the auto-replay that follows judges each of its test cases").Len())
		if inst.err != nil {
			require.False(t, r.realUncaptured.Load(), "a failed read is no connection lost")
			require.NotNil(t, r.realUnread.Load())
			require.Contains(t, *r.realUnread.Load(), "agent gone")
		}
	}
}

// What reportRealRun found of mock.real goes with the capture watch's end
// (finishCapture), whether or not the agent can count capture losses: a
// failed read fails the recording judged on its own, and is handed, as that,
// to a later pass, which fails nothing.
func TestARecordingsEndTakesWhatMockRealFound(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	inc := models.CaptureIncomplete{Class: models.CaptureIncompleteRealName, Destination: "10.0.0.7:443", Why: "list the address in mock.real"}
	for _, c := range []struct {
		name string
		inst realRunInst
	}{
		{"a failed read", realRunInst{err: errors.New("agent gone")}},
		{"a connection lost", realRunInst{run: models.RealRun{Incomplete: []models.CaptureIncomplete{inc}}}},
	} {
		for _, counts := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, agent counts %v", c.name, counts), func(t *testing.T) {
				// A fresh agent for each watch: its reads are counted.
				agent := func() Instrumentation {
					if !counts {
						return &countingInstr{errAt: map[int]error{1: models.ErrCaptureLossesUnsupported}, answers: []models.CaptureLosses{nil}}
					}
					return &countingInstr{answers: []models.CaptureLosses{nil}}
				}
				cfg := &config.Config{}
				cfg.Mock.Real = []string{"db:5432"}
				r := &Recorder{logger: zap.NewNop(), config: cfg, instrumentation: c.inst}
				r.reportRealRun(context.Background())
				w := watchCaptureLosses(context.Background(), zap.NewNop(), agent())
				if w.off == counts {
					t.Fatalf("the watch is off %v for an agent that counts %v", w.off, counts)
				}
				if fail, incomplete := r.finishCapture(w, "test-set-0"); !fail || !incomplete {
					t.Fatalf("judged on its own: fail %v, incomplete %v; want both", fail, incomplete)
				}
				got := judgeLater(t)
				w = watchCaptureLosses(context.Background(), zap.NewNop(), agent())
				if fail, incomplete := r.finishCapture(w, "test-set-0"); fail || !incomplete {
					t.Fatalf("judged later: fail %v, incomplete %v; want not failed, incomplete", fail, incomplete)
				}
				if len(*got) != 1 || (*got)[0].RealUncaptured == (c.inst.err != nil) || ((*got)[0].RealUnread != "") != (c.inst.err != nil) {
					t.Fatalf("handed %+v, want the %s as that", *got, c.name)
				}
			})
		}
	}
}

// The session's telemetry says a recording incomplete for the agent's captures
// not all handed over or written, as before, and for what the capture watch
// found, even where a later pass judges it and the run does not fail.
func TestTheSessionsEndSaysWhyItIsIncomplete(t *testing.T) {
	for _, c := range []struct {
		name                                   string
		stop                                   string
		heldBack, lossy, storeWrote, agentLost bool
		status, reason                         string
	}{
		{"clean", "", false, false, true, false, "completed", ""},
		{"stopped", "agent not ready", true, true, true, false, "aborted", "agent not ready"},
		{"held back", "", true, false, true, false, "incomplete", "the agent's captures from before the stop were not all handed over"},
		{"not written", "", true, true, false, false, "incomplete", "mocks the recording captured were not written to the mock file"},
		{"capture lost", "", false, true, true, false, "incomplete", "keploy's agent could not capture every call while it recorded, or could not be checked"},
	} {
		status, reason := sessionEnd(c.stop, c.heldBack, c.lossy, c.storeWrote, c.agentLost)
		if status != c.status || reason != c.reason {
			t.Errorf("%s: %q, %q; want %q, %q", c.name, status, reason, c.status, c.reason)
		}
	}
}

// keploy record logs what keploy could neither capture nor relay as real by
// its next step: a mock.real connection's other name, which the agent's Why
// names whole for the report, is never logged whole, at ERROR or, judged
// later, at WARN. An agent that lists no hosts sends the Why alone.
func TestReportRealRunNeverLogsAnOlderAgentsOtherName(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	host := "canary7q.tenant.example"
	why := "a mock.real connection to api.test:443 carried another name: a request with Host " + host + ", which mock.real does not list; keploy ended it (one connection carries one real name: list that name too, or let the client open a connection per name)"
	inc := models.CaptureIncomplete{Class: models.CaptureIncompleteRealName, Destination: "127.0.0.1:443", Why: why}
	for _, later := range []bool{false, true} {
		if later {
			judgeLater(t)
		}
		core, logs := observer.New(zapcore.DebugLevel)
		cfg := &config.Config{}
		cfg.Mock.Real = []string{"api.test:443"}
		var console bytes.Buffer
		r := &Recorder{logger: zap.New(core), config: cfg, instrumentation: realRunInst{run: models.RealRun{Incomplete: []models.CaptureIncomplete{inc}}}}
		r.console = &console
		r.reportRealRun(context.Background())
		require.Equal(t, 1, logs.Len(), "judged later %v: %v", later, logs.All())
		require.Contains(t, console.String(), why, "judged later %v: the console does not name an older agent's why whole", later)
		for _, e := range logs.All() {
			for k, v := range e.ContextMap() {
				require.NotContains(t, fmt.Sprint(v), host, "judged later %v: the %s line's %s names the other name whole", later, e.Level, k)
			}
		}
	}
}

// The agent lists the other name its Why names, with the tag its own line
// names it by (models.CaptureIncomplete.Hosts): keploy record's line names it
// by its shape and that tag, at ERROR and, judged later, at WARN, whatever
// form the name takes, and keeps the rest of the next step.
func TestReportRealRunLogsTheOtherNameByItsShapeAndTag(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	agent := models.NewLogKeySession()
	for _, host := range []string{"canary7q.tenant.example", "orders-svc", "10-0-0-1.default.pod.cluster.local:443"} {
		w := agent.LossText().Say("a mock.real connection to api.test:443 carried another name: a request with Host ").Host(host).
			Say(", which mock.real does not list; keploy ended it (one connection carries one real name: list that name too, or let the client open a connection per name)")
		why := w.String()
		inc := models.CaptureIncomplete{Class: models.CaptureIncompleteRealName, Destination: "127.0.0.1:443", Why: why, Hosts: w.Hosts(), HostsListed: true}
		for _, later := range []bool{false, true} {
			undo := func() {}
			if later {
				undo = JudgeLossesLater(func(RecordedLosses) {})
			}
			core, logs := observer.New(zapcore.DebugLevel)
			cfg := &config.Config{}
			cfg.Mock.Real = []string{"api.test:443"}
			var console bytes.Buffer
			r := &Recorder{logger: zap.New(core), config: cfg, instrumentation: realRunInst{run: models.RealRun{Incomplete: []models.CaptureIncomplete{inc}}}}
			r.SetConsole(&console)
			r.reportRealRun(context.Background())
			undo()
			require.Equal(t, 1, logs.Len(), "%s, judged later %v: %v", host, later, logs.All())
			require.Contains(t, console.String(), "a request with Host "+host+" "+models.HostTagLabel(agent.HostTag(host))+", which mock.real does not list",
				"%s, judged later %v: the console does not name the other name whole beside the tag the log names it by", host, later)
			step := fmt.Sprint(logs.All()[0].ContextMap()["next_step"])
			require.Contains(t, step, "carried another name: a request with Host "+agent.HostLogName(host)+", which mock.real does not list", "%s, judged later %v: the next step does not name the other name by its shape and the agent's tag", host, later)
			require.Contains(t, step, "list that name too")
			name, _, _ := strings.Cut(host, ":")
			require.NotContains(t, step, name, "%s, judged later %v: the next step names it whole", host, later)
		}
	}
}

// A connection to a mock.real address that several names share, whose
// request line keploy does not read, is closed unnamed (capture_incomplete),
// and the agent's why describes the line: keploy record's next step, at
// ERROR and, judged later, at WARN, holds no byte of it (its token, its
// path, an absolute target's host). The evidence is the agent's own
// (realdeps.ReadName, UnreadableWhy, Tracker.Pending): its why quoted the
// line, and the line was logged whole.
func TestReportRealRunNamesNoByteOfALineTheAgentCouldNotRead(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, line := range []string{
		"VERSION-CONTROL /repo/secret.txt?token=s3cr3tQ7 HTTP/1.1\r\nHost: canary7q.tenant.example\r\n\r\n",
		"GET http://canary7q.tenant.example/pay?token=s3cr3tQ7 HTTP/1.2\r\nHost: canary7q.tenant.example\r\n\r\n",
	} {
		_, err := realdeps.ReadName(bufio.NewReaderSize(strings.NewReader(line), realdeps.ReaderSize))
		require.ErrorIs(t, err, realdeps.ErrUnreadable, "%q", line)
		var tr realdeps.Tracker
		candidates := []string{"a.example.com", "b.example.com"}
		done := tr.Pending(realdeps.Origin{}, 1, "10.0.0.7:443", candidates)
		done(realdeps.Origin{}, realdeps.UnreadableWhy("10.0.0.7:443", candidates, err.Error()))
		run := tr.Run()
		require.Len(t, run.Incomplete, 1, "%q", line)
		for _, later := range []bool{false, true} {
			if later {
				judgeLater(t)
			}
			core, logs := observer.New(zapcore.DebugLevel)
			cfg := &config.Config{}
			cfg.Mock.Real = []string{"a.example.com:443", "b.example.com:443"}
			r := &Recorder{logger: zap.New(core), config: cfg, instrumentation: realRunInst{run: run}}
			r.reportRealRun(context.Background())
			require.NotZero(t, logs.Len(), "precondition: reportRealRun said nothing")
			for _, e := range logs.All() {
				for k, v := range e.ContextMap() {
					for _, b := range []string{"s3cr3tQ7", "canary7q", "secret.txt", "/pay"} {
						require.NotContains(t, fmt.Sprint(v), b, "%q judged later %v: the %s line's %s holds the request line", line, later, e.Level, k)
					}
				}
			}
		}
	}
}
