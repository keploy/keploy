//go:build linux

package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/async"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// Replay gives the application the fault the upstream gave it at record: a
// call recorded through the relay on real sockets, written to mocks.yaml and
// read back, then served by keploy test on a real socket. What the
// application reads, and how its connection ends, is the same both times.

// seen is what the application read and how its read ended.
type seen struct {
	got string
	end string // "reset", "eof", "its own timeout", or the error
}

func endOf(err error) string {
	switch {
	case errors.Is(err, syscall.ECONNRESET):
		return "reset"
	case err == io.EOF:
		return "eof"
	case errors.Is(err, os.ErrDeadlineExceeded):
		return "its own timeout"
	}
	return err.Error()
}

// appGivesUp is the application's client timeout: it reads until its own
// deadline, then closes. The deadline is the application's, as its client's
// timeout is: keploy's side has no clock.
func appGivesUp(t *testing.T, app net.Conn) seen {
	t.Helper()
	_ = app.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	var got []byte
	buf := make([]byte, 4096)
	for {
		n, err := app.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			return seen{string(got), endOf(err)}
		}
	}
}

// throughDisk is m written as mocks.yaml writes it and read back.
func throughDisk(t *testing.T, m *models.Mock) *models.Mock {
	t.Helper()
	doc, err := mockdb.EncodeMock(m, zap.NewNop())
	if err != nil {
		t.Fatalf("EncodeMock: %v", err)
	}
	onDisk, err := yamlLib.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var readBack yaml.NetworkTrafficDoc
	if err := yamlLib.Unmarshal(onDisk, &readBack); err != nil {
		t.Fatal(err)
	}
	mocks, err := mockdb.DecodeMocks([]*yaml.NetworkTrafficDoc{&readBack}, zap.NewNop())
	if err != nil || len(mocks) != 1 {
		t.Fatalf("DecodeMocks = (%d mocks, %v)\n%s", len(mocks), err, onDisk)
	}
	return mocks[0]
}

// replaying serves mocks to faultReq on a real socket, as keploy test does:
// decodeHTTP, then the proxy's close of the application's connection once it
// returns. It returns the application's end and decodeHTTP's result.
func replaying(t *testing.T, h *HTTP, mocks ...*models.Mock) (net.Conn, <-chan error) {
	t.Helper()
	app, proxy := loopback(t)
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() {
		err := h.decodeHTTP(ctx, []byte(faultReq), proxy, nil,
			&mockMemDb{mocks: mocks, updateUnFilteredReturn: true}, models.OutgoingOptions{})
		_ = proxy.Close()
		returned <- err
	}()
	t.Cleanup(func() { cancel(); _ = app.Close() })
	return app, returned
}

func awaitReturn(t *testing.T, returned <-chan error) error {
	t.Helper()
	select {
	case err := <-returned:
		return err
	case <-time.After(10 * time.Second): // turns a hang into a failure
		t.Fatal("decodeHTTP did not return within 10s")
		return nil
	}
}

