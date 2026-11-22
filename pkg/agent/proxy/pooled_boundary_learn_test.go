package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// pooledConn is one connection the app keeps open across tests, as a
// driver's pool does: one handler of the HTTP integration serves every
// request sent on it, for the whole row.
type pooledConn struct {
	app net.Conn
	r   *bufio.Reader
}

// openPooled opens a pooled connection to p's handler with the proxy's
// tracker and miss reporter. Its cleanup closes the app's end and waits for
// the handler to return, so the row leaves no handler behind.
func openPooled(t *testing.T, p *Proxy, mm *MockManager, opts models.OutgoingOptions) *pooledConn {
	t.Helper()
	app, agentEnd := net.Pipe()
	ctx := models.WithMockMismatchReporter(context.Background(), p.sendMockNotFoundError)
	ctx = learn.WithTracker(ctx, p.learnTracker)
	h := httpint.New(zap.NewNop())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.MockOutgoing(ctx, agentEnd, &models.ConditionalDstCfg{}, mm, opts)
		_ = agentEnd.Close()
	}()
	t.Cleanup(func() {
		_ = app.Close()
		<-done
	})
	return &pooledConn{app: app, r: bufio.NewReader(app)}
}

// send sends one request on the pooled connection and reads its answer.
func (c *pooledConn) send(t *testing.T, request string) (int, string) {
	t.Helper()
	if _, err := c.app.Write([]byte(request)); err != nil {
		t.Fatalf("the pooled connection took no request: %v", err)
	}
	resp, err := http.ReadResponse(c.r, nil)
	if err != nil {
		t.Fatalf("the pooled connection gave no answer: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func pooledPost(target, body string) string {
	return "POST " + target + " HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nContent-Length: " +
		strconv.Itoa(len(body)) + "\r\n\r\n" + body
}

// A pooled connection keeps being answered after a test boundary, and what
// each of its calls reads of the set's earlier payloads does not grow with
// the set (api-server's e2e suite under keploy mock record).
//
// Test-1 sends the set's earlier recordings on one pooled connection: many,
// each a payload of many words with an instant in it, so that their words
// pass the set's budget of derived data (32 MiB). The connection's later
// calls are the pool's: recorded between tests, as a driver's background
// calls are, so every recording made before each is one of its earlier
// places (the store's RangeRecordedBefore, read as lazy Raws). Each sends a
// clock the app read anew, which the echo reads every earlier payload's
// instants for, and a word the app made anew, which the word rule reads every
// earlier payload's words for. Test-2's first call reads every earlier
// payload's instants once. Every later call, in test-2 and past the next two
// boundaries, must be answered on the same connection without the set's
// earlier payloads' instants read again: only the recordings served since are
// new to the set. Test-3's call reads, once for the set, the places of the
// recordings test-1 was served (GivenPlaces), which test-4's does not again.
//
// Before, past its budget the set's derived data was one LRU by recording.
// The word rule's words filled it, and the learn step reads the earlier
// recordings in the same order for every changed value of every call, so
// its reads missed and a later call read the earlier payloads' instants
// again, up to three times each (196 and 667 texts for test-2's second call
// and test-4's, against 50 and 31 now). On api-server's
// round (14 813 Mongo recordings) that held a pooled connection's request
// for minutes after a test began, its handler never went back to reading,
// and the test's end gave up on the connection.
func TestAPooledConnectionKeepsBeingAnsweredAfterATestBoundary(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	p.learnTracker.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	const earlier, words = 200, 1200
	now := time.Now()
	stamp := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339Nano) }
	var all, set1 []*models.Mock
	bodies := make([]string, earlier)
	for i := range bodies {
		w := make([]string, words)
		for j := range w {
			w[j] = fmt.Sprintf("w%dx%d", i, j)
		}
		bodies[i] = `{"at":"` + stamp(-time.Duration(earlier-i)*time.Minute) + `","note":"` + strings.Join(w, " ") + `"}`
		m := r3Mock(fmt.Sprintf("mock-e%d", i), now.Add(time.Duration(i+1)*time.Millisecond), "POST", "/log", bodies[i], `{"ok":true}`)
		all, set1 = append(all, m), append(set1, m)
	}
	// The pool's calls, each recorded between two tests' windows, each on a
	// path and under a key of its own, so that each call pairs with its own
	// recording whatever the learn step learned of another.
	type poolCall struct {
		test, mock, path, key, word string
		at                          time.Duration
	}
	calls := []poolCall{
		{"test-2", "mock-c1", "/pong1", "a", "a1b2c3d4", 1500 * time.Millisecond},
		{"test-2", "mock-c2", "/pong2", "b", "b2c3d4e5", 1600 * time.Millisecond},
		{"test-3", "mock-c3", "/pong3", "c", "c3d4e5f6", 3500 * time.Millisecond},
		{"test-4", "mock-c4", "/pong4", "d", "d4e5f6a7", 5500 * time.Millisecond},
	}
	for i, c := range calls {
		all = append(all, r3Mock(c.mock, now.Add(c.at), "POST", c.path, `{"at":"`+stamp(c.at)+`","`+c.key+`":"`+c.word+`"}`, fmt.Sprintf(`{"pong":%d}`, i+1)))
	}
	for _, m := range all {
		m.DeriveLifetime()
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{
		{TestCase: "test-1", Start: now, End: now.Add(time.Second)},
		{TestCase: "test-2", Start: now.Add(2 * time.Second), End: now.Add(3 * time.Second)},
		{TestCase: "test-3", Start: now.Add(4 * time.Second), End: now.Add(5 * time.Second)},
		{TestCase: "test-4", Start: now.Add(6 * time.Second), End: now.Add(7 * time.Second)},
	})
	mm.SetServedObserver(p.learnTracker.ServedMock)
	stage := func(start time.Time, names ...string) {
		mm.SetMocksWithWindow(nil, pkg.FilterConfigMocksMapping(context.Background(), zap.NewNop(), all, names), start, start.Add(time.Second))
		p.ContinueTestErrorCapture()
	}
	end := func(test string) {
		misses, err := p.GetMockErrors(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(misses) > 0 {
			t.Fatalf("%s left misses: %+v", test, misses)
		}
	}
	conn := openPooled(t, p, mm, models.OutgoingOptions{Learn: true, LearnPool: true})

	var names1 []string
	for _, m := range set1 {
		names1 = append(names1, m.Name)
	}
	stage(now, names1...)
	for i, body := range bodies {
		if code, got := conn.send(t, pooledPost("/log", body)); code != http.StatusOK || got != `{"ok":true}` {
			t.Fatalf("test-1's call %d on the pooled connection was answered %d %s", i, code, got)
		}
	}
	end("test-1")

	// send sends call i on the pooled connection, its clock read as record's
	// full round reads it (learn.FullRoundLead after it was recorded) and its
	// word made anew, and counts the payload texts the learn step read for
	// their instants.
	send := func(i int) int64 {
		t.Helper()
		c := calls[i]
		body := `{"at":"` + stamp(c.at+learn.FullRoundLead) + `","` + c.key + `":"z9y8x7w` + strconv.Itoa(i) + `"}`
		want := fmt.Sprintf(`{"pong":%d}`, i+1)
		before := learn.InstantReads()
		code, got := conn.send(t, pooledPost(c.path, body))
		read := learn.InstantReads() - before
		if code != http.StatusOK || got != want {
			misses, _ := p.GetMockErrors(context.Background())
			t.Fatalf("%s's call %s %s on the pooled connection was answered %d %s, want %s (misses %+v)", c.test, c.path, body, code, got, want, misses)
		}
		return read
	}
	stage(now.Add(2*time.Second), "mock-c1", "mock-c2")
	first := send(0)
	if first < earlier {
		t.Fatalf("test-2's first call read %d payload texts for their instants, fewer than the set's %d earlier recordings: the row reads nothing of the set", first, earlier)
	}
	second := send(1)
	end("test-2")
	stage(now.Add(4*time.Second), "mock-c3")
	third := send(2)
	end("test-3")
	stage(now.Add(6*time.Second), "mock-c4")
	fourth := send(3)
	end("test-4")
	t.Logf("payload texts read for their instants: test-2's first call %d, its second %d, test-3's %d, test-4's %d (%d earlier recordings)", first, second, third, fourth, earlier)
	// A call past the set's first reads its own values and the recordings
	// served since the previous call alone: a few dozen texts whatever the
	// set's size, never half its earlier payloads. Test-3's reads the places
	// of test-1's recordings once for the set (GivenPlaces), which a call
	// past it does not read again.
	if second >= earlier/2 {
		t.Errorf("test-2's second call read %d payload texts for their instants again (the first read %d, %d earlier recordings): each call re-read the set's earlier payloads", second, first, earlier)
	}
	if fourth >= earlier/2 {
		t.Errorf("test-4's call, after two test boundaries, read %d payload texts for their instants again (the first read %d, %d earlier recordings): each call re-read the set's earlier payloads", fourth, first, earlier)
	}
}
