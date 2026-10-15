package generic

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// holds counts the requests a parser holds (integrations.RequestHolder), and
// says when the count changes.
type holds struct {
	mu      sync.Mutex
	n, max  int
	changed chan struct{}
}

func newHolds() *holds { return &holds{changed: make(chan struct{}, 64)} }

func (h *holds) Hold() func() {
	h.mu.Lock()
	h.n++
	h.max = max(h.max, h.n)
	h.mu.Unlock()
	h.changed <- struct{}{}
	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			h.n--
			h.mu.Unlock()
			h.changed <- struct{}{}
		})
	}
}

func (h *holds) held() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

// pingStore answers PING with PONG, from one recorded exchange; with no
// exchange it answers nothing.
type pingStore struct {
	integrations.MockMemDb
	mocks []*models.Mock
}

func (s pingStore) GetSessionMocks() ([]*models.Mock, error) { return s.mocks, nil }

func (s pingStore) UpdateUnFilteredMock(_, _ *models.Mock) bool { return true }

func pingPong() *models.Mock {
	m := exchange("mock-1", "PING")
	m.TestModeInfo.IsFiltered = true
	m.Spec.GenericResponses = []models.Payload{{Message: []models.OutputBinary{{Type: models.String, Data: "PONG"}}}}
	return m
}

// The generic parser reads on, with a deadline, before it matches the
// requests it gathered: blocked reading while it holds them. It says it
// holds them (integrations.HoldRequest) from the read until it has written
// the answer, so a test's end waits for the answer instead of judging the
// test without the call (keploy #4806: a call that slipped into the next
// test, or past the last one). Without the hold, the end saw a connection
// blocked reading an empty socket, and called it settled.
func TestTheGenericParserHoldsWhatItReadUntilItAnswers(t *testing.T) {
	h := newHolds()
	ctx, cancel := context.WithCancel(integrations.WithRequestHolder(context.Background(), h))
	defer cancel()
	app, proxySide := net.Pipe()
	defer app.Close()
	done := make(chan error, 1)
	go func() {
		done <- decodeGeneric(ctx, zap.NewNop(), []byte("PING"), proxySide, nil, pingStore{mocks: []*models.Mock{pingPong()}}, models.OutgoingOptions{})
	}()
	select {
	case <-h.changed:
	case <-time.After(10 * time.Second):
		t.Fatal("the parser read a request and never said it holds it")
	}
	if n := h.held(); n != 1 {
		t.Fatalf("held %d while the request waits for its answer, want 1", n)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(app, got); err != nil || string(got) != "PONG" {
		t.Fatalf("answer %q, %v", got, err)
	}
	for h.held() != 0 {
		select {
		case <-h.changed:
		case <-time.After(10 * time.Second):
			t.Fatal("the parser answered and still holds the request")
		}
	}
	// The next request is held again.
	if _, err := io.WriteString(app, "PING"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(app, got); err != nil || string(got) != "PONG" {
		t.Fatalf("second answer %q, %v", got, err)
	}
	for h.held() != 0 {
		<-h.changed
	}
	h.mu.Lock()
	most := h.max
	h.mu.Unlock()
	if most != 1 {
		t.Fatalf("held at most %d at once, want 1", most)
	}
	cancel()
	_ = app.Close()
	<-done
}

// A request no recorded exchange answers is a miss the parser returns: the
// proxy files it before the connection is let go, so the parser keeps it held
// until then (letting the connection go drops the mark).
func TestTheGenericParserKeepsAMissHeldForTheProxyToFile(t *testing.T) {
	h := newHolds()
	app, proxySide := net.Pipe()
	defer app.Close()
	err := decodeGeneric(integrations.WithRequestHolder(context.Background(), h), zap.NewNop(), []byte("PING"), proxySide, nil, pingStore{}, models.OutgoingOptions{})
	if err == nil {
		t.Fatal("a request no exchange answers returned no miss")
	}
	if n := h.held(); n != 1 {
		t.Fatalf("held %d after the miss was returned, want 1 until the proxy files it", n)
	}
}

// Under the switch, a request a read returned with the deadline's timeout is
// a request: a follow-up of exactly 1024 bytes (one full read, then a read
// that times out) is held and answered. Before, the timeout came back with
// the bytes and the request was dropped: never held, never answered, no miss.
func TestTheGenericParserKeepsARequestAReadReturnedWithItsDeadline(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHolds()
	ctx, cancel := context.WithCancel(integrations.WithRequestHolder(context.Background(), h))
	defer cancel()
	app, proxySide := net.Pipe()
	defer app.Close()
	big := bytes.Repeat([]byte("A"), 1024)
	m := exchange("mock-big", string(big))
	m.TestModeInfo.IsFiltered = true
	m.Spec.GenericResponses = []models.Payload{{Message: []models.OutputBinary{{Type: models.String, Data: "BIGOK"}}}}
	done := make(chan error, 1)
	go func() {
		done <- decodeGeneric(ctx, zap.NewNop(), []byte("PING"), proxySide, nil, pingStore{mocks: []*models.Mock{pingPong(), m}}, models.OutgoingOptions{})
	}()
	got := make([]byte, 4)
	if _, err := io.ReadFull(app, got); err != nil || string(got) != "PONG" {
		t.Fatalf("answer %q, %v", got, err)
	}
	if _, err := app.Write(big); err != nil {
		t.Fatal(err)
	}
	ans := make([]byte, 5)
	if _, err := io.ReadFull(app, ans); err != nil || string(ans) != "BIGOK" {
		t.Fatalf("the 1024-byte request's answer %q, %v", ans, err)
	}
	cancel()
	_ = app.Close()
	<-done
}
