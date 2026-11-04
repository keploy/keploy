package app

import (
	"context"
	"io"
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
}

func (f *fakeComposeStack) ProjectName() string { return "fake" }

func (f *fakeComposeStack) Up(ctx context.Context, _ docker.ComposeUpOptions, _, _ io.Writer) error {
	f.upCalls.Add(1)
	if f.upBlocksUntil != nil {
		select {
		case <-f.upBlocksUntil:
		case <-ctx.Done():
		}
	}
	return f.upErr
}

func (f *fakeComposeStack) Down(_ context.Context, grace time.Duration) error {
	f.downCall++
	f.downGrace = grace
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
