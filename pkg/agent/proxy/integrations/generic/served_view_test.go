package generic_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/generic"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// aloft holds a connection's requests in one request of the learn step, as
// the agent's accepted connection does.
type aloft struct{ f *learn.Flight }

func (aloft) Hold() func()              { return func() {} }
func (a aloft) InFlight() *learn.Flight { return a.f }

var servedBase = time.Date(2026, 10, 30, 11, 0, 0, 0, time.UTC)

// piece is one recorded piece of a generic exchange, as the recorder keeps it
// (encodePayload): ASCII text as it is, any other bytes base64 encoded.
func piece(origin models.OriginType, b string) models.Payload {
	typ, data := models.String, b
	for i := 0; i < len(b); i++ {
		if b[i] > 127 {
			typ, data = "binary", base64.StdEncoding.EncodeToString([]byte(b))
			break
		}
	}
	return models.Payload{Origin: origin, Message: []models.OutputBinary{{Type: typ, Data: data}}}
}

// exchange is a generic recording of one exchange: the request in reqs'
// pieces, answered with answer.
// A connection's first exchange is session, every later one per-test
// (perTest), as the recorder states them (exchangeLifetime).
func exchange(name string, at time.Time, perTest bool, answer string, reqs ...string) *models.Mock {
	m := &models.Mock{Version: "api.keploy.io/v1beta1", Name: name, Kind: models.GENERIC}
	lt, stated := models.LifetimeSession, models.MetadataLifetimeSession
	if perTest {
		lt, stated = models.LifetimePerTest, models.MetadataLifetimePerTest
	}
	m.TestModeInfo.Lifetime = lt
	m.Spec.Metadata = map[string]string{"type": "config", models.MetadataLifetimeKey: stated, "connID": "c1"}
	m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = at, at.Add(500*time.Microsecond)
	for _, r := range reqs {
		m.Spec.GenericRequests = append(m.Spec.GenericRequests, piece(models.FromClient, r))
	}
	m.Spec.GenericResponses = []models.Payload{piece(models.FromServer, answer)}
	return m
}

// servedOn sends live, in its pieces, on a connection the generic parser
// answers from a store that holds recs and tells tracker of each consume as
// the agent's does, and wants served's answer back. It returns the recording
// read with the view the serve stated (learn.Order.ServedView), in the next
// test of the set, as a later call reads it.
func servedOn(t *testing.T, tracker *learn.Tracker, recs []*models.Mock, served *models.Mock, live ...string) learn.Raw {
	t.Helper()
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetServedObserver(tracker.ServedMock)
	mm.SetServedInObserver(tracker.ServedMockIn)
	if served.TestModeInfo.Lifetime == models.LifetimePerTest {
		// A test's own exchange, served while that test is open.
		mm.SetMocksWithWindow(recs, nil, models.BaseTime, time.Now().Add(time.Hour))
		mm.ServeExactTest("T", recs)
	} else {
		mm.SetMocksWithWindow(nil, recs, models.BaseTime, time.Now().Add(time.Hour))
	}
	tracker.Begin()
	in := tracker.Lift()
	ctx, cancel := context.WithCancel(learn.WithTracker(context.Background(), tracker))
	ctx = context.WithValue(ctx, models.ClientConnectionIDKey, "c1")
	ctx = context.WithValue(ctx, models.DestConnectionIDKey, "d1")
	ctx = integrations.WithRequestHolder(ctx, aloft{in})
	app, f := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = generic.New(zap.NewNop()).MockOutgoing(ctx, f, nil, mm, models.OutgoingOptions{})
	}()
	t.Cleanup(func() {
		cancel()
		_ = app.Close()
		_ = f.Close()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("the generic parser did not return after the connection closed")
		}
	})
	for i, p := range live {
		if i > 0 {
			// A piece of its own: past the parser's 10 ms read deadline
			// would end the request, so the pieces go in one burst.
			time.Sleep(time.Millisecond)
		}
		if _, err := io.WriteString(app, p); err != nil {
			t.Fatalf("send %q: %v", p, err)
		}
	}
	answer := served.Spec.GenericResponses[0].Message[0].Data
	if served.Spec.GenericResponses[0].Message[0].Type != models.String {
		b, _ := base64.StdEncoding.DecodeString(answer)
		answer = string(b)
	}
	got := make([]byte, len(answer))
	_ = app.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(app, got); err != nil || string(got) != answer {
		t.Fatalf("the request was not answered with %s: %q, %v", served.Name, got, err)
	}
	_ = app.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("the generic parser did not return after the connection closed")
	}
	in.Land()
	tracker.End()
	tracker.Begin()
	order := tracker.Placed().Order
	// A connection's first exchange (session) is numbered with no place in
	// F's order (learn.Tracker.ServedMockIn): read as served all the same.
	first, last := order.Of(served)
	if (first == 0 || last == 0) && !served.IsProtocolPhase() {
		t.Fatalf("%s is at %d,%d: its serve was not numbered", served.Name, first, last)
	}
	return order.ServedView(served, learn.RawOf(served, true).ServedIn(first, last))
}

