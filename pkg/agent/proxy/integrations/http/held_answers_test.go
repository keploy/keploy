package http

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A recording the replay keeps for every test is served, and held, each time
// the app asks for it. Of the answers served from one recording, only the
// newest holds its connection past the app's FIN: a later answer of the same
// recording held ends the hold before it (heldAnswers).

func ended(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

func TestHeldAnswers_ALaterAnswerOfARecordingEndsTheHoldBeforeIt(t *testing.T) {
	var s heldAnswers
	ctx := context.Background()

	first, doneFirst := s.hold(ctx, "mock-3")
	other, doneOther := s.hold(ctx, "mock-4")
	if ended(first) || ended(other) {
		t.Fatal("a hold ended when an answer of another recording was held")
	}
	second, doneSecond := s.hold(ctx, "mock-3")
	if !ended(first) {
		t.Fatal("the hold of mock-3's first answer outlasts its second answer's: want it ended")
	}
	if ended(second) || ended(other) {
		t.Fatal("holding mock-3's second answer ended its own hold, or mock-4's")
	}
	// The first answer's hold, over now, does not take the second's place.
	doneFirst()
	third, doneThird := s.hold(ctx, "mock-3")
	if !ended(second) {
		t.Fatal("the hold of mock-3's second answer outlasts its third answer's once the first's was over: " +
			"the first's end unregistered the second")
	}
	doneSecond()
	doneThird()
	doneOther()
	if !ended(third) || !ended(other) {
		t.Fatal("a hold's context outlives its done")
	}
	left := 0
	s.newest.Range(func(any, any) bool { left++; return true })
	if left != 0 {
		t.Fatalf("%d recording(s) still registered once every hold is over; want none", left)
	}

	// Answers of recordings with no name are not told apart: none ends another's hold.
	a, doneA := s.hold(ctx, "")
	b, doneB := s.hold(ctx, "")
	defer doneA()
	defer doneB()
	if ended(a) || ended(b) {
		t.Fatal("an answer of a recording with no name ended another's hold")
	}

	// A hold's context ends with the run's.
	run, cancel := context.WithCancel(ctx)
	c, doneC := s.hold(run, "mock-5")
	defer doneC()
	cancel()
	if !ended(c) {
		t.Fatal("a hold outlives the run's end")
	}
}

// Through decodeHTTP, one handler serving one retained recording (a config
// mock) to an app that asks again and again: an app that closed each
// connection once it had read what it read at record has that connection held
// past its FIN only until it asks again, so one connection is held, not one
// per ask. An app that still has the earlier connection open when it asks
// again keeps it: that hold ends at the app's close, as before. Held where
// the app's socket cannot be asked (Windows) the same: the next serve ends
// the hold, not a look at the socket.
func TestAHoldPastTheFINEndsWhenItsRecordingIsServedAgain(t *testing.T) {
	const request = "GET /charge HTTP/1.1\r\nHost: pay.example\r\n\r\n"
	const head = "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\n"
	const asks = 4
	for _, kind := range []struct {
		name  string
		fault models.Fault
	}{
		{"timeout", models.Fault{Kind: models.FaultTimeout, AfterHead: true, BodyBytes: 5, AppEndedFirst: true}},
		{"stopped_reading", models.Fault{Kind: models.FaultStoppedReading, AfterHead: true, BodyBytes: 5, ContentLength: 10, AppEndedFirst: true}},
	} {
		for _, proven := range []string{"", "1"} {
			t.Run(kind.name+"/KEPLOY_PROVEN="+proven, func(t *testing.T) {
				t.Setenv("KEPLOY_PROVEN", proven)
				f := kind.fault
				m := &models.Mock{Name: "mock-config", Kind: models.HTTP, Spec: models.MockSpec{
					Metadata: map[string]string{"type": "config"},
					HTTPReq: &models.HTTPReq{Method: "GET", URL: "http://pay.example/charge", ProtoMajor: 1, ProtoMinor: 1,
						Header: map[string]string{"Host": "pay.example"}},
					HTTPResp: &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Length": "10"}, Body: "0123456789"},
					Fault:    &f,
				}}
				m.TestModeInfo.Lifetime = models.LifetimeSession
				h := &HTTP{Logger: zap.NewNop()}
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				type served struct {
					app   net.Conn
					marks *idleMarks
					done  chan error
				}
				ask := func(i int) served {
					app, proxy := tcpPair(t)
					if _, err := app.Write([]byte(request)); err != nil {
						t.Fatal(err)
					}
					if _, err := readFullConn(proxy, len(request)); err != nil {
						t.Fatal(err)
					}
					marks := newIdleMarks()
					done := make(chan error, 1)
					go func() {
						err := h.decodeHTTP(integrations.WithRequestHolder(ctx, marks), []byte(request), proxy, nil,
							&mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}, models.OutgoingOptions{})
						_ = proxy.Close()
						done <- err
					}()
					if got, err := readsUntilDeadline(app, 200*time.Millisecond); got != head+"01234" || !errors.Is(err, os.ErrDeadlineExceeded) {
						t.Fatalf("ask %d: the app read %q, then %v; want %q, then nothing until its own timeout", i, got, err, head+"01234")
					}
					return served{app, marks, done}
				}
				awaitDone := func(s served, what string) {
					t.Helper()
					select {
					case err := <-s.done:
						if err != nil && (ctx.Err() == nil || !errors.Is(err, context.Canceled)) {
							t.Fatalf("decodeHTTP = %v %s; want nil", err, what)
						}
					case <-time.After(10 * time.Second): // turns a hang into a failure
						t.Fatalf("the connection was still held %s", what)
					}
				}
				awaitPastFIN := func(s served, i int) {
					t.Helper()
					select {
					case <-s.marks.marked:
					case err := <-s.done:
						t.Fatalf("ask %d: decodeHTTP returned %v at the app's close, before the recording was served again", i, err)
					case <-time.After(10 * time.Second): // turns a hang into a failure
						t.Fatalf("ask %d: the serve neither held the connection past the app's close nor returned", i)
					}
				}

				// The app closes each connection, then asks again.
				prev := ask(0)
				_ = prev.app.Close()
				awaitPastFIN(prev, 0)
				for i := 1; i < asks; i++ {
					next := ask(i)
					awaitDone(prev, fmt.Sprintf("once the recording was served again (ask %d)", i))
					select {
					case <-prev.marks.released:
					default:
						t.Fatalf("ask %d: the earlier connection was let go with its idle mark still taken", i)
					}
					_ = next.app.Close()
					awaitPastFIN(next, i)
					prev = next
				}
				// The newest is held past its app's close until the run's end.
				select {
				case err := <-prev.done:
					t.Fatalf("decodeHTTP of the last ask returned %v before the run's end; want it held", err)
				case <-prev.marks.released:
					t.Fatal("the last ask's idle mark was released before the run's end")
				default:
				}

				// An app that still has its connection open when it asks again
				// keeps it held: its own close ends that hold, as before.
				open := ask(asks)
				awaitDone(prev, "once the recording was served again")
				again := ask(asks + 1)
				select {
				case err := <-open.done:
					t.Fatalf("decodeHTTP returned %v while its app still had the connection open; want it held until the app's close", err)
				default:
				}
				if markedAt(open.marks) {
					t.Fatal("the connection the app still has open was marked idle")
				}
				_ = open.app.Close()
				awaitDone(open, "at the app's close, once a later answer of its recording was held")
				_ = again.app.Close()
				awaitPastFIN(again, asks+1)

				cancel()
				awaitDone(again, "at the run's end")
			})
		}
	}
}
