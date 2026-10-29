package utils

import (
	"os"
	"runtime"
	"strings"
	"sync"
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

// eventRecorder is a Sentry transport that keeps the events it is sent.
type eventRecorder struct {
	mu     sync.Mutex
	events []*sentry.Event
}

func (e *eventRecorder) Configure(sentry.ClientOptions) {}
func (e *eventRecorder) SendEvent(ev *sentry.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
}
func (e *eventRecorder) Flush(time.Duration) bool { return true }

// A crash report carries the end of this run's log, never all of it. The log
// holds up to 100 MiB (log.FileCapBytes), the native agent's lines and
// --debug's among them: a panic handler must not read that into memory, and a
// Sentry event cannot carry it (its limit is 1 MB).
func TestHandleRecoveryAttachesABoundedTailOfTheLog(t *testing.T) {
	tr := &eventRecorder{}
	prev := sentry.CurrentHub().Client()
	if err := sentry.Init(sentry.ClientOptions{Dsn: "https://public@sentry.invalid/1", Transport: tr}); err != nil {
		t.Fatalf("sentry.Init: %v", err)
	}
	t.Cleanup(func() {
		sentry.ConfigureScope(func(s *sentry.Scope) { s.RemoveExtra("logfile") })
		sentry.CurrentHub().BindClient(prev)
	})

	// A 100 MiB log, sparse but for the lines it ends with.
	f, err := os.CreateTemp(t.TempDir(), "keploy-logs-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if err := f.Truncate(100 << 20); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 2); err != nil {
		t.Fatal(err)
	}
	const last = "the line the run logged last\n"
	if _, err := f.WriteString(strings.Repeat("a line of the run\n", 1000) + last); err != nil {
		t.Fatal(err)
	}
	prevLog := LogFile
	LogFile = f
	t.Cleanup(func() { LogFile = prevLog })

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	HandleRecovery(zap.NewNop(), "boom", "Recovered from panic")
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 16<<20 {
		t.Errorf("the crash report allocated %d MiB for a 100 MiB log; want a bounded read of its end", grew>>20)
	}

	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.events) != 1 {
		t.Fatalf("Sentry was sent %d events, want the crash", len(tr.events))
	}
	got, _ := tr.events[0].Extra["logfile"].(string)
	if len(got) == 0 || len(got) > crashReportLogTail || !strings.HasSuffix(got, last) {
		t.Errorf("the crash report carries %d bytes of the log ending %q; want its end, at most %d bytes",
			len(got), got[max(0, len(got)-40):], crashReportLogTail)
	}
}
