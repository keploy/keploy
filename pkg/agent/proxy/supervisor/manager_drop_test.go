package supervisor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A mock the session's manager drops as the session hands it over, for memory
// pressure (its request was made while the memory guard paused recording) or
// for the recording's output closed, is said lost by EmitMockOutcome, so a
// parser that says it kept a mock says it only of one the recording took. The
// session does not count it again as left out (the manager counts it), and
// the manager says which mock it dropped and why, at WARN or above. A mock the
// manager takes is not said lost, and reaches the output. Through EmitMock's
// path and the shutdown path both.
func TestAMockTheManagerDropsAsItIsHandedOverIsSaidLost(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*syncMock.SyncMockManager, chan *models.Mock)
		lost bool
	}{
		{"memory pressure", func(m *syncMock.SyncMockManager, ch chan *models.Mock) {
			m.SetOutputChannel(ch)
			m.SetMemoryPressure(true)
		}, true},
		{"output closed", func(m *syncMock.SyncMockManager, ch chan *models.Mock) {
			m.SetOutputChannel(ch)
			m.CloseOutChan()
		}, true},
		{"taken", func(m *syncMock.SyncMockManager, ch chan *models.Mock) {
			m.SetOutputChannel(ch)
		}, false},
	} {
		for _, shutdown := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/shutdown=%v", tc.name, shutdown), func(t *testing.T) {
				core, logs := observer.New(zapcore.DebugLevel)
				mgr := syncMock.New(zap.New(core))
				ch := make(chan *models.Mock, 4)
				tc.set(mgr, ch)
				s := &Session{RouteMocksViaSyncMock: true, Mgr: mgr, Logger: zap.NewNop(), Ctx: context.Background(), ClientConnID: "conn-7"}
				// A per-test kind: every Generic mock derives as the session's,
				// which memory pressure keeps (lostToPressureLocked).
				m := &models.Mock{Kind: models.Kind("Kafka"), Name: "fetch"}
				m.Spec.ReqTimestampMock = time.Now()
				m.Spec.ResTimestampMock = m.Spec.ReqTimestampMock.Add(time.Millisecond)
				counted := mgr.MocksLeftOut()
				lost, err := s.EmitMockOutcome(m, shutdown)
				if err != nil {
					t.Fatalf("%s, shutdown %v: err %v", tc.name, shutdown, err)
				}
				// That the manager really dropped it comes first, apart from what
				// the caller is told: a mock kept is no silent drop.
				var got *models.Mock
				select {
				case got = <-ch:
				default:
				}
				if tc.lost && got != nil {
					t.Fatalf("%s, shutdown %v: the mock reached the output: the manager did not drop it", tc.name, shutdown)
				}
				if lost != tc.lost {
					t.Fatalf("%s, shutdown %v: lost %v, want %v", tc.name, shutdown, lost, tc.lost)
				}
				if n := mgr.MocksLeftOut() - counted; n != 0 {
					t.Fatalf("%s, shutdown %v: counted %d left out; the manager counts its own drop", tc.name, shutdown, n)
				}
				// A test case over a mock dropped for the output has nothing
				// else to leave it out. Pressure's spans cover its own.
				over, _ := mgr.WasMockOrphanedInWindow(m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock)
				if want := tc.name == "output closed"; over != want {
					t.Fatalf("%s, shutdown %v: spanned %v, want %v", tc.name, shutdown, over, want)
				}
				if !tc.lost {
					if got != m {
						t.Fatalf("%s, shutdown %v: the output got %v; want the mock", tc.name, shutdown, got)
					}
					return
				}
				said := false
				for _, e := range logs.All() {
					f := e.ContextMap()
					if e.Level >= zapcore.WarnLevel && f["mock_kind"] == "Kafka" && f["connID"] == "conn-7" && f["why"] != nil && f["why"] != "" {
						said = true
					}
				}
				if !said {
					t.Fatalf("%s, shutdown %v: no WARN or above names the dropped mock (kind, connection) and why: %v", tc.name, shutdown, logs.All())
				}
			})
		}
	}
}

