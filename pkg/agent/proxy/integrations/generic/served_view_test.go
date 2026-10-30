package generic_test

import (
	"context"
	"encoding/base64"
	"errors"
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
	"go.keploy.io/server/v3/pkg/provenswitch"
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

// servedOn sends live, in its pieces (callOn), and wants served's answer
// back. It returns the recording read with the view the serve stated
// (learn.Order.ServedView), in the next test of the set, as a later call
// reads it.
func servedOn(t *testing.T, tracker *learn.Tracker, recs []*models.Mock, served *models.Mock, live ...string) learn.Raw {
	t.Helper()
	app, done, in := callOn(t, tracker, recs, served, live...)
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

// missedOn sends live, in its pieces (callOn), and wants the request missed
// by exact serving: the parser answers nothing and returns the miss
// (models.ErrNoMockMatched, keploy/unmocked_call) with its report, whose
// diff says exact serving refused the closest recording (genericMissReport:
// "exact serving answers only an identical exchange", where a miss with the
// switch off names the similarity thresholds), and served is not served
// (its serve not numbered).
func missedOn(t *testing.T, tracker *learn.Tracker, recs []*models.Mock, served *models.Mock, live ...string) {
	t.Helper()
	_, done, in := callOn(t, tracker, recs, served, live...)
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("FALSE GREEN: the generic parser did not return a miss; want the request missed: no recording of the test sent it")
	}
	if !errors.Is(err, models.ErrNoMockMatched) {
		t.Fatalf("the generic parser returned %v; want a miss (%v)", err, models.ErrNoMockMatched)
	}
	var mr interface {
		MismatchReport() *models.MockMismatchReport
	}
	if !errors.As(err, &mr) || mr.MismatchReport() == nil || !strings.Contains(mr.MismatchReport().Diff, "exact serving answers only an identical exchange") {
		t.Fatalf("the generic parser missed with %v; want exact serving's miss, its report naming the closest recording refused by exact serving", err)
	}
	in.Land()
	tracker.End()
	tracker.Begin()
	if first, last := tracker.Placed().Order.Of(served); first != 0 || last != 0 {
		t.Fatalf("%s is at %d,%d after a miss; want it not served", served.Name, first, last)
	}
}

// callOn sends live, in its pieces, on a connection the generic parser
// answers from a store that holds recs and tells tracker of each consume as
// the agent's does: served's test open where served is a test's own, else no
// test open. Under the proven switch both are served exactly, as the agent
// serves them: a test it opens (ServeExactTest), and between tests the pool
// of the set's reusable recordings (ServeExactPool, service/agent
// exact_proven.go). A connection's first exchange is protocol-phase, which
// every view shows (treedb_shown.go showsStatic), so the pool's names decide
// nothing here: any view would serve it, as an open test's does. No row
// serves a connection's first exchange while a test is open; the compare is
// the same (findExactMatch). It returns the application's end of the
// connection, what the parser returns once it does, and the request the
// connection's requests are held in.
func callOn(t *testing.T, tracker *learn.Tracker, recs []*models.Mock, served *models.Mock, live ...string) (net.Conn, <-chan error, *learn.Flight) {
	t.Helper()
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetServedObserver(tracker.ServedMock)
	mm.SetServedInObserver(tracker.ServedMockIn)
	if served.TestModeInfo.Lifetime == models.LifetimePerTest {
		// A test's own exchange, served while that test is open.
		mm.SetMocksWithWindow(recs, nil, models.BaseTime, time.Now().Add(time.Hour))
		// The agent opens exact serving only under the proven switch
		// (service/agent exact_proven.go).
		if provenswitch.On() {
			mm.ServeExactTest("T", recs)
		}
	} else {
		// A connection's first exchange (session), served with no test open.
		mm.SetMocksWithWindow(nil, recs, models.BaseTime, time.Now().Add(time.Hour))
		// Under the proven switch the agent installs the pool's exact view
		// whenever no test is open (service/agent exact_proven.go).
		if provenswitch.On() {
			names := make([]string, 0, len(recs))
			for _, m := range recs {
				names = append(names, m.Name)
			}
			mm.ServeExactPool(models.PoolKeyNone, names)
		}
	}
	tracker.Begin()
	in := tracker.Lift()
	ctx, cancel := context.WithCancel(learn.WithTracker(context.Background(), tracker))
	ctx = context.WithValue(ctx, models.ClientConnectionIDKey, "c1")
	ctx = context.WithValue(ctx, models.DestConnectionIDKey, "d1")
	ctx = integrations.WithRequestHolder(ctx, aloft{in})
	app, f := net.Pipe()
	done := make(chan error, 1)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		done <- generic.New(zap.NewNop()).MockOutgoing(ctx, f, nil, mm, models.OutgoingOptions{})
	}()
	t.Cleanup(func() {
		cancel()
		_ = app.Close()
		_ = f.Close()
		select {
		case <-returned:
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
	return app, done, in
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

// binLookup is a binary request less its id (the recorder keeps it base64
// encoded). Without exact serving the parser serves a request only like its
// recording by the similarity of its binary pieces alone (findBinaryMatch):
// over 0.4 for a recording not marked filtered, as this harness stages a
// connection's first exchange; with another id this one is served so
// (TestAGenericServeStatesNothingWithTheSwitchOff). The 0.9 bar of a
// recording marked filtered is not reached here: in this harness only the
// exact view marks one so (its own, exactView.add), and exact serving runs
// no similarity.
func binLookup() string {
	var fields strings.Builder
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&fields, "k%02d=v%02d ", i, 99-i)
	}
	return "\xff\x01" + fields.String() + lookup
}

// Every serve the generic parser makes of a recording under the proven
// switch states what it gave the application (learn.Tracker.
// ServedMockAsRecorded where the application sent the recorded request byte
// for byte, else learn.Tracker.ServedMockLive with the request as the
// application sent it; F always answers with the recorded answer). The
// switch serves every exchange exactly: a test's own (a connection's later
// exchange) while its test is open (ServeExactTest), a connection's first
// (session) from the pool's view with no test open (ServeExactPool), both
// as the agent's exact_proven.go serves them. Exact serving answers only
// the recorded bytes, in any pieces (findExactMatch), and the serve states
// them as recorded by the same compare (sentAsRecorded): a request sent as
// recorded, and one sent in other pieces than recorded, of either. A later
// call's changed value is then learned where no word of it stands in what
// F gave. Before, each such serve was one no adapter stated, and the learn
// step refused every later changed value of the set.
//
// So under the switch no generic serve is stated live: a request only like
// its recording, or other than an obfuscated recorded piece (a noisy buffer
// is a difference there, and compared byte for byte), is no serve, of a
// connection's first exchange or of a later one: it misses
// (keploy/unmocked_call) and states nothing. #5002 added rows expecting
// both served, the first exchange's as if no test open meant no exact
// serving: that was a changed request passed with another request's
// answer. The view such a serve would state without an exact view
// (servedView) is TestAGenericServesViewIsTheRequestAsSentAndTheRecordedAnswer's.
func TestEveryGenericServeStatesTheViewItGave(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	const (
		unstated = "where its adapter did not say what it gave the application there"
		path     = "body.ref"
		recorded = "82"
		fresh    = "9f3e"
	)
	at := servedBase.Add(time.Millisecond)
	bin := binLookup()
	for _, tc := range []struct {
		name       string
		perTest    bool     // a connection's later exchange (else its first)
		rec        []string // the recorded request's pieces
		noisy      bool     // the recording's first piece is obfuscated
		live       []string // the request the application sends, in pieces
		miss       bool     // the request misses: no serve, nothing stated
		value      string   // the later call's changed value
		asRecorded bool
		why        string // "" learns; else a part of the refusal
	}{
		{"a request sent as recorded, a fresh value", false, []string{lookup + "1\r\n"}, false, []string{lookup + "1\r\n"}, false, fresh, true, ""},
		{"a later exchange sent as recorded, a fresh value", true, []string{lookup + "1\r\n"}, false, []string{lookup + "1\r\n"}, false, fresh, true, ""},
		{"a request sent in other pieces than recorded, a fresh value", false, []string{lookup, "1\r\n"}, false, []string{lookup + "1\r\n"}, false, fresh, true, ""},
		{"a later exchange sent in other pieces than recorded, a fresh value", true, []string{lookup, "1\r\n"}, false, []string{lookup + "1\r\n"}, false, fresh, true, ""},
		{"a binary request like the recording", false, []string{bin + "1\r\n"}, false, []string{bin + "4242\r\n"}, true, "", false, ""},
		{"a request other than its obfuscated recorded piece", false, []string{lookup + "1\r\n"}, true, []string{lookup + "4242\r\n"}, true, "", false, ""},
		{"a later binary exchange like the recording", true, []string{bin + "1\r\n"}, false, []string{bin + "4242\r\n"}, true, "", false, ""},
		{"a later exchange other than its obfuscated recorded piece", true, []string{lookup + "1\r\n"}, true, []string{lookup + "4242\r\n"}, true, "", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			served := exchange("mocks-lookup", at, tc.perTest, "+balance=17 currency=EUR\r\n", tc.rec...)
			if tc.noisy {
				served.Noise = []string{regexp.QuoteMeta(served.Spec.GenericRequests[0].Message[0].Data)}
			}
			if tc.miss {
				missedOn(t, learn.NewTracker(), []*models.Mock{served}, served, tc.live...)
				return
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
// as it did: the same answer, a connection's first exchange only like its
// recording (the similarity match) and one other than its obfuscated
// recorded piece included (TestEveryGenericServeStatesTheViewItGave's
// first-exchange misses, which only exact serving refuses), and the
// recording read as a serve no adapter stated (which no learn step reads
// with the switch off). With the switch off serving keeps every generic
// exchange session (DeriveLifetime, models/protocol_phase.go), so these
// rows are the switch-off control for the later-exchange misses too: the
// same requests. This harness does not serve a test's own exchange without
// an exact view.
func TestAGenericServeStatesNothingWithTheSwitchOff(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "0")
	at := servedBase.Add(time.Millisecond)
	bin := binLookup()
	for _, tc := range []struct {
		rec, live string
		noisy     bool // the recording's piece is obfuscated
	}{
		{lookup + "1\r\n", lookup + "1\r\n", false},
		{"\xff\x01" + lookup + "1\r\n", "\xff\x01" + lookup + "1\r\n", false},
		{bin + "1\r\n", bin + "4242\r\n", false},
		{lookup + "1\r\n", lookup + "4242\r\n", true},
	} {
		served := exchange("mocks-lookup", at, false, "+balance=17 currency=EUR\r\n", tc.rec)
		if tc.noisy {
			served.Noise = []string{regexp.QuoteMeta(served.Spec.GenericRequests[0].Message[0].Data)}
		}
		r := servedOn(t, learn.NewTracker(), []*models.Mock{served}, served, tc.live)
		if r.AsRecorded() || r.Live() {
			t.Fatalf("with the switch off %s read as recorded %v, with a view %v; want neither: nothing stated", served.Name, r.AsRecorded(), r.Live())
		}
	}
}

// A serve the store refused to consume (another connection consumed the
// recording first) gave nothing: the parser states nothing of it, reads the
// store again, and states the recording it then serves. The other
// connection's serve of the refused recording is numbered (the store's own
// consume) and unstated, so a statement of the refused serve would land on
// it. Each is served from the exact view the agent installs under the
// switch, with the connection's request held as the agent holds it, as
// callOn serves one: a connection's first exchange with no test open
// (ServeExactPool), and a test's own while its test is open
// (ServeExactTest), which the parser reads among the filtered recordings
// (the view marks its own so).
func TestAGenericRecordingTheStoreDidNotConsumeStatesNothing(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	for _, perTest := range []bool{false, true} {
		t.Run(fmt.Sprintf("per-test %v", perTest), func(t *testing.T) {
			refusedConsumeStatesNothing(t, perTest)
		})
	}
}

func refusedConsumeStatesNothing(t *testing.T, perTest bool) {
	tracker := learn.NewTracker()
	served := exchange("mocks-lookup", servedBase.Add(time.Millisecond), perTest, "+balance=17 currency=EUR\r\n", lookup+"1\r\n")
	other := exchange("mocks-other", servedBase.Add(2*time.Millisecond), perTest, "+balance=18 currency=EUR\r\n", lookup+"1\r\n")
	recs := []*models.Mock{served, other}
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetServedObserver(tracker.ServedMock)
	mm.SetServedInObserver(tracker.ServedMockIn)
	if perTest {
		mm.SetMocksWithWindow(recs, nil, models.BaseTime, time.Now().Add(time.Hour))
		mm.ServeExactTest("T", recs)
	} else {
		mm.SetMocksWithWindow(nil, recs, models.BaseTime, time.Now().Add(time.Hour))
		mm.ServeExactPool(models.PoolKeyNone, []string{served.Name, other.Name})
	}
	if !mm.ExactServing() {
		t.Fatal("no exact view installed under the switch")
	}
	tracker.Begin()
	ctx := learn.WithTracker(context.Background(), tracker)
	ctx = context.WithValue(ctx, models.ClientConnectionIDKey, "c1")
	ctx = context.WithValue(ctx, models.DestConnectionIDKey, "d1")
	in := tracker.Lift()
	ctx = integrations.WithRequestHolder(ctx, aloft{in})
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
	in.Land()
	if refusing.refused == nil {
		t.Fatal("the store refused no consume")
	}
	if !refusing.heldExact {
		t.Fatal("no consume was asked of a store held to its call's round under exact serving, as the agent's parser asks one")
	}
	for i, filtered := range refusing.filtered {
		if filtered != perTest {
			t.Fatalf("consume %d asked for a recording marked filtered %v; want %v (a test's own is read among the filtered, a first exchange among the unfiltered)", i, filtered, perTest)
		}
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

// refuseFirst is a store whose first consume another connection makes
// first: that consume is the store's own (numbered, stated by no parser) and
// the parser's is refused, and the recording is shown no more. It refuses in
// the store the manager holds a call to under exact serving (HoldToRound),
// which the parser reads and consumes through once it marked the call's
// round, and in what that store serves a held request with (ServingFor).
type refuseFirst struct {
	*proxy.MockManager
	refused *models.Mock
	// heldExact is whether a consume was asked of a held store that serves
	// exactly; filtered is whether each consume asked for a recording
	// marked filtered.
	heldExact bool
	filtered  []bool
}

// HoldToRound is the store the manager holds a call to (MockManager.
// HoldToRound), refusing as r does.
func (r *refuseFirst) HoldToRound(call integrations.CallRound) (integrations.MockMemDb, func() bool) {
	held, restaged := r.MockManager.HoldToRound(call)
	return &refusingIn{MockMemDb: held, r: r}, restaged
}

// refusingIn is a store a call is held to, or what it serves a held request
// with, refusing as r does. It serves exactly where the store it wraps does
// (integrations.ExactServing), and serves a held request as that store does
// (ServingFor); a serve reads nothing else of it but its MockMemDb methods.
type refusingIn struct {
	integrations.MockMemDb
	r *refuseFirst
}

func (h *refusingIn) ExactServing() bool { return integrations.ExactServing(h.MockMemDb) }

func (h *refusingIn) ServingFor(f *learn.Flight) integrations.MockMemDb {
	if s, ok := h.MockMemDb.(integrations.ServeTeller); ok {
		return &refusingIn{MockMemDb: s.ServingFor(f), r: h.r}
	}
	return h
}

// UpdateUnFilteredMock refuses the first consume asked, once another
// connection made it: the store's own consume of old, which the store's
// observer numbers as a serve of old and no parser states.
func (h *refusingIn) UpdateUnFilteredMock(old, new *models.Mock) bool {
	h.r.heldExact = h.r.heldExact || h.ExactServing()
	h.r.filtered = append(h.r.filtered, old.TestModeInfo.IsFiltered)
	if h.r.refused == nil {
		h.r.refused = old
		if !h.MockMemDb.UpdateUnFilteredMock(old, new) {
			panic("the other connection's consume of " + old.Name + " was refused")
		}
		return false
	}
	return old.Name != h.r.refused.Name && h.MockMemDb.UpdateUnFilteredMock(old, new)
}

// GetSessionMocks is what the store shows the parser, less the refused
// recording (a reusable one stays shown after its consume).
func (h *refusingIn) GetSessionMocks() ([]*models.Mock, error) {
	//nolint:staticcheck // SA1019: the generic parser reads its pool through GetSessionMocks, so the wrapper forwards the store's own.
	mocks, err := h.MockMemDb.GetSessionMocks()
	if err != nil || h.r.refused == nil {
		return mocks, err
	}
	out := make([]*models.Mock, 0, len(mocks))
	for _, m := range mocks {
		if m.Name != h.r.refused.Name {
			out = append(out, m)
		}
	}
	return out, nil
}
