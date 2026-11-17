package proxy

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

func bgRealClock(real time.Time) (time.Time, bool) { return real, true }

// With the switch off a manager notes every serve to the serve clock it is
// handed, by either door (a mock taken by address or by value), but a
// protocol phase's (a DNS answer, a connection's handshake) and a nameless
// mock's: a later call's span starts at the latest serve noted before it.
func TestTheServeClockNotesEveryServeButAProtocolPhases(t *testing.T) {
	m := NewMockManager(nil, nil, zap.NewNop())
	m.observeServed(&models.Mock{Name: "before-the-clock", Kind: models.Mongo}, nil)
	c := &learn.ServeClock{}
	c.SetClock(bgRealClock)
	m.SetServeClock(c)
	if s, ok := c.SpanTo(time.Now()); ok {
		t.Fatalf("a serve made before the manager had the clock was noted: span from %v", s.From)
	}
	// The earliest serve the clock keeps, of neither group read below: a
	// group none of whose serves was noted by its name would start its span
	// here (learn.ServeClock.SpansOf).
	m.observeServed(&models.Mock{Name: "earliest", Kind: models.Mongo}, nil)

	before := time.Now()
	m.observeServed(&models.Mock{Name: "poll", Kind: models.Mongo}, nil)
	mid := time.Now()
	m.observeServed(&models.Mock{Name: "lookup", Kind: models.DNS}, nil)
	m.observeServedCopy(models.Mock{Name: "lookup-2", Kind: models.DNS}, nil)
	m.observeServed(&models.Mock{Kind: models.Mongo}, nil)
	m.observeServedCopy(models.Mock{Kind: models.Mongo}, nil)
	s, ok := c.SpanTo(time.Now())
	if !ok || s.From.Before(before) || s.From.After(mid) {
		t.Fatalf("span from %v %v; want the data-plane serve's, between %v and %v (no protocol phase's, no nameless mock's)", s.From, ok, before, mid)
	}

	after := time.Now()
	m.observeServedCopy(models.Mock{Name: "poll-2", Kind: models.Mongo}, nil)
	if s, ok := c.SpanTo(time.Now()); !ok || s.From.Before(after) {
		t.Fatalf("a serve by value was not noted: span from %v %v; want at or after %v", s.From, ok, after)
	}

	// Each serve is noted with its recording's name, by either door: a
	// background call of that recording's group starts its span at it
	// (learn.ServeClock.SpansOf), not at a later serve of another flow's,
	// nor at the earliest serve kept, as a group whose serve went unnamed
	// would. Mutation: note a serve by either door without its name.
	done := time.Now()
	m.observeServed(&models.Mock{Name: "other", Kind: models.Mongo}, nil)
	for _, g := range []struct {
		name     string
		from, to time.Time
	}{{"poll", before, mid}, {"poll-2", after, done}} {
		spans, ok := c.SpansOf(time.Now(), func(n string) bool { return n == g.name })
		if !ok || len(spans) != 1 || spans[0].From.Before(g.from) || spans[0].From.After(g.to) {
			t.Fatalf("spans = %+v,%v of %s's group; want one from its serve, between %v and %v", spans, ok, g.name, g.from, g.to)
		}
	}
}

// spanParser is a parser the test reaches through handleConnection: it
// reports whether its connection's ctx tells a call's span for the
// background check (learn.CallSpanOf), of a call arriving now.
type spanParser struct{ saw chan bool }

func (spanParser) MatchType(context.Context, []byte) bool { return true }
func (spanParser) RecordOutgoing(context.Context, *integrations.RecordSession) error {
	return nil
}
func (s spanParser) MockOutgoing(ctx context.Context, _ net.Conn, _ *models.ConditionalDstCfg, _ integrations.MockMemDb, _ models.OutgoingOptions) error {
	_, ok := learn.CallSpanOf(ctx, time.Now())
	s.saw <- ok
	return nil
}
func (spanParser) MockNoise() mocknoise.Adapter { return nil }

// spanThrough hands a kept connection to handleConnection on a proxy whose
// serve clock reads the real clock and noted a serve, and returns the proxy
// and whether the parser's ctx told a call's span.
func spanThrough(t *testing.T) (*Proxy, bool) {
	t.Helper()
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_TEST
	p := New(zap.NewNop(), fakeDestInfo{dest: ipv4Dest(t, "127.0.0.1:50051")}, cfg)
	p.SetBackgroundClock(bgRealClock)
	p.serveClock.Served()
	w := spanParser{saw: make(chan bool, 1)}
	p.Integrations = map[integrations.IntegrationType]integrations.Integrations{integrations.HTTP: w}
	p.integrationsPriority = []ParserPriority{{Priority: 1, ParserType: integrations.HTTP}}
	_ = p.Mock(context.Background(), models.OutgoingOptions{DisableMysqlAutoDetect: true, Mocking: true})
	app, src := acceptedConn(t)
	tc := p.inflight.track(src)
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), tc) }()
	if _, err := io.WriteString(app, "hello\n"); err != nil {
		t.Fatal(err)
	}
	ok := awaitEvent(t, w.saw, "the parser being handed the connection")
	if err := awaitEvent(t, done, "handleConnection returning"); err != nil {
		t.Fatalf("handleConnection: %v", err)
	}
	return p, ok
}

// With the proven switch off the session's manager notes its serves to the
// proxy's serve clock, and a parser reads a call's span for the background
// check on it, through its connection's ctx. Under the switch neither: the
// manager notes nothing to it, and the parser reads the learn tracker's span
// (none here: the tracker was handed no clock).
func TestWithTheSwitchOffAParserReadsACallsSpanOnTheServeClock(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	p, ok := spanThrough(t)
	if !ok {
		t.Fatal("the switch off: the parser's ctx tells no span on the serve clock")
	}
	if got := p.getMockManager().serveClock.Load(); got != p.serveClock {
		t.Fatalf("the switch off: the session's manager notes its serves to %p; want the proxy's serve clock %p", got, p.serveClock)
	}

	t.Setenv(provenswitch.Env, "1")
	p, ok = spanThrough(t)
	if ok {
		t.Fatal("under the switch the parser's ctx told a span on the serve clock; want the tracker's alone")
	}
	if got := p.getMockManager().serveClock.Load(); got != nil {
		t.Fatal("under the switch the session's manager notes its serves to the serve clock")
	}
}
