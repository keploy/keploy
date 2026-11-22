package supervisor

import (
	"context"
	"strings"
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// An answer the client never read (models.Mock.IsUndelivered) that a session
// leaves out, as a retired parser's session leaves out each mock its parser
// still emits, records no span: no replay serves it and no test case needs it,
// so a test case recorded over it is not left out, whether the caller keeps
// its own spans (Session.Orphans: a DaemonSet agent, per pod) or the manager
// keeps them (RecordOrphanWindow). It is still counted and said, with
// spanRecorded false and a next_step that says no test case is left out for
// it or lacks it. The answer read beside it is spanned over its own times, as
// every mock left out is. Under either switch.
func TestATaggedAnswerARetiredSessionLeavesOutRecordsNoSpan(t *testing.T) {
	for _, on := range []string{"0", "1"} {
		t.Run(provenswitch.Env+"="+on, func(t *testing.T) {
			t.Setenv(provenswitch.Env, on)
			for _, unread := range []bool{true, false} {
				for _, viaOrphans := range []bool{true, false} {
					leftOutTaggedOrNot(t, unread, viaOrphans)
				}
			}
		})
	}
}

func leftOutTaggedOrNot(t *testing.T, unread, viaOrphans bool) {
	t.Helper()
	prev := leftOutWarns
	leftOutWarns = NewWarnLimiters(leftOutWarnEvery) // so this one is the WARN
	defer func() { leftOutWarns = prev }()
	core, logs := observer.New(zapcore.DebugLevel)
	mgr := syncMock.New(zap.NewNop())
	spans := &rawSpans{}
	s := &Session{RouteMocksViaSyncMock: true, Mgr: mgr, Logger: zap.New(core), Ctx: context.Background()}
	if viaOrphans {
		s.Orphans = spans
	}
	s.retire(leftOutParserHung)
	at := time.Now().Add(-5 * time.Second)
	m := &models.Mock{Kind: models.Kind("Kafka"), Name: "fetch"}
	m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = at, at.Add(500*time.Millisecond) // a long poll
	if unread {
		m.MarkUndelivered()
	}
	counted := mgr.MocksLeftOut()
	left, err := s.EmitMockOutcome(m, false)
	if !left || err != nil {
		t.Fatalf("unread %v, orphans %v: left out %v, err %v; want the retired session to leave it out", unread, viaOrphans, left, err)
	}
	if n := mgr.MocksLeftOut() - counted; n != 1 {
		t.Fatalf("unread %v, orphans %v: counted %d, want the mock left out once", unread, viaOrphans, n)
	}
	// A test case recorded inside the answer's span.
	from, to := at.Add(100*time.Millisecond), at.Add(200*time.Millisecond)
	orphaned, _ := mgr.WasMockOrphanedInWindow(from, to)
	switch {
	case viaOrphans && unread && len(spans.got) != 0:
		t.Fatalf("orphans: a tagged answer left out spanned %v; want no span", spans.got)
	case viaOrphans && !unread && (len(spans.got) != 1 || !spans.got[0][0].Equal(at) || !spans.got[0][1].Equal(at.Add(500*time.Millisecond))):
		t.Fatalf("orphans: an answer read left out spanned %v; want its own [request, response]", spans.got)
	case viaOrphans && orphaned:
		t.Fatalf("orphans: the manager left out a test case; want the caller's spans alone")
	case !viaOrphans && orphaned == unread:
		t.Fatalf("manager, unread %v: a test case inside the answer's span left out %v; want it left out only for an answer read", unread, orphaned)
	}
	w := logs.FilterLevelExact(zapcore.WarnLevel).FilterMessage(LeftOutWarnMsg).All()
	if len(w) != 1 {
		t.Fatalf("unread %v, orphans %v: %d left-out WARNs, want 1", unread, viaOrphans, len(w))
	}
	fields := w[0].ContextMap()
	if got := fields["spanRecorded"]; got != !unread {
		t.Fatalf("unread %v, orphans %v: spanRecorded is %v, want %v", unread, viaOrphans, got, !unread)
	}
	next, _ := fields["next_step"].(string)
	class := leftOutClass(leftOutCause(leftOutParserHung))
	want := leftOutNextStep(class, true)
	if unread {
		want = leftOutUnreadNextStep(class)
		// It still ends with what fits the cause, as every left-out mock's does.
		if !strings.Contains(next, "never read") || !strings.Contains(next, "no test case is left out for it or lacks it") ||
			strings.Contains(next, "no_mocks") || !strings.HasSuffix(next, " "+leftOutKnob(class)) {
			t.Fatalf("the next_step of a tagged answer left out is %q, want one that says no test case is left out for it or lacks it", next)
		}
	}
	if next != want {
		t.Fatalf("unread %v, orphans %v: next_step %q, want %q", unread, viaOrphans, next, want)
	}
}
