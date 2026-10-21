package utils

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"go.keploy.io/server/v3/utils/logcontent/logtest"
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

// eventKeeper keeps the events Sentry is sent.
type eventKeeper struct {
	flushCounter
	events []*sentry.Event
}

func (k *eventKeeper) SendEvent(e *sentry.Event) { k.events = append(k.events, e) }

// A recovered panic's value is what the code that raised it made it, the
// payload a decoder could not read as often as not. Recover logs it as
// logcontent.Panic allows, stops keploy for a reason of its own, and sends
// Sentry, which is off the machine, only what logcontent.PanicText says of
// it, whatever KEPLOY_LOG_CONTENT says.
func TestRecoverLogsAndReportsAPanicsValueAsLogcontentAllows(t *testing.T) {
	const canary = "kp-canary-recover-5e2f"
	logtest.Each(t, func(t *testing.T, on bool) {
		tr := &eventKeeper{}
		prev := sentry.CurrentHub().Client()
		if err := sentry.Init(sentry.ClientOptions{Dsn: "https://public@sentry.invalid/1", Transport: tr}); err != nil {
			t.Fatalf("sentry.Init: %v", err)
		}
		t.Cleanup(func() { sentry.CurrentHub().BindClient(prev) })
		prevCancel := cancel
		SetCancel(func() {})
		t.Cleanup(func() { SetCancel(prevCancel) })

		logger, logs := logtest.Observe()
		func() {
			defer Recover(logger)
			panic("bad frame " + canary)
		}()
		logtest.Check(t, logs, canary, on, "Recovered from panic")
		stops := logs.FilterMessage("stopping Keploy").All()
		if len(stops) != 1 || strings.Contains(fmt.Sprint(stops[0].ContextMap()), canary) {
			t.Fatalf("keploy was stopped %d time(s), or for the panic's value: %v", len(stops), stops)
		}
		if len(tr.events) == 0 {
			t.Fatal("no crash report was sent")
		}
		for _, e := range tr.events {
			for _, x := range e.Exception {
				if strings.Contains(x.Value, canary) {
					t.Fatalf("the crash report carries the panic's value: %q", x.Value)
				}
			}
			for k, v := range e.Extra {
				if strings.Contains(fmt.Sprint(v), canary) {
					t.Fatalf("the crash report's %s carries the panic's value", k)
				}
			}
		}
	})
}
