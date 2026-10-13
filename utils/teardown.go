package utils

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// The agent's teardown progress, for the CLI that stops it.
//
// A keploy agent removes what it attached to the kernel as it shuts down, and
// an agent killed part way through removing a TLS uprobe leaks it: the
// breakpoint stays in every process that maps the probed library. So the CLI
// kills a stopping agent only once it has stopped making progress, or has
// detached everything and still not exited (pkg/platform/http, stopWatch).
// What it judges by is what ReportTeardownProgress writes here.

// TeardownProgress is what an agent reports of its teardown once it has begun.
type TeardownProgress struct {
	// Seq counts the steps of the teardown taken so far (TeardownStep): it
	// moves while the teardown makes progress.
	Seq uint64 `json:"seq"`
	// Beat counts the reporter's beats since the teardown began, one every
	// TeardownBeat of the agent's own: each is one check of its progress. An
	// agent that is not scheduled does not beat, so a loaded host only
	// slows the checks down.
	Beat uint64 `json:"beat"`
	// Done says the agent has detached everything it attached
	// (MarkDetachDone): nothing a kill can cut short is left.
	Done bool `json:"done"`
}

// TeardownBeat is how often the reporter beats once the teardown has begun.
const TeardownBeat = time.Second

// teardownBeat is TeardownBeat, which tests shorten.
var teardownBeat = TeardownBeat

var (
	teardownSeq  atomic.Uint64
	teardownDone atomic.Bool
	// teardownKick wakes the reporter to write a step or the end at once.
	teardownKick = make(chan struct{}, 1)
)

func kickTeardownReport() {
	select {
	case teardownKick <- struct{}{}:
	default:
	}
}

// TeardownStep records a step of the agent's teardown: something detached,
// closed or stopped. A teardown that takes no step for the CLI's fixed count
// of beats is taken for one that has stopped making progress, so a stage
// that can take long should record each of its parts.
func TeardownStep() {
	teardownSeq.Add(1)
	kickTeardownReport()
}

// TeardownSteps returns how many teardown steps have been recorded
// (TeardownStep): what the reporter writes as Seq.
func TeardownSteps() uint64 { return teardownSeq.Load() }

// MarkDetachDone records that the agent has detached everything it attached:
// all that is left is to exit, and the CLI may kill it if it does not. It is
// called as late as the process allows -- by main, once the command has
// returned and anything that main itself removes is gone.
func MarkDetachDone() {
	teardownDone.Store(true)
	kickTeardownReport()
}

// ReportTeardownProgress writes the agent's teardown progress to path
// (TeardownProgress, as JSON) once ctx is done -- the root context, whose
// cancel begins the teardown: at once, on every step and on MarkDetachDone,
// and on every beat (TeardownBeat) until the process exits. Whatever an
// earlier agent left at path is removed now, so a CLI never reads it as this
// agent's. Each write replaces the file whole (a rename), so a reader never
// sees half of one. A failed write is only logged: the CLI that reads no
// progress never kills for want of it.
func ReportTeardownProgress(ctx context.Context, logger *zap.Logger, path string) {
	reportTeardownProgress(ctx, logger, path, nil)
}

// reportTeardownProgress is ReportTeardownProgress, whose reporter also ends
// once stop is closed (tests; nil: never).
func reportTeardownProgress(ctx context.Context, logger *zap.Logger, path string, stop <-chan struct{}) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) && logger != nil {
		logger.Debug("failed to remove a stale teardown progress file", zap.String("path", path), zap.Error(err))
	}
	every := teardownBeat // read here, not in the goroutine, which outlives a test
	go func() {
		select {
		case <-ctx.Done():
		case <-stop:
			return
		}
		beats := time.NewTicker(every)
		defer beats.Stop()
		var beat uint64
		logged := false
		for {
			p := TeardownProgress{Seq: teardownSeq.Load(), Beat: beat, Done: teardownDone.Load()}
			if err := writeTeardownProgress(path, p); err != nil && !logged && logger != nil {
				logged = true
				logger.Debug("failed to write the agent's teardown progress", zap.String("path", path), zap.Error(err))
			}
			select {
			case <-beats.C:
				beat++
			case <-teardownKick:
			case <-stop:
				return
			}
		}
	}()
}

// writeTeardownProgress replaces the file at path with p.
func writeTeardownProgress(path string, p TeardownProgress) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	// Readable by the CLI that asked for it, which may not be this user (an
	// agent elevated by sudo writes for the user who ran keploy).
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// ParseTeardownProgress reads what ReportTeardownProgress wrote.
func ParseTeardownProgress(b []byte) (TeardownProgress, error) {
	var p TeardownProgress
	err := json.Unmarshal(b, &p)
	return p, err
}