func TestReplayFault_TheApplicationSeesWhatItSawAtRecord(t *testing.T) {
	const head = "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\n"
	const chunkedHead = "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
	// An event stream the upstream cut inside its second event: its client
	// dispatched the first event alone. Its frames read back with both
	// events ended, so replay sends the bytes the upstream sent. One reset
	// after its second event ended: its frames read back 2 bytes short (no
	// space after a field's ':'), and replay sends the bytes it sent.
	const sseHead = "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 40\r\n\r\n"
	const sseCut = "data: a\n\ndata: b"
	const sseEnded = "data: a\n\ndata: b\n\n"
	for _, tc := range []struct {
		name   string
		record func(t *testing.T, c *relayedConn) seen
		app    func(t *testing.T, app net.Conn) seen
	}{{
		name: "a reset before any answer",
		record: func(t *testing.T, c *relayedConn) seen {
			resetConn(t, c.dest)
			<-c.relayDone
			_ = c.src.Close()
			return seenAt(t, c.app)
		},
		app: seenAt,
	}, {
		name: "a close with no answer",
		record: func(t *testing.T, c *relayedConn) seen {
			_ = c.dest.Close()
			return seenAt(t, c.app)
		},
		app: seenAt,
	}, {
		name: "a body cut short against its Content-Length",
		record: func(t *testing.T, c *relayedConn) seen {
			write(t, c.dest, c.dst, head+"0123456789")
			_ = c.dest.Close()
			return seenAt(t, c.app)
		},
		app: seenAt,
	}, {
		name: "a chunked body cut short",
		record: func(t *testing.T, c *relayedConn) seen {
			write(t, c.dest, c.dst, chunkedHead+"a\r\n0123456789\r\n")
			_ = c.dest.Close()
			return seenAt(t, c.app)
		},
		app: seenAt,
	}, {
		name: "a reset after the head",
		record: func(t *testing.T, c *relayedConn) seen {
			write(t, c.dest, c.dst, head+"01234")
			relayed(t, c.app, head+"01234")
			resetConn(t, c.dest)
			<-c.relayDone
			_ = c.src.Close()
			s := seenAt(t, c.app)
			s.got = head + "01234" + s.got
			return s
		},
		app: func(t *testing.T, app net.Conn) seen {
			relayed(t, app, head+"01234")
			s := seenAt(t, app)
			s.got = head + "01234" + s.got
			return s
		},
	}, {
		name: "an event stream reset inside its second event",
		record: func(t *testing.T, c *relayedConn) seen {
			write(t, c.dest, c.dst, sseHead+sseCut)
			relayed(t, c.app, sseHead+sseCut)
			resetConn(t, c.dest)
			<-c.relayDone
			_ = c.src.Close()
			s := seenAt(t, c.app)
			s.got = headersInOrder(sseHead + sseCut + s.got)
			return s
		},
		app: func(t *testing.T, app net.Conn) seen {
			// Read before the reset comes: a reset drops what the
			// application has not read.
			sent := make([]byte, len(sseHead+sseCut))
			if _, err := io.ReadFull(app, sent); err != nil {
				t.Fatalf("the application read %q, %v; want the head and %d body bytes before the reset", sent, err, len(sseCut))
			}
			s := seenAt(t, app)
			s.got = headersInOrder(string(sent) + s.got)
			return s
		},
	}, {
		name: "an event stream reset after its second event",
		record: func(t *testing.T, c *relayedConn) seen {
			write(t, c.dest, c.dst, sseHead+sseEnded)
			relayed(t, c.app, sseHead+sseEnded)
			resetConn(t, c.dest)
			<-c.relayDone
			_ = c.src.Close()
			s := seenAt(t, c.app)
			s.got = headersInOrder(sseHead + sseEnded + s.got)
			return s
		},
		app: func(t *testing.T, app net.Conn) seen {
			sent := make([]byte, len(sseHead+sseEnded))
			if _, err := io.ReadFull(app, sent); err != nil {
				t.Fatalf("the application read %q, %v; want the head and %d body bytes before the reset", sent, err, len(sseEnded))
			}
			s := seenAt(t, app)
			s.got = headersInOrder(string(sent) + s.got)
			return s
		},
	}, {
		name: "an event stream cut short against its Content-Length inside its second event",
		record: func(t *testing.T, c *relayedConn) seen {
			write(t, c.dest, c.dst, sseHead+sseCut)
			_ = c.dest.Close()
			s := seenAt(t, c.app)
			s.got = headersInOrder(s.got)
			return s
		},
		app: func(t *testing.T, app net.Conn) seen {
			s := seenAt(t, app)
			s.got = headersInOrder(s.got)
			return s
		},
	}, {
		name: "no answer until the application gave up",
		record: func(t *testing.T, c *relayedConn) seen {
			s := appGivesUp(t, c.app)
			appClosed(t, c)
			return s
		},
		app: appGivesUp,
	}, {
		name: "the head and part of the body, then nothing until the application gave up",
		record: func(t *testing.T, c *relayedConn) seen {
			write(t, c.dest, c.dst, head+"01234")
			s := appGivesUp(t, c.app)
			appClosed(t, c)
			return s
		},
		app: appGivesUp,
	}} {
		eitherSwitch(t, tc.name, func(t *testing.T) {
			c := newRelayedConn(t)
			c.start(t)
			write(t, c.app, c.src, faultReq)
			relayed(t, c.dest, faultReq)
			atRecord := tc.record(t, c)
			mocks, err := c.finish(t)
			m := oneMock(t, mocks, err)
			if m.Spec.Fault == nil {
				t.Fatalf("recorded no fault; the application saw %+v", atRecord)
			}

			h := &HTTP{Logger: zap.NewNop()}
			app, returned := replaying(t, h, throughDisk(t, m))
			atReplay := tc.app(t, app)
			if atReplay != atRecord {
				t.Errorf("at replay the application saw %+v; at record %+v (the mock's fault: %s)", atReplay, atRecord, m.Spec.Fault)
			}
			if m.Spec.Fault.Kind == models.FaultTimeout {
				// Held until the application closed: the connection is
				// its client's to end, not a clock's.
				select {
				case err := <-returned:
					t.Fatalf("decodeHTTP returned %v before the application closed", err)
				default:
				}
				_ = app.Close()
			}
			if err := awaitReturn(t, returned); err != nil {
				t.Errorf("decodeHTTP = %v, want nil: a recorded fault is a served mock, not a failure", err)
			}
		})
	}
}

