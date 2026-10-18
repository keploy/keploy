package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// failsHeld fails the eBPF load, as the kernel refuses it, with what it loaded
// held up in its teardown (a held stage) until the test lets it go.
type failsHeld struct {
	coreAgent.Hooks // embedded: nil, so any other call panics loudly
	err             error
	stage           *heldStage
}

func (h failsHeld) Load(ctx context.Context, _ coreAgent.HookCfg, _ config.Agent) error {
	h.stage.start(ctx)
	return h.err
}

// An agent whose hooks fail to load reports its start failed, with the code it
// exits with, before its teardown, which can take long: the CLI that waits for
// it fails the start at once, with the cause the code names. Reported only at
// the exit, a teardown longer than the CLI's count of quiet beats read as a
// start that had stopped making progress, and a fresh start was tried, three
// times, for a failure each meets again. Run in a process of its own: the
// start's report is the process's.
func TestSetupReportsAFailedStartBeforeItsTeardown(t *testing.T) {
	if os.Getenv("KEPLOY_TEST_BOOT_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSetupReportsAFailedStartBeforeItsTeardown$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), "KEPLOY_TEST_BOOT_CHILD=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return
	}
	// Setup removes the agent's ready file: this one, not the host's.
	defer func(f string) { agentReadyFile = f }(agentReadyFile)
	agentReadyFile = filepath.Join(t.TempDir(), "agent.ready")
	path := filepath.Join(t.TempDir(), utils.BootProgressFileName)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The reporter writes into the test's temp dir until it has seen ctx end:
	// it is waited for before that dir is removed.
	reported := utils.ReportBootProgress(ctx, zap.NewNop(), path)
	t.Cleanup(func() { cancel(); <-reported })
	stage := newHeldStage(t)
	err := fmt.Errorf("%w: failed to set memlock rlimit: %w", utils.ErrPrivilegeRequired, syscall.EPERM)
	a := &Agent{logger: zap.NewNop(), Hooks: failsHeld{err: err, stage: stage}, config: &config.Config{}}
	returned := make(chan error, 1)
	go func() { returned <- a.Setup(ctx, make(chan int)) }()

	deadline := time.Now().Add(30 * time.Second)
	for {
		if b, rerr := os.ReadFile(path); rerr == nil {
			if p, perr := utils.ParseBootProgress(b); perr == nil && p.Failed {
				if p.ExitCode != utils.ExitPrivilegeRequired {
					t.Fatalf("the report says exit code %d, want %d", p.ExitCode, utils.ExitPrivilegeRequired)
				}
				break
			}
		}
		select {
		case r := <-returned:
			t.Fatalf("Setup returned (%v) before its start was reported failed", r)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the start's failure was not reported while its teardown was held")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case r := <-returned:
		t.Fatalf("Setup returned (%v) with its teardown held", r)
	default:
	}
	stage.end()
	if r := <-returned; r == nil {
		t.Fatal("Setup of a start that failed returned nil")
	}
}

// spinningProxy starts as a proxy whose start takes long without a step does
// (a CA install's JVMs, on a loaded host): it works on its own thread until the
// test lets it go.
type spinningProxy struct {
	coreAgent.Proxy // embedded: nil, so any other call panics loudly
	release         chan struct{}
}

func (p *spinningProxy) StartProxy(context.Context, coreAgent.ProxyOptions) error {
	for {
		select {
		case <-p.release:
			return nil
		default:
		}
		for i := 0; i < 1e6; i++ { // work, on this goroutine's thread
		}
	}
}

func (p *spinningProxy) SetAuxiliaryHook(coreAgent.AuxiliaryProxyHook) {}

// The proxy's start is a stage of the agent's start (utils.BeginBootWork):
// while it works, the agent reports itself busy, so the CLI counts none of its
// beats as one without progress. Unmarked, a proxy start of more than the
// CLI's count of quiet beats (a CA install on a loaded host) read as a start
// that had stopped. Run in a process of its own: the start's report is the
// process's.
func TestTheProxysStartIsReportedBusyWhileItWorks(t *testing.T) {
	if os.Getenv("KEPLOY_TEST_BOOT_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestTheProxysStartIsReportedBusyWhileItWorks$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), "KEPLOY_TEST_BOOT_CHILD=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return
	}
	if !utils.TeardownProgressSeen() {
		t.Skip("a stage's progress is seen on Linux alone")
	}
	path := filepath.Join(t.TempDir(), utils.BootProgressFileName)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The reporter writes into the test's temp dir until it has seen ctx end:
	// it is waited for before that dir is removed.
	reported := utils.ReportBootProgress(ctx, zap.NewNop(), path)
	t.Cleanup(func() { cancel(); <-reported })
	proxy := &spinningProxy{release: make(chan struct{})}
	a := &Agent{logger: zap.NewNop(), Hooks: loadedHooks{}, Proxy: proxy, config: &config.Config{}}
	grp, gctx := errgroup.WithContext(ctx)
	hookCtx, stop := context.WithCancel(context.WithValue(gctx, models.ErrGroupKey, grp))
	defer func() { stop(); _ = grp.Wait() }()
	returned := make(chan error, 1)
	go func() { returned <- a.Hook(hookCtx, models.HookOptions{Mode: models.MODE_RECORD}) }()
	defer close(proxy.release)

	deadline := time.Now().Add(30 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil {
			if p, err := utils.ParseBootProgress(b); err == nil && p.Busy {
				return
			}
		}
		select {
		case err := <-returned:
			t.Fatalf("Hook returned (%v) while its proxy was starting", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent was not reported busy while its proxy's start worked")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