// learnAfter is the learn step's verdict on a later call's value at path,
// recorded as recorded and now value, read with r, the serve before it.
func learnAfter(t *testing.T, r learn.Raw, path, recorded, value string) learn.Learned {
	t.Helper()
	rec := learn.Shape{Leaves: map[string]learn.Leaf{path: {Path: path, Type: learn.String, Value: recorded}}}
	now := learn.Shape{Leaves: map[string]learn.Leaf{path: {Path: path, Type: learn.String, Value: value}}}
	got := learn.Classify(learn.NewDriftCache(), []string{path}, now, rec, time.Now(), nil, []learn.Raw{r}, nil)
	if len(got) != 1 {
		t.Fatalf("%d verdicts; want one", len(got))
	}
	return got[0]
}

const lookup = "LOOKUP table=accounts column=balance region=emea limit=1 id="

// Every serve the generic parser makes of a recording under the proven
// switch states what it gave the application (learn.Tracker.
// ServedMockAsRecorded where the application sent the recorded request byte
// for byte, else learn.Tracker.ServedMockLive with the request as the
// application sent it; F always answers with the recorded answer): a
// request sent as recorded, one sent in other pieces than recorded, one
// only like the recording (the similarity match), and one whose recorded
// piece was obfuscated. A later call's changed value is then learned where
// no word of it stands in what F gave, and refused, naming the recording by
// what F gave there, where it holds a word only the live request held.
// Before, each such serve was one no adapter stated, and the learn step
// refused every later changed value of the set.
func TestEveryGenericServeStatesTheViewItGave(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	const (
		unstated = "where its adapter did not say what it gave the application there"
		path     = "body.ref"
		recorded = "82"
		fresh    = "9f3e"
	)
	at := servedBase.Add(time.Millisecond)
	// bin is a binary request (the recorder keeps it base64 encoded): the
	// parser matches a request only like its recording by a similarity of
	// binary pieces alone, and a test's own at 0.9, which a request this
	// long and one id apart is.
	var fields strings.Builder
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&fields, "k%02d=v%02d ", i, 99-i)
	}
	bin := "\xff\x01" + fields.String() + lookup
	for _, tc := range []struct {
		name       string
		perTest    bool     // a connection's later exchange (else its first)
		rec        []string // the recorded request's pieces
		noisy      bool     // the recording's first piece is obfuscated
		live       []string // the request the application sends, in pieces
		value      string   // the later call's changed value
		asRecorded bool
		why        string // "" learns; else a part of the refusal
	}{
		{"a request sent as recorded, a fresh value", false, []string{lookup + "1\r\n"}, false, []string{lookup + "1\r\n"}, fresh, true, ""},
		{"a later exchange sent as recorded, a fresh value", true, []string{lookup + "1\r\n"}, false, []string{lookup + "1\r\n"}, fresh, true, ""},
		{"a request sent in other pieces than recorded, a fresh value", false, []string{lookup, "1\r\n"}, false, []string{lookup + "1\r\n"}, fresh, true, ""},
		{"a binary request like the recording, a fresh value", false, []string{bin + "1\r\n"}, false, []string{bin + "4242\r\n"}, fresh, false, ""},
		{"a later binary exchange like the recording, a fresh value", true, []string{bin + "1\r\n"}, false, []string{bin + "4242\r\n"}, fresh, false, ""},
		{"a request whose recorded piece is obfuscated, a fresh value", false, []string{lookup + "1\r\n"}, true, []string{lookup + "4242\r\n"}, fresh, false, ""},
		{"a request whose recorded piece is obfuscated, the word only it sent", false, []string{lookup + "1\r\n"}, true, []string{lookup + "4242\r\n"}, "4242", false, "as record's full round gave it"},
		{"a later exchange whose recorded piece is obfuscated, the word only it sent", true, []string{lookup + "1\r\n"}, true, []string{lookup + "4242\r\n"}, "4242", false, "as record's full round gave it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			served := exchange("mocks-lookup", at, tc.perTest, "+balance=17 currency=EUR\r\n", tc.rec...)
			if tc.noisy {
				served.Noise = []string{regexp.QuoteMeta(served.Spec.GenericRequests[0].Message[0].Data)}
			}
			r := servedOn(t, learn.NewTracker(), []*models.Mock{served}, served, tc.live...)
			got := learnAfter(t, r, path, recorded, tc.value)
			switch {
			case tc.why == "" && got.Kind == learn.KindUnaccounted:
				t.Fatalf("FALSE RED: %s refused (%q); want learned: no word of it stands in what F gave at %s", tc.value, got.Why, served.Name)
			case tc.why != "" && got.Kind != learn.KindUnaccounted:
				t.Fatalf("FALSE GREEN: %s %s (%q); want refused, naming %s", tc.value, got.Kind, got.Why, tc.why)
			case tc.why != "" && (!strings.Contains(got.Why, served.Name) || !strings.Contains(got.Why, tc.why) || strings.Contains(got.Why, unstated)):
				t.Fatalf("FALSE REASON: %s refused (%q); want it to name %s by what F gave there: %s", tc.value, got.Why, served.Name, tc.why)
			}
			if r.AsRecorded() != tc.asRecorded || r.Live() == tc.asRecorded {
				t.Fatalf("%s read as recorded %v, with a view %v; want as recorded %v", served.Name, r.AsRecorded(), r.Live(), tc.asRecorded)
			}
		})
	}
}