// headersInOrder is answer with its head's header lines sorted: replay writes
// a recorded head's headers in an order of its own, which HTTP gives no
// meaning, so an answer is compared by its header lines, not their order.
func headersInOrder(answer string) string {
	head, body, ok := strings.Cut(answer, "\r\n\r\n")
	if !ok {
		return answer
	}
	lines := strings.Split(head, "\r\n")
	sort.Strings(lines[1:])
	return strings.Join(lines, "\r\n") + "\r\n\r\n" + body
}

// appClosed closes the application's end and waits until the upstream reads
// the close, forwarded: the application's close is the call's first end.
func appClosed(t *testing.T, c *relayedConn) {
	t.Helper()
	_ = c.app.Close()
	buf := make([]byte, 1)
	_ = c.dest.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.dest.Read(buf); err != io.EOF {
		t.Fatalf("the upstream read %v; want the application's close forwarded (EOF)", err)
	}
}

// seenAt reads at the application until its connection ends.
func seenAt(t *testing.T, app net.Conn) seen {
	t.Helper()
	got, err := appRead(t, app)
	return seen{got, endOf(err)}
}

// A reason phrase the upstream gave is served as it gave it, on a whole answer
// as on one cut short.
func TestReplayFault_ACutShortAnswerKeepsItsReasonPhrase(t *testing.T) {
	const head = "HTTP/1.1 402 Card Declined\r\nContent-Length: 20\r\n\r\n"
	c := newRelayedConn(t)
	c.start(t)
	write(t, c.app, c.src, faultReq)
	relayed(t, c.dest, faultReq)
	write(t, c.dest, c.dst, head+"0123456789")
	_ = c.dest.Close()
	atRecord := seenAt(t, c.app)
	mocks, err := c.finish(t)
	m := oneMock(t, mocks, err)
	app, returned := replaying(t, &HTTP{Logger: zap.NewNop()}, throughDisk(t, m))
	if atReplay := seenAt(t, app); atReplay != atRecord || !strings.HasPrefix(atReplay.got, "HTTP/1.1 402 Card Declined\r\n") {
		t.Errorf("at replay the application saw %+v; at record %+v", atReplay, atRecord)
	}
	_ = awaitReturn(t, returned)
}