// An answer the client never read that the manager drops is lost, so no parser
// says it kept it, but it costs no test case: it spans nothing, whether the
// manager keeps the spans or the caller does (Session.Orphans).
func TestAnUnreadAnswerTheManagerDropsSpansNothing(t *testing.T) {
	for _, viaOrphans := range []bool{false, true} {
		mgr := syncMock.New(zap.NewNop())
		mgr.SetOutputChannel(make(chan *models.Mock, 1))
		mgr.CloseOutChan()
		spans := &rawSpans{}
		s := &Session{RouteMocksViaSyncMock: true, Mgr: mgr, Logger: zap.NewNop(), Ctx: context.Background()}
		if viaOrphans {
			s.Orphans = spans
		}
		m := &models.Mock{Kind: models.Kind("Kafka"), Name: "fetch"}
		m.Spec.ReqTimestampMock = time.Now()
		m.Spec.ResTimestampMock = m.Spec.ReqTimestampMock.Add(time.Millisecond)
		m.MarkUndelivered()
		lost, err := s.EmitMockOutcome(m, false)
		if !lost || err != nil {
			t.Fatalf("orphans %v: lost %v, err %v; want it lost", viaOrphans, lost, err)
		}
		over, _ := mgr.WasMockOrphanedInWindow(m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock)
		if over || len(spans.got) != 0 {
			t.Fatalf("orphans %v: an unread answer spanned (manager %v, caller %v); want no span", viaOrphans, over, spans.got)
		}
	}
}

// A mock the manager drops for its output, closed or at its byte bound, has
// nothing else to leave out a test case over it: the session spans it over
// its own times, request to response, where the session's spans go
// (Session.Orphans), as a mock left out is. Its parser has consumed its input,
// so the session's pending work is cleared (OnPendingCleared), as for a mock
// taken: left armed, the hang watchdog would abort the connection once it
// idles. Memory pressure's own spans cover its drops, which span nothing here.
func TestAMockTheManagerDropsIsSpannedOverItsTimesAndClearsPendingWork(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*testing.T, *syncMock.SyncMockManager)
		drop syncMock.MockDrop
		span bool
	}{
		{"output closed", func(_ *testing.T, m *syncMock.SyncMockManager) {
			m.SetOutputChannel(make(chan *models.Mock, 1))
			m.CloseOutChan()
		}, syncMock.MockDroppedOutputClosed, true},
		{"byte bound", func(t *testing.T, m *syncMock.SyncMockManager) {
			ch := make(chan *models.Mock) // nothing reads it yet: its consumer stalls
			m.SetOutputChannel(ch)
			t.Cleanup(func() {
				go func() {
					for range ch {
					}
				}()
				m.CloseOutChan()
			})
			// The first in the drainer's hand, the next behind it: a mock
			// past the bound behind them is dropped.
			for range 2 {
				f := &models.Mock{Kind: models.Kind("Kafka"), Name: "filler"}
				f.Spec.ReqTimestampMock = time.Now()
				if d := m.AddMock(f); d.Dropped() {
					t.Fatalf("setup: a filler was dropped: %q", d)
				}
			}
		}, syncMock.MockDroppedOutputFull, true},
		{"memory pressure", func(_ *testing.T, m *syncMock.SyncMockManager) {
			m.SetOutputChannel(make(chan *models.Mock, 1))
			m.SetMemoryPressure(true)
		}, syncMock.MockDroppedUnderPressure, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := syncMock.New(zap.NewNop())
			tc.set(t, mgr)
			spans := &rawSpans{}
			cleared := 0
			s := &Session{RouteMocksViaSyncMock: true, Mgr: mgr, Logger: zap.NewNop(), Ctx: context.Background(), ClientConnID: "conn-8",
				Orphans: spans, OnPendingCleared: func() { cleared++ }}
			m := &models.Mock{Kind: models.Kind("Kafka"), Name: "fetch"}
			if tc.drop == syncMock.MockDroppedOutputFull {
				m.Spec.HTTPResp = &models.HTTPResp{Body: strings.Repeat("x", 64<<20)}
			}
			m.Spec.ReqTimestampMock = time.Now()
			m.Spec.ResTimestampMock = m.Spec.ReqTimestampMock.Add(3 * time.Millisecond)
			req, res := m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock
			drops := mgr.DropCount()
			lost, err := s.EmitMockOutcome(m, false)
			if !lost || err != nil {
				t.Fatalf("lost %v, err %v; want it lost", lost, err)
			}
			if tc.drop != syncMock.MockDroppedUnderPressure && mgr.DropCount() != drops+1 {
				t.Fatalf("setup: the manager counted %d drops for the output, want 1 (%s)", mgr.DropCount()-drops, tc.drop)
			}
			if cleared != 1 {
				t.Fatalf("pending work cleared %d times, want once: the parser consumed the dropped mock's input", cleared)
			}
			want := [][2]time.Time{{req, res}}
			if !tc.span {
				want = nil
			}
			if fmt.Sprint(spans.got) != fmt.Sprint(want) {
				t.Fatalf("spanned %v, want %v", spans.got, want)
			}
		})
	}
}
