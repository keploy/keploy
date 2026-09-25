package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/cli/provider"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	contractSvc "go.keploy.io/server/v3/pkg/service/contract"
	diffSvc "go.keploy.io/server/v3/pkg/service/diff"
	recordSvc "go.keploy.io/server/v3/pkg/service/record"
	replaySvc "go.keploy.io/server/v3/pkg/service/replay"
	reportSvc "go.keploy.io/server/v3/pkg/service/report"
	toolsSvc "go.keploy.io/server/v3/pkg/service/tools"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// realFlags is the real CmdConfigurator's flags with none of its validation:
// the commands below run against fake services, so there is no keploy folder,
// config file or application for Validate to check, and nothing it would
// check decides the exit code under test.
type realFlags struct{ *provider.CmdConfigurator }

func (realFlags) ValidateFlags(context.Context, *cobra.Command) error { return nil }
func (realFlags) Validate(context.Context, *cobra.Command) error      { return nil }

// runKeploy runs `keploy <args>` through the real command tree against factory
// and returns the exit code the process would end with -- main.finalExitCode's
// rule: an exit code already armed stands, else a returned error is 1.
func runKeploy(t *testing.T, ctx context.Context, factory ServiceFactory, args ...string) int {
	t.Helper()
	return runKeployLogging(t, ctx, zap.NewNop(), factory, args...)
}

// runKeployLogging is runKeploy with the commands logging to logger.
func runKeployLogging(t *testing.T, ctx context.Context, logger *zap.Logger, factory ServiceFactory, args ...string) int {
	t.Helper()
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	root := Root(ctx, logger, factory, realFlags{provider.NewCmdConfigurator(logger, config.New())})
	root.SetArgs(args)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil && utils.ErrCode == 0 {
		return utils.ExitKeployError
	}
	return utils.ErrCode
}

// liveCtx is a context nobody has interrupted, installed as the root context's
// cancel the way main installs utils.NewCtx's: `keploy test` calls
// utils.ExecCancel on its way out.
func liveCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	utils.SetCancel(cancel)
	t.Cleanup(cancel)
	return ctx, cancel
}

type fakeRecord struct{ start func(context.Context) error }

func (f fakeRecord) Start(ctx context.Context) error { return f.start(ctx) }

var _ recordSvc.Service = fakeRecord{}

func recordThat(start func(context.Context) error) ServiceFactory {
	return agentSvcFactory{svc: fakeRecord{start: start}}
}

func appExit(kind models.AppErrorType, code int) error {
	return fmt.Errorf("user application terminated unexpectedly hence stopping keploy: %w",
		models.AppError{AppErrorType: kind, Err: errors.New("exit status"), ExitCode: code})
}

// startStoppedBy is the error record.Start returns when the application ended
// the recording (pkg/service/record/apperror.go): the stop reason, over the
// AppError and the error that carries, which AppError does not unwrap to.
type startStoppedBy struct{ app models.AppError }

func (e startStoppedBy) Error() string {
	return "error in running the user application, hence stopping keploy"
}
func (e startStoppedBy) Unwrap() []error { return []error{e.app, e.app.Err} }