// faultMock is a recorded GET /charge whose call ended in fault, with resp
// its answer's head and body as recorded.
func faultMock(fault *models.Fault, resp *models.HTTPResp) *models.Mock {
	if resp == nil {
		resp = &models.HTTPResp{}
	}
	return &models.Mock{
		Name: "mock-12",
		Kind: models.HTTP,
		Spec: models.MockSpec{
			HTTPReq: &models.HTTPReq{Method: "GET", URL: "http://pay.example/charge", ProtoMajor: 1, ProtoMinor: 1,
				Header: map[string]string{"Host": "pay.example"}},
			HTTPResp: resp,
			Fault:    fault,
		},
	}
}

func okHead(body string, header map[string]string) *models.HTTPResp {
	return &models.HTTPResp{StatusCode: 200, Header: header, Body: body}
}

// A delay holds the whole answer for its duration, then serves it, and the
// connection stays for the next call; the run's end stops the hold and serves
// nothing. That is so for a matched mock and for an async lane's recorded
// delivery: the two places decodeHTTP enacts a mock's fault.
//
// The first half is timed from the answer-fault hook's ask, which replay
// makes just before it holds the answer (the hook's nil leaves the mock its
// own delay). The hold starts after the ask, and a timer fires no sooner than
// its duration, so the answer comes no sooner than the delay after it, with no
// margin. A stamp read once the call was handed to decodeHTTP could fall after
// the hold had started, on a starved scheduler, and read the hold short. The
// second half is not timed: it ends the run once the hook was asked, so the
// run ends as the hold starts or during it, never before the answer is chosen.
func TestReplayFault_ADelayHoldsTheWholeAnswerBoundedByTheRun(t *testing.T) {
	const delay = 50 * time.Millisecond
	for _, tc := range []struct {
		name string
		// serves sets h to serve m at the row's place, and returns the mocks
		// decodeHTTP's matcher is given.
		serves func(h *HTTP, m *models.Mock) []*models.Mock
	}{
		{"a matched mock", func(_ *HTTP, m *models.Mock) []*models.Mock { return []*models.Mock{m} }},
		{"an async lane's delivery", func(h *HTTP, m *models.Mock) []*models.Mock {
			asyncDelivery(h, m)
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// holding is replay set to serve an answer held for d, with an
			// answer-fault hook that stamps its ask and returns nil.
			holding := func(d time.Duration) (*HTTP, []*models.Mock, <-chan time.Time) {
				asked := make(chan time.Time, 1)
				h := &HTTP{Logger: zap.NewNop()}
				h.SetAnswerFault(func(context.Context, *models.Mock) *models.Fault {
					select {
					case asked <- time.Now():
					default:
					}
					return nil
				})
				m := faultMock(&models.Fault{Kind: models.FaultDelay, Delay: d}, okHead(`{"ok":true}`, map[string]string{"Content-Length": "11"}))
				return h, tc.serves(h, m), asked
			}
			askedAt := func(asked <-chan time.Time) time.Time {
				t.Helper()
				select {
				case at := <-asked:
					return at
				case <-time.After(10 * time.Second): // turns a hang into a failure
					t.Fatal("replay did not ask the answer-fault hook for the delayed call within 10s")
					return time.Time{}
				}
			}

			h, mocks, asked := holding(delay)
			app, returned := replaying(t, h, mocks...)
			got := readUntil(app, `{"ok":true}`, 10*time.Second)
			if !strings.HasPrefix(got, "HTTP/1.1 200 OK\r\n") || !strings.HasSuffix(got, `{"ok":true}`) {
				t.Fatalf("the delayed call was answered %q, want its whole answer", got)
			}
			if held := time.Since(askedAt(asked)); held < delay {
				t.Errorf("the answer came after %s, before its delay of %s", held, delay)
			}
			select {
			case err := <-returned:
				t.Fatalf("decodeHTTP returned %v after a delayed answer; want the connection kept for the next call", err)
			default:
			}

			h, mocks, asked = holding(time.Hour)
			app, proxy := loopback(t)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			done := make(chan error, 1)
			go func() {
				done <- h.decodeHTTP(ctx, []byte(faultReq), proxy, nil,
					&mockMemDb{mocks: mocks, updateUnFilteredReturn: true}, models.OutgoingOptions{})
				_ = proxy.Close()
			}()
			askedAt(asked)
			cancel()
			if err := awaitReturn(t, done); err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("decodeHTTP = %v at the run's end during a delay", err)
			}
			if s := seenAt(t, app); s.got != "" {
				t.Errorf("the run ended during a delay and the application read %q; want nothing", s.got)
			}
		})
	}
}

