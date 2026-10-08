package utils

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"go.uber.org/zap"
)

type flushCounter struct{ n atomic.Int64 }

func (f *flushCounter) Configure(sentry.ClientOptions) {}
func (f *flushCounter) SendEvent(*sentry.Event)        {}
func (f *flushCounter) Flush(time.Duration) bool {
	f.n.Add(1)
	return true
}

// Recover runs on every clean return of the goroutines that defer it; a flush
// there waits on Sentry's one transport worker for an event never captured.
func TestRecoverDoesNotFlushSentryWithoutAPanic(t *testing.T) {
	tr := &flushCounter{}
	prev := sentry.CurrentHub().Client()
	if err := sentry.Init(sentry.ClientOptions{Dsn: "https://public@sentry.invalid/1", Transport: tr}); err != nil {
		t.Fatalf("sentry.Init: %v", err)
	}
	t.Cleanup(func() { sentry.CurrentHub().BindClient(prev) })

	func() { defer Recover(zap.NewNop()) }()
	if got := tr.n.Load(); got != 0 {
		t.Fatalf("Recover flushed Sentry %d time(s) on a clean return", got)
	}
}
