package proxy

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A client of a port the recording never saw that connects waiting to be
// greeted and gives up only after the replay began the next test made its
// call where it connected: its miss carries the round it arrived in, so the
// next test's end never charges it. Arrived in a test, it names that test
// and is in it; arrived between tests, it is outside every test and marked
// before the next test's view was installed.
func TestASilentClientsMissCarriesTheRoundItArrivedIn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		arrive string // the test open as the client connects; "" for the pool view
	}{
		{"in a test", "T1"},
		{"between tests", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mm, ms := s12Set(t)
			p := testProxy(t)
			p.errChannel = make(chan error, 4)
			p.mockManager = mm
			recordedMySQL(p, "127.0.0.1:3306", "")
			mm.ServeExactPool(models.PoolKeyNone, nil)
			var epoch, bounds uint64
			if tc.arrive != "" {
				mm.ServeExactTest(tc.arrive, []*models.Mock{ms["own"]})
				var ok bool
				if epoch, bounds, ok = mm.ExactTestInstalled(tc.arrive); !ok {
					t.Fatalf("precondition: %s's install is not known", tc.arrive)
				}
			}
			client := newSilentClient()
			done := replayProbe(context.Background(), p, client, "10.9.9.9:13306", zap.NewNop())
			if probe := readOrVerdict(t, done, client); probe != nil {
				t.Fatalf("the probe decided (reason %q) before the client did anything", probe.Reason)
			}
			// The open test ends and the next begins before the client
			// gives up waiting for its greeting.
			mm.ServeExactPool(models.PoolKeyNone, nil)
			mm.ServeExactTest("T2", []*models.Mock{ms["other"]})
			nextEpoch, nextBounds, ok := mm.ExactTestInstalled("T2")
			if !ok {
				t.Fatal("precondition: T2's install is not known")
			}
			_ = client.Close()
			if probe := awaitEvent(t, done, "the probe's verdict once the client left"); probe.IsMySQL {
				t.Fatalf("reason %q: an endpoint the recording cannot be tied to was served as MySQL", probe.Reason)
			}
			var err error
			select {
			case err = <-p.errChannel:
			default:
				t.Fatal("no miss was reported for the connection that waited to be greeted")
			}
			pe, isPE := err.(models.ParserError)
			if !isPE || pe.MismatchReport == nil {
				t.Fatalf("reported %v; want a miss with its report", err)
			}
			r := pe.MismatchReport
			if r.Mark != models.RoundMarkedSettled || !r.MarkRound || r.MarkedTest != tc.arrive || r.MarkedInTest != (tc.arrive != "") {
				t.Fatalf("the miss was marked %q (round %v, test %q, in a test %v); want settled, with its round, naming %q",
					r.Mark, r.MarkRound, r.MarkedTest, r.MarkedInTest, tc.arrive)
			}
			mark := models.MissMark{Epoch: r.MarkEpoch, Bounds: r.MarkBounds, Test: r.MarkedTest, Known: true}
			if !mark.Before(nextEpoch, nextBounds) {
				t.Fatalf("the miss was marked at staging %d, bound %d: not before T2's install (%d, %d), so T2's end would be charged with it",
					r.MarkEpoch, r.MarkBounds, nextEpoch, nextBounds)
			}
			if tc.arrive != "" {
				if r.MarkEpoch != epoch || r.MarkBounds != bounds {
					t.Fatalf("the miss was marked at staging %d, bound %d; want %s's install (%d, %d)", r.MarkEpoch, r.MarkBounds, tc.arrive, epoch, bounds)
				}
				if pe.MissKind != models.MissInTest {
					t.Fatalf("the miss is of kind %q; want %q, the kind of the test it arrived in", pe.MissKind, models.MissInTest)
				}
			} else if pe.MissKind != models.MissOutsideTest {
				t.Fatalf("the miss is of kind %q; want %q: it arrived between tests", pe.MissKind, models.MissOutsideTest)
			}
		})
	}
}
