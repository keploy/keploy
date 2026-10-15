package record

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
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
	inc := models.CaptureIncomplete{Class: models.CaptureIncompleteRealName, Destination: "10.0.0.7:443", Why: why}
	rec := func(inst Instrumentation, real ...string) (*observer.ObservedLogs, bool) {
		core, logs := observer.New(zapcore.WarnLevel)
		cfg := &config.Config{}
		cfg.Mock.Real = real
		r := &Recorder{logger: zap.New(core), config: cfg, instrumentation: inst}
		r.reportRealRun(context.Background())
		return logs, r.realUncaptured.Load()
	}
	logs, failed := rec(realRunInst{run: models.RealRun{Incomplete: []models.CaptureIncomplete{inc, inc, {Destination: "10.0.0.8:443", Why: "x"}}}}, "db:5432")
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