// The run's end ends a timeout's hold: the connection is never held past it.
func TestReplayFault_ATimeoutIsHeldNoLongerThanTheRun(t *testing.T) {
	m := faultMock(&models.Fault{Kind: models.FaultTimeout}, nil)
	app, proxy := loopback(t)
	t.Cleanup(func() { _ = app.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (&HTTP{Logger: zap.NewNop()}).decodeHTTP(ctx, []byte(faultReq), proxy, nil,
			&mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}, models.OutgoingOptions{})
		_ = proxy.Close()
	}()
	if s := appGivesUp(t, app); s != (seen{"", "its own timeout"}) {
		t.Fatalf("the application saw %+v; want nothing until its own timeout", s)
	}
	cancel()
	_ = awaitReturn(t, done)
}

// The answer-fault hook gives a mock with no fault of its own the hook's
// fault, enacted as a recorded one is; its nil leaves a mock its own fault,
// and a fault it gives that this keploy cannot enact is served as nothing,
// the reason named.
func TestReplayFault_TheAnswerFaultHook(t *testing.T) {
	whole := faultMock(nil, okHead(`{"ok":true}`, map[string]string{"Content-Length": "11"}))
	for _, tc := range []struct {
		name    string
		mock    *models.Mock
		hook    AnswerFault
		want    seen
		wantErr error
	}{
		{"the hook's fault", whole, func(context.Context, *models.Mock) *models.Fault {
			return &models.Fault{Kind: models.FaultEOF}
		}, seen{"", "eof"}, nil},
		{"the hook's nil keeps the mock's own", faultMock(&models.Fault{Kind: models.FaultReset}, nil),
			func(context.Context, *models.Mock) *models.Fault { return nil }, seen{"", "reset"}, nil},
		{"no hook: the whole answer", whole, nil,
			seen{"HTTP/1.1 200 OK\r\nContent-Length: 11\r\n\r\n{\"ok\":true}", "eof"}, nil},
		{"a fault the hook gives that cannot be enacted", whole, func(context.Context, *models.Mock) *models.Fault {
			return &models.Fault{Kind: "explode"}
		}, seen{"", "eof"}, models.ErrFaultUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &HTTP{Logger: zap.NewNop()}
			h.SetAnswerFault(func(context.Context, *models.Mock) *models.Fault { return &models.Fault{Kind: "set before"} })
			h.SetAnswerFault(tc.hook)
			app, returned := replaying(t, h, tc.mock)
			if tc.want.got != "" {
				got := readUntil(app, `{"ok":true}`, 10*time.Second)
				_ = app.Close()
				if got != tc.want.got {
					t.Errorf("the application read %q, want %q", got, tc.want.got)
				}
				_ = awaitReturn(t, returned)
				return
			}
			if s := seenAt(t, app); s != tc.want {
				t.Errorf("the application saw %+v, want %+v", s, tc.want)
			}
			if err := awaitReturn(t, returned); !errors.Is(err, tc.wantErr) || (tc.wantErr != nil && !strings.Contains(err.Error(), "mock-12")) {
				t.Errorf("decodeHTTP = %v, want %v naming the mock", err, tc.wantErr)
			}
		})
	}
}