// `keploy record` that did not record has to exit non-zero, or a CI job that
// wraps it goes green with nothing recorded. Every failure below was logged and
// then exited 0.
//
// When the user's APPLICATION is what ended the recording, the exit code is
// the application's own -- the same contract `keploy mock record` keeps for
// its test command (utils/exitcodes.go): whoever reads it learns what the
// application did, and a Keploy code there would claim a Keploy failure that
// did not happen. A failure of Keploy's own is 1, or the specific code the
// failure carries or already armed.
func TestRecordThatFailsExitsNonZero(t *testing.T) {
	for _, tc := range []struct {
		name    string
		factory ServiceFactory
		want    int
	}{
		{"no service", agentSvcFactory{err: errors.New("no such service")}, utils.ExitKeployError},
		{"not a record service", agentSvcFactory{svc: struct{}{}}, utils.ExitKeployError},
		{"keploy failed", recordThat(func(context.Context) error {
			return errors.New("failed to get new test-set id")
		}), utils.ExitKeployError},
		{"keploy failed with a specific cause", recordThat(func(context.Context) error {
			return fmt.Errorf("failed setting up the environment: %w", utils.ErrEnvironmentUnsupported)
		}), utils.ExitEnvironmentUnsupported},
		// The agent client arms the code of an agent that could not start
		// before Start returns its own, flatter, error.
		{"keploy failed with a code already armed", recordThat(func(context.Context) error {
			utils.SetExitCodeOnce(utils.ExitPrivilegeRequired)
			return errors.New("failed setting up the environment")
		}), utils.ExitPrivilegeRequired},
		// Only the application's own exit is mirrored: an AppError that
		// reports a Keploy failure is Keploy's, whatever code it carries.
		{"keploy failed inside the application's error", recordThat(func(context.Context) error {
			return models.AppError{AppErrorType: models.ErrInternal, Err: errors.New("failed to load hooks"), ExitCode: 9}
		}), utils.ExitKeployError},
		{"application exited 7", recordThat(func(context.Context) error {
			return appExit(models.ErrUnExpected, 7)
		}), 7},
		{"application was killed by SIGTERM", recordThat(func(context.Context) error {
			return appExit(models.ErrUnExpected, 143)
		}), 143},
		{"application could not be found by the shell", recordThat(func(context.Context) error {
			return appExit(models.ErrCommandError, 127)
		}), 127},
		// No code to mirror: the command never became a process.
		{"application could not be started", recordThat(func(context.Context) error {
			return appExit(models.ErrCommandError, -1)
		}), utils.ExitKeployError},
		// Nor here: the wait for the app container failed, or the daemon
		// sent an error in place of a status (pkg/client/app/fromcontainer.go).
		// An application that ENDED the recording did not succeed, whatever
		// code it carries.
		{"application ended with no exit status", recordThat(func(context.Context) error {
			return appExit(models.ErrUnExpected, 0)
		}), utils.ExitKeployError},
		// With no code of the application's to mirror, the code is Keploy's
		// -- and a failure tagged with a specific one keeps it through the
		// AppError, as a tag must through every layer it crosses
		// (utils/exitcodes.go): a flat 1 would send the user to their
		// application over an environment Keploy named.
		{"application could not be started, for a reason keploy tagged", recordThat(func(context.Context) error {
			return startStoppedBy{models.AppError{AppErrorType: models.ErrCommandError, ExitCode: -1,
				Err: fmt.Errorf("failed to start the replacement container: %w", utils.ErrEnvironmentUnsupported)}}
		}), utils.ExitEnvironmentUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := liveCtx(t)
			if got := runKeploy(t, ctx, tc.factory, "record"); got != tc.want {
				t.Fatalf("exit code %d, want %d", got, tc.want)
			}
		})
	}
}

// The exit code of a recording the application ended is the application's,
// so it can be any code at all -- 3, 4 and 6 included, which Keploy uses for
// its own failures (utils/exitcodes.go). The failure line says which it is:
// it names the application's code when that is what the process exits with,
// and only then -- not over a run a signal ended, which exits 0, nor over a
// code Keploy had already armed, which is the one the process exits with.
func TestRecordNamesTheApplicationsExitCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		// before is what happened before Start returned err.
		before func(stop context.CancelFunc)
		err    error
		// want is the appExitCode logged, "" for none.
		want string
	}{
		{"application exited 3", nil, appExit(models.ErrUnExpected, 3), "3"},
		{"application could not be found by the shell", nil, appExit(models.ErrCommandError, 127), "127"},
		{"application ended with no exit status", nil, appExit(models.ErrUnExpected, 0), ""},
		{"keploy failed", nil, fmt.Errorf("failed setting up the environment: %w", utils.ErrPrivilegeRequired), ""},
		{"application killed by the signal that stopped keploy", func(stop context.CancelFunc) {
			utils.MarkInterrupted()
			stop()
		}, appExit(models.ErrUnExpected, 143), ""},
		// The agent client arms the code of an agent that could not start.
		{"application exited after keploy armed its own code", func(context.CancelFunc) {
			utils.SetExitCodeOnce(utils.ExitPrivilegeRequired)
		}, appExit(models.ErrUnExpected, 7), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			utils.ClearInterrupted()
			t.Cleanup(utils.ClearInterrupted)
			core, logs := observer.New(zapcore.ErrorLevel)
			ctx, cancel := liveCtx(t)
			runKeployLogging(t, ctx, zap.New(core), recordThat(func(context.Context) error {
				if tc.before != nil {
					tc.before(cancel)
				}
				return tc.err
			}), "record")
			failed := logs.FilterMessage("failed to record").All()
			if len(failed) != 1 {
				t.Fatalf("logged %d \"failed to record\" lines, want 1: %v", len(failed), logs.All())
			}
			got := ""
			if v, ok := failed[0].ContextMap()["appExitCode"]; ok {
				got = fmt.Sprint(v)
			}
			if got != tc.want {
				t.Fatalf("appExitCode %q logged, want %q (\"\": none)", got, tc.want)
			}
		})
	}
}