// With the proven switch off the generic parser states nothing, and serves
// as it did: the same answer, and the recording read as a serve no adapter
// stated (which no learn step reads with the switch off).
func TestAGenericServeStatesNothingWithTheSwitchOff(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "0")
	at := servedBase.Add(time.Millisecond)
	for _, live := range []string{lookup + "1\r\n", "\xff\x01" + lookup + "1\r\n"} {
		served := exchange("mocks-lookup", at, false, "+balance=17 currency=EUR\r\n", live)
		r := servedOn(t, learn.NewTracker(), []*models.Mock{served}, served, live)
		if r.AsRecorded() || r.Live() {
			t.Fatalf("with the switch off %s read as recorded %v, with a view %v; want neither: nothing stated", served.Name, r.AsRecorded(), r.Live())
		}
	}
}

// A serve the store refused to consume (another connection consumed the
// recording first) gave nothing: the parser states nothing of it, reads the
// store again, and states the recording it then serves.
func TestAGenericRecordingTheStoreDidNotConsumeStatesNothing(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	tracker := learn.NewTracker()
	served := exchange("mocks-lookup", servedBase.Add(time.Millisecond), false, "+balance=17 currency=EUR\r\n", lookup+"1\r\n")
	other := exchange("mocks-other", servedBase.Add(2*time.Millisecond), false, "+balance=18 currency=EUR\r\n", lookup+"1\r\n")
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetServedObserver(tracker.ServedMock)
	mm.SetServedInObserver(tracker.ServedMockIn)
	mm.SetMocksWithWindow(nil, []*models.Mock{served, other}, models.BaseTime, time.Now().Add(time.Hour))
	tracker.Begin()
	ctx := learn.WithTracker(context.Background(), tracker)
	ctx = context.WithValue(ctx, models.ClientConnectionIDKey, "c1")
	ctx = context.WithValue(ctx, models.DestConnectionIDKey, "d1")
	refusing := &refuseFirst{MockManager: mm}
	app, f := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = generic.New(zap.NewNop()).MockOutgoing(ctx, f, nil, refusing, models.OutgoingOptions{})
	}()
	t.Cleanup(func() {
		_ = app.Close()
		_ = f.Close()
		<-done
	})
	if _, err := io.WriteString(app, lookup+"1\r\n"); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len("+balance=18 currency=EUR\r\n"))
	_ = app.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(app, got); err != nil {
		t.Fatalf("no answer: %v", err)
	}
	_ = app.Close()
	<-done
	if refusing.refused == nil {
		t.Fatal("the store refused no consume")
	}
	tracker.End()
	tracker.Begin()
	order := tracker.Placed().Order
	refused, answered := refusing.refused, served
	if refused.Name == served.Name {
		answered = other
	}
	if string(got) != answered.Spec.GenericResponses[0].Message[0].Data {
		t.Fatalf("answered %q; want %s's answer", got, answered.Name)
	}
	if r := order.ServedView(refused, learn.RawOf(refused, true)); r.AsRecorded() || r.Live() {
		t.Fatalf("%s, which the store refused to consume, read as recorded %v, with a view %v; want nothing stated", refused.Name, r.AsRecorded(), r.Live())
	}
	if r := order.ServedView(answered, learn.RawOf(answered, true)); !r.AsRecorded() {
		t.Fatalf("%s, served as recorded, is not read so", answered.Name)
	}
}

// refuseFirst is a store that refuses the first recording a consume asks
// for, and shows it no more, as when another connection consumed it first.
type refuseFirst struct {
	*proxy.MockManager
	refused *models.Mock
}

func (r *refuseFirst) UpdateUnFilteredMock(old, new *models.Mock) bool {
	if r.refused == nil {
		r.refused = old
	}
	if old.Name == r.refused.Name {
		return false
	}
	return r.MockManager.UpdateUnFilteredMock(old, new)
}

// GetSessionMocks is the store's startup and session mocks (no test is
// open, so no exact view leads them), each once (a mock staged before the
// first test is in both), less the refused one.
func (r *refuseFirst) GetSessionMocks() ([]*models.Mock, error) {
	startup, err := r.GetStartupMocks()
	if err != nil {
		return nil, err
	}
	session, err := r.GetSessionScopedMocks()
	if err != nil {
		return nil, err
	}
	out := make([]*models.Mock, 0, len(startup)+len(session))
	seen := make(map[*models.Mock]bool, len(startup)+len(session))
	for _, m := range append(startup, session...) {
		if seen[m] || (r.refused != nil && m.Name == r.refused.Name) {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out, nil
}
