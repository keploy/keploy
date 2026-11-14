package async

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// kindParser takes the calls of one protocol (mock kind) only, as a real
// parser's MatchesLane does, and answers a keep-alive of its own.
type kindParser struct {
	kind  models.Kind
	empty string
}

func (k *kindParser) MatchesLane(m *models.Mock, _ models.AsyncLane) bool {
	return m != nil && m.Kind == k.kind
}
func (k *kindParser) MatchRequestShape(_, _ *models.Mock, _ models.AsyncLane) (bool, string) {
	return true, ""
}
func (k *kindParser) EmptyResponse(models.AsyncLane) ([]byte, error) { return []byte(k.empty), nil }
func (k *kindParser) ResponseValueKey(*models.Mock, models.AsyncLane) string {
	return string(k.kind)
}

// laneTypedParser is a kindParser that serves lanes of the types it names.
type laneTypedParser struct {
	kindParser
	types []string
}

func (l *laneTypedParser) LaneTypes() []string { return l.types }

// An "http" lane names an endpoint, which an app reaches over HTTP/1.1 or
// HTTP/2 as ALPN decides. A call to it over HTTP/2 routes to the lane through
// the HTTP/2 parser, which serves "http" lanes (LaneTyper), and is answered
// with that parser's keep-alive; a call over HTTP/1.1 stays the HTTP/1
// parser's. A parser that declares no "http" lanes takes none of their calls.
func TestAnHTTPLaneTakesAnHTTP2CallThroughTheParserThatServesIt(t *testing.T) {
	h1 := &kindParser{kind: models.HTTP, empty: "HTTP/1 keep-alive"}
	h2 := &laneTypedParser{kindParser: kindParser{kind: models.HTTP2, empty: "HTTP/2 keep-alive"}, types: []string{"http"}}
	lane := models.AsyncLane{Name: "watch", Type: "http"}
	e := NewEngine(zap.NewNop(), []models.AsyncLane{lane}, map[string]AsyncParser{"http": h1, "http2": h2})

	h2Call := &models.Mock{Kind: models.HTTP2}
	if got, ok := e.LaneFor(h2Call); !ok || got.Name != "watch" {
		t.Fatal("an HTTP/2 call to the lane's endpoint routed to no lane")
	}
	rec, ka, err := e.Decide(context.Background(), lane, h2Call)
	if err != nil || rec != nil || string(ka) != "HTTP/2 keep-alive" {
		t.Fatalf("the HTTP/2 call was answered %q (recorded %v, err %v); want the HTTP/2 parser's keep-alive", ka, rec, err)
	}
	if _, ka, _ := e.Decide(context.Background(), lane, &models.Mock{Kind: models.HTTP}); string(ka) != "HTTP/1 keep-alive" {
		t.Fatalf("the HTTP/1 call was answered %q; want the HTTP/1 parser's keep-alive", ka)
	}

	undeclared := NewEngine(zap.NewNop(), []models.AsyncLane{lane}, map[string]AsyncParser{"http": h1, "http2": &h2.kindParser})
	if _, ok := undeclared.LaneFor(h2Call); ok {
		t.Fatal("a parser that declares no http lanes took an http lane's call")
	}
}

// A poll lane with nothing recorded for it (a watch whose long-poll was still
// open when recording stopped) is answered with the keep-alive no sooner than
// its throttle, as a poll with a recorded value is: answered at once, a client
// that re-polls as soon as it is answered polls without pause, thousands of
// times a second, for the whole replay. A non-poll lane's keep-alive is not
// held.
func TestAPollLaneWithNothingRecordedHoldsItsKeepAlive(t *testing.T) {
	e := newTestEngine(&fakeParser{matches: true, shapeOK: true, empty: []byte("KA")})

	start := time.Now()
	rec, ka, err := e.Decide(context.Background(), models.AsyncLane{Name: "L", Type: "fakePoll", ThrottleMs: 80}, &models.Mock{})
	if err != nil || rec != nil || string(ka) != "KA" {
		t.Fatalf("want the keep-alive, got %q (recorded %v, err %v)", ka, rec, err)
	}
	if took := time.Since(start); took < 60*time.Millisecond {
		t.Fatalf("a poll with nothing recorded was answered in %v; want it held by its throttle (~80ms)", took)
	}

	start = time.Now()
	if _, ka, _ := e.Decide(context.Background(), models.AsyncLane{Name: "L", Type: "fake", ThrottleMs: 5000}, &models.Mock{}); string(ka) != "KA" {
		t.Fatalf("want the keep-alive, got %q", ka)
	}
	if took := time.Since(start); took >= 500*time.Millisecond {
		t.Fatalf("a non-poll lane's keep-alive was held %v (ThrottleMs=5000)", took)
	}
}