// A recording a signal ended -- the user's Ctrl+C, a SIGTERM -- is a recording
// that finished, and exits 0 whatever record.Start returned on its way out.
// Start returns nil for a stop on every path it can tell is one, but it cannot
// on all of them: a stop that lands while it is still bringing the
// environment up can surface as the failure it caused ("keploy-agent did not
// become ready in time"), and so can an application the same signal killed.
// Whether a signal arrived is utils.Interrupted's to say, not the error's.
func TestRecordThatASignalEndedExitsZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"keploy stopped mid-setup", errors.New("keploy-agent did not become ready in time")},
		{"the application killed by the same signal", appExit(models.ErrUnExpected, 143)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			utils.ClearInterrupted()
			t.Cleanup(utils.ClearInterrupted)
			ctx, cancel := liveCtx(t)
			signalled := recordThat(func(context.Context) error {
				utils.MarkInterrupted()
				cancel()
				return tc.err
			})
			if got := runKeploy(t, ctx, signalled, "record"); got != 0 {
				t.Fatalf("a recording a signal ended exited %d", got)
			}
		})
	}
}

// replayTestDB lists the test sets `keploy test` finds, and nothing else: the
// runs below end before a test is read.
type replayTestDB struct {
	replaySvc.TestDB
	list func(context.Context) ([]string, error)
}

func (db replayTestDB) GetAllTestSetIDs(ctx context.Context) ([]string, error) { return db.list(ctx) }

type replayInstrumentation struct{ replaySvc.Instrumentation }

func (replayInstrumentation) NotifyGracefulShutdown(context.Context) error { return nil }

type replayTelemetry struct{ replaySvc.Telemetry }

func (replayTelemetry) TestRunAborted(string) {}

// realReplayer is the real replay service over a keploy folder whose test sets
// list returns.
func realReplayer(list func(context.Context) ([]string, error)) ServiceFactory {
	return agentSvcFactory{svc: newReplayer(list)}
}

func newReplayer(list func(context.Context) ([]string, error)) replaySvc.Service {
	return replaySvc.NewReplayer(zap.NewNop(), replayTestDB{list: list}, nil, nil, nil, nil,
		replayTelemetry{}, replayInstrumentation{}, nil, &config.Config{})
}

// `keploy test` that ran no test has to exit non-zero. The replay service
// arms the exit code itself for everything that goes wrong once it starts
// (replay.Start's first defer), including the empty keploy/ folder of a CI job
// whose recording step recorded nothing -- which exited 0. The two failures
// before it starts are this command's to arm.
func TestTestThatRunsNoTestExitsNonZero(t *testing.T) {
	for _, tc := range []struct {
		name    string
		factory ServiceFactory
	}{
		{"no service", agentSvcFactory{err: errors.New("no such service")}},
		{"not a replay service", agentSvcFactory{svc: struct{}{}}},
		{"no test sets recorded", realReplayer(func(context.Context) ([]string, error) { return nil, nil })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := liveCtx(t)
			if got := runKeploy(t, ctx, tc.factory, "test"); got != utils.ExitKeployError {
				t.Fatalf("exit code %d, want %d", got, utils.ExitKeployError)
			}
		})
	}
}

