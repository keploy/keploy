package app

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/platform/docker"
)

// forceComposeLibrary has the App take the in-process library path's
// decisions for one row, in a build that registers no runner, which is every
// OSS build.
func forceComposeLibrary(t *testing.T) {
	t.Helper()
	was := composeLibrarySupported
	composeLibrarySupported = func() bool { return true }
	t.Cleanup(func() { composeLibrarySupported = was })
}

// fakeComposeStack is a composeStack that answers from canned data, so the
// in-memory compose path keeps unit coverage without a docker daemon.
type fakeComposeStack struct {
	ps       []docker.ServiceState
	psCalls  int
	downCall int
	// downGrace is the stop grace the last Down was given; downErr is what
	// Down answers.
	downGrace time.Duration
	downErr   error
	upErr     error
	// upCalls counts Up, which a run calls from its own goroutine.
	upCalls atomic.Int32
	// upBlocksUntil, when non-nil, makes Up block until it is closed or the
	// context ends — the shape a foreground `up` has.
	upBlocksUntil chan struct{}
	// upStarted, when non-nil, is closed once Up has been called and holds
	// its context (upContext).
	upStarted chan struct{}
	upMu      sync.Mutex
	upCtx     context.Context
	// downBlocks makes Down block until its context ends, as a down of a
	// project that never moves does; downCtxEnded says it ended.
	downBlocks   bool
	downCtxEnded atomic.Bool
	// noProject makes the runner name no project.
	noProject bool
}

// upContext is the context the run's Up was given (nil before Up).
func (f *fakeComposeStack) upContext() context.Context {
	f.upMu.Lock()
	defer f.upMu.Unlock()
	return f.upCtx
}

func (f *fakeComposeStack) ProjectName() string {
	if f.noProject {
		return ""
	}
	return "fake"
}

func (f *fakeComposeStack) Up(ctx context.Context, _ docker.ComposeUpOptions, _, _ io.Writer) error {
	f.upMu.Lock()
	f.upCtx = ctx
	f.upMu.Unlock()
	if f.upCalls.Add(1) == 1 && f.upStarted != nil {
		close(f.upStarted)
	}
	if f.upBlocksUntil != nil {
		select {
		case <-f.upBlocksUntil:
		case <-ctx.Done():
		}
	}
	return f.upErr
}

func (f *fakeComposeStack) Down(ctx context.Context, grace time.Duration) error {
	f.downCall++
	f.downGrace = grace
	if f.downBlocks {
		<-ctx.Done()
		f.downCtxEnded.Store(true)
		return ctx.Err()
	}
	return f.downErr
}

func (f *fakeComposeStack) Ps(context.Context) ([]docker.ServiceState, error) {
	f.psCalls++
	return f.ps, nil
}

func (f *fakeComposeStack) ContainerIDsForService(_ context.Context, service string) ([]string, error) {
	var ids []string
	for _, s := range f.ps {
		if s.Service == service {
			ids = append(ids, s.ID)
		}
	}
	return ids, nil
}
