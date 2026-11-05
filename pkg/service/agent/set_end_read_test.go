package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// readKey marks the context a test hands a read, so a fake can tell that a
// hop passed the read's own context on.
type readKey struct{}

func readCtx(ctx context.Context) context.Context {
	return context.WithValue(ctx, readKey{}, "read")
}

// setEndProxy notes, in order, the parsers' set-end filing (with the read's
// context, or "file:other ctx") and the reads, and files with fileErr.
type setEndProxy struct {
	coreAgent.Proxy // nil: any other call panics loudly
	did             *[]string
	fileErr         error
}

func (p setEndProxy) FileAtSetEnd(ctx context.Context) error {
	if ctx.Value(readKey{}) != "read" {
		*p.did = append(*p.did, "file:other ctx")
		return p.fileErr
	}
	*p.did = append(*p.did, "file")
	return p.fileErr
}

func (p setEndProxy) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) {
	*p.did = append(*p.did, "read")
	return nil, nil
}

// A set's end read has the parsers file what they hold for a test of the set
// before it reads the misses, so the read takes them; a test's end read
// does not.
func TestGetMockErrors_ASetEndReadFilesWhatTheParsersHoldFirst(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want []string
	}{
		{"a test's end", readCtx(context.Background()), []string{"read"}},
		{"the set's end", readCtx(models.WithSetEndRead(context.Background())), []string{"file", "read"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var did []string
			a := &Agent{logger: zap.NewNop(), Proxy: setEndProxy{did: &did}}
			if _, err := a.GetMockErrors(tc.ctx); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(did, tc.want) {
				t.Fatalf("did %v, want %v", did, tc.want)
			}
		})
	}
}

// A set's end read whose parsers could not file every call they hold (one
// panicked, or the read's context ended) is unread: the read cannot vouch
// for every miss, and the set is not judged on it.
func TestGetMockErrors_ASetEndReadThatCouldNotFileIsUnread(t *testing.T) {
	var did []string
	a := &Agent{logger: zap.NewNop(), Proxy: setEndProxy{did: &did, fileErr: errors.New("a parser panicked: parser bug")}}
	_, err := a.GetMockErrors(readCtx(models.WithSetEndRead(context.Background())))
	if !errors.Is(err, models.ErrUnmatchedCallsUnread) || !strings.Contains(err.Error(), "parser bug") {
		t.Fatalf("read said %v, want unread naming why", err)
	}
	if want := []string{"file", "read"}; !slices.Equal(did, want) {
		t.Fatalf("did %v, want %v", did, want)
	}
}

// settlingSetEndProxy is settleProxy noting, in order, the settle's first
// read of what the parsers hold, the parsers' set-end filing, and the read.
type settlingSetEndProxy struct {
	*settleProxy
	once *sync.Once
	did  *[]string
}

func (p settlingSetEndProxy) InFlightState(answers bool) (int, []string) {
	p.once.Do(func() { *p.did = append(*p.did, "settle") })
	return p.settleProxy.InFlightState(answers)
}

func (p settlingSetEndProxy) FileAtSetEnd(context.Context) error {
	*p.did = append(*p.did, "file")
	return nil
}

func (p settlingSetEndProxy) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) {
	*p.did = append(*p.did, "read")
	return nil, nil
}

// Under the switch a replay's set end read settles the application's calls
// before the parsers file what they hold: a call held while the read settles
// is filed with the rest, so the read takes it.
func TestGetMockErrors_ASetEndReadFilesAfterTheSettle(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	a, h, p := settleAgent()
	a.config = config.New()
	a.config.Agent.Mode = models.MODE_TEST
	h.established = 1
	p.set(1, 0)
	var did []string
	a.Proxy = settlingSetEndProxy{settleProxy: p, once: &sync.Once{}, did: &did}
	if _, err := a.GetMockErrors(models.WithSetEndRead(context.Background())); err != nil {
		t.Fatal(err)
	}
	if want := []string{"settle", "file", "read"}; !slices.Equal(did, want) {
		t.Fatalf("did %v, want %v", did, want)
	}
}

// optsProxy notes the options the agent hands its proxy's Mock.
type optsProxy struct {
	*workerPool
	got *models.OutgoingOptions
}

func (p optsProxy) Mock(_ context.Context, opts models.OutgoingOptions) error {
	*p.got = opts
	return nil
}

// filingOptsProxy is optsProxy that files at a set's end read.
type filingOptsProxy struct{ optsProxy }

func (filingOptsProxy) FileAtSetEnd(context.Context) error { return nil }

// A session's client reads its sets' ends only to a Proxy that files what
// its parsers hold at that read (SetEndFilingProxy): through one that cannot,
// such as a wrapper that forwards Mock alone, no parser may hold a call
// nothing would file.
func TestMockOutgoing_SaysTheClientReadsSetEndsOnlyToAProxyThatFilesThem(t *testing.T) {
	for _, tc := range []struct {
		files, reads, want bool
	}{
		{files: true, reads: true, want: true},
		{files: true, reads: false, want: false},
		{files: false, reads: true, want: false},
		{files: false, reads: false, want: false},
	} {
		a := newClockAgent(t)
		var got models.OutgoingOptions
		op := optsProxy{workerPool: &workerPool{}, got: &got}
		a.Proxy = op
		if tc.files {
			a.Proxy = filingOptsProxy{op}
		}
		if err := a.MockOutgoing(context.Background(), models.OutgoingOptions{ReadsSetEnds: tc.reads}); err != nil {
			t.Fatal(err)
		}
		if got.ReadsSetEnds != tc.want {
			t.Fatalf("files %v, reads %v: the proxy was told ReadsSetEnds=%v, want %v", tc.files, tc.reads, got.ReadsSetEnds, tc.want)
		}
	}
}