// ...and a `keploy test` the user interrupted before any test ran still exits
// 0: cancelled is not failed.
func TestTestInterruptedBeforeAnyTestExitsZero(t *testing.T) {
	ctx, cancel := liveCtx(t)
	interrupted := realReplayer(func(ctx context.Context) ([]string, error) {
		cancel()
		return nil, ctx.Err()
	})
	if got := runKeploy(t, ctx, interrupted, "test"); got != 0 {
		t.Fatalf("an interrupted run exited %d", got)
	}
}

// fakeTools fails, or succeeds, every tools operation the same way.
type fakeTools struct {
	toolsSvc.Service
	err error
}

func (f fakeTools) Update(context.Context) error                 { return f.err }
func (f fakeTools) Export(context.Context) error                 { return f.err }
func (f fakeTools) Import(context.Context, string, string) error { return f.err }
func (f fakeTools) Templatize(context.Context) error             { return f.err }
func (f fakeTools) Sanitize(context.Context) error               { return f.err }
func (f fakeTools) Normalize(context.Context) error              { return f.err }

type fakeContract struct{ err error }

func (f fakeContract) Generate(context.Context, bool) error    { return f.err }
func (f fakeContract) GenerateFromTests(context.Context) error { return f.err }
func (f fakeContract) Download(context.Context, bool) error    { return f.err }
func (f fakeContract) Validate(context.Context) error          { return f.err }

type fakeReport struct{ err error }

func (f fakeReport) GenerateReport(context.Context) error { return f.err }

type fakeDiff struct{ err error }

func (f fakeDiff) Compare(context.Context, string, string, []string) error { return f.err }

var (
	_ contractSvc.Service = fakeContract{}
	_ reportSvc.Service   = fakeReport{}
	_ diffSvc.Service     = fakeDiff{}
)

// serviceCommands is every command, other than record and test, that runs a
// service: its arguments, and that service failing with err (nil: succeeding).
var serviceCommands = []struct {
	args []string
	svc  func(err error) interface{}
}{
	{[]string{"contract", "generate"}, func(err error) interface{} { return fakeContract{err} }},
	{[]string{"contract", "generate", "--infer"}, func(err error) interface{} { return fakeContract{err} }},
	{[]string{"contract", "download"}, func(err error) interface{} { return fakeContract{err} }},
	{[]string{"contract", "test"}, func(err error) interface{} { return fakeContract{err} }},
	{[]string{"diff", "test-run-1", "test-run-2"}, func(err error) interface{} { return fakeDiff{err} }},
	{[]string{"export", "postman"}, func(err error) interface{} { return fakeTools{err: err} }},
	{[]string{"import", "postman"}, func(err error) interface{} { return fakeTools{err: err} }},
	{[]string{"normalize"}, func(err error) interface{} { return fakeTools{err: err} }},
	{[]string{"report"}, func(err error) interface{} { return fakeReport{err} }},
	{[]string{"sanitize"}, func(err error) interface{} { return fakeTools{err: err} }},
	{[]string{"templatize"}, func(err error) interface{} { return fakeTools{err: err} }},
	{[]string{"update"}, func(err error) interface{} { return fakeTools{err: err} }},
}