// A fault after the head whose mock cannot give the head it needs is refused,
// the reason named, and nothing is sent: never the head as a whole answer.
func TestReplayFault_AHeadTheMockCannotGiveIsRefused(t *testing.T) {
	cl := map[string]string{"Content-Length": "20"}
	for _, tc := range []struct {
		name  string
		fault models.Fault
		resp  *models.HTTPResp
		want  string // in the error; "" for a head served
	}{
		{"no recorded head", models.Fault{Kind: models.FaultReset, AfterHead: true}, &models.HTTPResp{}, "no recorded head"},
		{"no response at all", models.Fault{Kind: models.FaultTimeout, AfterHead: true}, nil, "no recorded head"},
		{"fewer body bytes than the fault sends", models.Fault{Kind: models.FaultTruncate, BodyBytes: 11}, okHead("0123456789", cl), "body holds 10"},
		{"all the body bytes the fault sends", models.Fault{Kind: models.FaultTruncate, BodyBytes: 10}, okHead("0123456789", cl), ""},
		{"a truncate the head does not frame", models.Fault{Kind: models.FaultTruncate, BodyBytes: 5}, okHead("01234", nil), "no Content-Length or Transfer-Encoding"},
		{"a chunked truncate", models.Fault{Kind: models.FaultTruncate, BodyBytes: 5}, okHead("5\r\n01", map[string]string{"Transfer-Encoding": "chunked"}), ""},
		{"a reset after a head that does not frame its body", models.Fault{Kind: models.FaultReset, AfterHead: true, BodyBytes: 5}, okHead("01234", nil), ""},
		{"a body kept as a stream's frames", models.Fault{Kind: models.FaultReset, AfterHead: true, BodyBytes: 16}, framedSSE("data: a\n\ndata: b"), "kept as a stream's frames"},
		{"a body kept as one frame, a byte of it sent", models.Fault{Kind: models.FaultTruncate, BodyBytes: 1}, framedSSE("data: a"), "kept as a stream's frames"},
		{"a body kept as a stream's frames, none of it sent", models.Fault{Kind: models.FaultReset, AfterHead: true}, framedSSE("data: a\n\ndata: b"), ""},
	} {
		eitherSwitch(t, tc.name, func(t *testing.T) {
			f := tc.fault
			m := faultMock(&f, tc.resp)
			if tc.resp == nil {
				m.Spec.HTTPResp = nil // as a gob mock may carry it
			}
			head, err := (&HTTP{Logger: zap.NewNop()}).faultHead(m, &f)
			if tc.want == "" {
				if err != nil || !strings.HasPrefix(string(head), "HTTP/1.1 200 OK\r\n") || !strings.HasSuffix(string(head), "\r\n\r\n"+tc.resp.Body[:f.BodyBytes]) {
					t.Errorf("faultHead = %q, %v; want the head and %d body bytes", head, err, f.BodyBytes)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "mock-12") {
				t.Errorf("faultHead = %q, %v; want an error naming the mock and %q", head, err, tc.want)
			}
		})
	}
}

// framedSSE is an SSE answer whose body, wire as the upstream sent it, is
// kept as its frames, as an earlier keploy wrote a fault's body, and read
// back from them: "data: a\n\ndata: b", cut inside its second event, reads
// back as "data:a\n\ndata:b\n\n", 16 bytes, both events ended.
func framedSSE(wire string) *models.HTTPResp {
	header := map[string]string{"Content-Type": "text/event-stream", "Content-Length": "40"}
	frames := models.StreamChunksOf(header, wire)
	var back models.HTTPResp
	out, err := yamlLib.Marshal(models.HTTPResp{StatusCode: 200, Header: header, StreamBody: frames})
	if err == nil {
		err = yamlLib.Unmarshal(out, &back)
	}
	if err != nil || len(back.StreamBody) != len(frames) || len(frames) == 0 {
		panic(fmt.Sprintf("framedSSE(%q): %v, %d frames read back of %d\n%s", wire, err, len(back.StreamBody), len(frames), out))
	}
	return &back
}

// A fault mock an earlier keploy wrote with its body as a stream's frames is
// refused at replay, the reason named, and nothing is sent: the frames do not
// keep the bytes the upstream sent before its fault, and the bytes they read
// back as are another answer (both events of a stream cut inside its second,
// whose client dispatched the first alone). Written again, the mock keeps
// its frames, so the refusal stays.
func TestReplayFault_ABodyKeptAsFramesIsRefused(t *testing.T) {
	eitherSwitch(t, "a reset inside the second event", func(t *testing.T) {
		m := throughDisk(t, faultMock(&models.Fault{Kind: models.FaultReset, AfterHead: true, BodyBytes: 16}, framedSSE("data: a\n\ndata: b")))
		if len(m.Spec.HTTPResp.StreamBody) == 0 {
			t.Fatalf("the mock's frames were written as bytes: body %q", m.Spec.HTTPResp.Body)
		}
		app, returned := replaying(t, &HTTP{Logger: zap.NewNop()}, m)
		if got := seenAt(t, app); got.got != "" {
			t.Errorf("the application read %q; want nothing sent for a fault the mock cannot give", got.got)
		}
		if err := awaitReturn(t, returned); err == nil || !strings.Contains(err.Error(), "kept as a stream's frames") {
			t.Errorf("decodeHTTP = %v; want the refusal naming the frames", err)
		}
	})
}

// A recorded call that ended in a fault is no representative answer for a
// pass-through egress rule's calls: it is passed over, never its head served
// whole to each of them.
func TestReplayFault_APassThroughCallIsNotServedAFault(t *testing.T) {
	m := faultMock(&models.Fault{Kind: models.FaultTruncate, BodyBytes: 2}, okHead("ok", map[string]string{"Content-Length": "20"}))
	h := &HTTP{Logger: zap.NewNop()}
	u, err := url.Parse("http://pay.example/charge")
	if err != nil {
		t.Fatal(err)
	}
	in := &req{method: "GET", url: u}
	if got := h.serveOnePassThroughMock(&mockMemDb{mocks: []*models.Mock{m}}, in, "pay.example", 0, nil); got != nil {
		t.Errorf("the pass-through call was served %q, a call that ended in a fault", got)
	}
	m.Spec.Fault = nil
	if got := h.serveOnePassThroughMock(&mockMemDb{mocks: []*models.Mock{m}}, in, "pay.example", 0, nil); got == nil {
		t.Error("the pass-through call was served nothing from the same call with a whole answer")
	}
}

// asyncDelivery sets h to serve m as an async lane's recorded delivery to
// faultReq: the lane's engine, not the mock matcher, decides what it is
// served.
func asyncDelivery(h *HTTP, m *models.Mock) {
	m.Spec.Async = &models.AsyncMeta{Lane: "charge", Seq: 1}
	engine := async.NewEngine(zap.NewNop(),
		[]models.AsyncLane{{Name: "charge", Type: "http", Match: map[string]string{"pathRegex": "^/charge$"}}},
		map[string]async.AsyncParser{"http": h})
	engine.Load([]*models.Mock{m})
	h.SetAsyncEngine(engine)
}

// An async lane's recorded delivery whose call ended in a fault gets the
// fault, as the matched call's does: never its head served as an answer.
func TestReplayFault_AnAsyncDeliveryGetsItsFault(t *testing.T) {
	h := &HTTP{Logger: zap.NewNop()}
	asyncDelivery(h, faultMock(&models.Fault{Kind: models.FaultEOF}, nil))
	app, returned := replaying(t, h)
	if s := seenAt(t, app); s != (seen{"", "eof"}) {
		t.Errorf("the application saw %+v of an async delivery that ended in a close; want nothing, then EOF", s)
	}
	if err := awaitReturn(t, returned); err != nil {
		t.Errorf("decodeHTTP = %v, want nil", err)
	}
}