// Every other command that runs a service: a failure it logs has to fail the
// process too. Each of these printed its ERROR line and exited 0, so a script
// that runs `keploy normalize` or `keploy report` and checks $? was told it
// worked.
func TestServiceCommandsThatFailExitNonZero(t *testing.T) {
	for _, c := range serviceCommands {
		for _, tc := range []struct {
			name    string
			factory ServiceFactory
			want    int
		}{
			{"succeeds", agentSvcFactory{svc: c.svc(nil)}, 0},
			{"fails", agentSvcFactory{svc: c.svc(errors.New("boom"))}, utils.ExitKeployError},
			// The code a failure is tagged with (utils.ExitCodeFor), whichever
			// command hits it.
			{"fails with a tagged cause", agentSvcFactory{svc: c.svc(fmt.Errorf("%w: tracefs is not mounted", utils.ErrEnvironmentUnsupported))}, utils.ExitEnvironmentUnsupported},
			{"no service", agentSvcFactory{err: errors.New("no such service")}, utils.ExitKeployError},
			{"wrong service", agentSvcFactory{svc: struct{}{}}, utils.ExitKeployError},
		} {
			t.Run(fmt.Sprint(c.args, " ", tc.name), func(t *testing.T) {
				ctx, _ := liveCtx(t)
				if got := runKeploy(t, ctx, tc.factory, c.args...); got != tc.want {
					t.Fatalf("exit code %d, want %d", got, tc.want)
				}
			})
		}
	}
}

// signalDuring lands a signal -- the user's Ctrl+C, CI's or the kubelet's
// SIGTERM -- while the command gets its service from the factory it wraps,
// the way utils.NewCtx's handler delivers one: marked, then the root context
// cancelled. Everything the command does after that, it does interrupted.
type signalDuring struct {
	ServiceFactory
	cancel context.CancelFunc
}

func (f signalDuring) GetService(ctx context.Context, name string) (interface{}, error) {
	utils.MarkInterrupted()
	f.cancel()
	return f.ServiceFactory.GetService(ctx, name)
}

// A command a signal ended did not fail: Ctrl+C is the user saying stop, and a
// SIGTERM is CI or the kubelet saying it. Every command exits 0 then, whatever
// failed on the way out -- the service that saw its context cancelled, or the
// service it never got -- exactly as `keploy record` and `keploy test` always
// have. One rule, not one per command: decided per command, a `keploy report`
// or a long `keploy contract test` that the user stopped would exit 1 where a
// stopped recording exits 0.
func TestACommandASignalEndedExitsZero(t *testing.T) {
	commands := append([]struct {
		args []string
		svc  func(err error) interface{}
	}{
		{[]string{"record"}, func(err error) interface{} {
			return fakeRecord{start: func(context.Context) error { return err }}
		}},
		{[]string{"test"}, func(err error) interface{} {
			return newReplayer(func(context.Context) ([]string, error) { return nil, err })
		}},
	}, serviceCommands...)
	for _, c := range commands {
		for _, tc := range []struct {
			name    string
			factory ServiceFactory
		}{
			{"stopped", agentSvcFactory{svc: c.svc(context.Canceled)}},
			{"failed as it stopped", agentSvcFactory{svc: c.svc(fmt.Errorf("%w: tracefs is not mounted", utils.ErrEnvironmentUnsupported))}},
			{"no service", agentSvcFactory{err: context.Canceled}},
			{"wrong service", agentSvcFactory{svc: struct{}{}}},
		} {
			t.Run(fmt.Sprint(c.args, " ", tc.name), func(t *testing.T) {
				utils.ClearInterrupted()
				t.Cleanup(utils.ClearInterrupted)
				ctx, cancel := liveCtx(t)
				if got := runKeploy(t, ctx, signalDuring{tc.factory, cancel}, c.args...); got != 0 {
					t.Fatalf("a command a signal ended exited %d", got)
				}
			})
		}
	}
}

// A code a service armed itself outranks the generic one: `keploy update`
// arms 4 where there is no build for this platform, before it returns the
// error that says so.
func TestServiceCommandKeepsACodeItArmed(t *testing.T) {
	ctx, _ := liveCtx(t)
	update := agentSvcFactory{svc: armingTools{code: utils.ExitUnsupportedPlatform}}
	if got := runKeploy(t, ctx, update, "update"); got != utils.ExitUnsupportedPlatform {
		t.Fatalf("exit code %d, want %d", got, utils.ExitUnsupportedPlatform)
	}
}

type armingTools struct {
	toolsSvc.Service
	code int
}

func (a armingTools) Update(context.Context) error {
	utils.SetExitCodeOnce(a.code)
	return errors.New("release asset is not available")
}
