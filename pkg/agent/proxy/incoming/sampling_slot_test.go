package proxy

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A sampled connection gives its sampling slot back before its client can
// have the end of its answer. A client that sends its requests one after
// another, each on a new connection once it has the answer before it whole,
// then has every request captured with one slot (--enable-sampling=1),
// however late the handler of the exchange before runs again. Given back only
// once that handler had returned, the slot could still be held when the next
// connection came (a loaded host runs the handler late), and that connection
// was passed through uncaptured: its request was missing from the recording
// and from the static-dedup counts, with nothing said.
//
// The handler of the first exchange is held (answerWritten) once its answer
// is written whole: the client has it all, and the handler has not run on.
// The second request is sent on a new connection meanwhile. One capture slot
// is all there is: the first exchange holds it until it is let go, and the
// second's capture waits for it, holding its sampling slot.
//
// An answer that ends as the app closes its connection is no shape here:
// its client has the end only as the handler has returned.
func TestASequentialClientIsNotBypassedByTheExchangeBefore(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	okAnswer := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "2")
		_, _ = w.Write([]byte("ok"))
	}
	for _, shape := range []struct {
		name string
		// body is the first request's body; the second request is a GET.
		body string
		// first answers the first request; the second has okAnswer.
		first http.HandlerFunc
		want  []string
	}{
		{"an answer of known length", "", okAnswer, []string{"/first", "/second"}},
		{"a request with a body", `{"sku":"a","qty":1}`, okAnswer, []string{"/first", "/second"}},
		{"a chunked answer", "", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("ok"))
			w.(http.Flusher).Flush()
		}, []string{"/first", "/second"}},
		{"an answer with no body", "", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}, []string{"/first", "/second"}},
		// Nothing of a stream is captured: its slot is given back as well.
		{"a streamed answer, not recorded", "", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: ok\n\n"))
			w.(http.Flusher).Flush()
		}, []string{"/second"}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			// Restored once the handlers are gone: Cleanup runs last-in
			// first, and the teardown registered last waits for them.
			savedWritten := answerWritten
			t.Cleanup(func() { answerWritten = savedWritten })
			savedSem := captureHookSem
			t.Cleanup(func() { captureHookSem = savedSem })
			captureHookSem = make(chan struct{}, 1)

			app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/first" {
					shape.first(w, r)
					return
				}
				okAnswer(w, r)
			}))
			t.Cleanup(app.Close)
			appAddr := app.Listener.Addr().String()

			var (
				mu       sync.Mutex
				captured []string
			)
			stubCaptureHook(t, func(_ context.Context, _ *zap.Logger, _ chan *models.TestCase,
				req *http.Request, _ *http.Response, _, _ time.Time,
				_ models.IncomingOptions, _ bool, _ bool, _ uint16) {
				mu.Lock()
				captured = append(captured, req.URL.Path)
				mu.Unlock()
			})

			held := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			letGo := func() { releaseOnce.Do(func() { close(release) }) }
			var written atomic.Int32
			answerWritten = func() {
				if written.Add(1) == 1 {
					close(held)
					<-release
				}
			}

			cfg := &config.Config{}
			cfg.Agent.RecordRequests = true
			cfg.Agent.EnableSampling = 1
			pm := New(zap.NewNop(), nil, cfg)
			serve := servedConns(t, pm, letGo)

			firstServed := serve.exchange(t, appAddr, "/first", shape.body)
			select {
			case <-held:
			case <-serve.ctx.Done():
				t.Fatal("the first exchange's handler never wrote its answer whole")
			}
			secondServed := serve.exchange(t, appAddr, "/second", "")
			letGo()
			<-firstServed
			<-secondServed
			pm.captures.Wait()

			mu.Lock()
			defer mu.Unlock()
			sort.Strings(captured)
			if !reflect.DeepEqual(captured, shape.want) {
				t.Fatalf("a client that sent its second request once it had the first answer whole had %v captured with one sampling slot, want %v: the slot of the exchange before was still held", captured, shape.want)
			}
		})
	}
}

// A sampled connection whose capture finds no capture slot free keeps its
// sampling slot until its capture has one, and the connections that come
// meanwhile are passed through, not sampled: the sampling budget bounds the
// handlers that wait for a capture slot, each with the bodies it captured in
// hand. Its slot given back as its answer's end was found regardless, every
// connection after it was sampled in turn and waited too, without bound while
// the captures were slower than the app.
func TestASampledExchangeWaitingForACaptureSlotKeepsItsSamplingSlot(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	savedSem := captureHookSem
	t.Cleanup(func() { captureHookSem = savedSem })
	sem := make(chan struct{}, 1)
	sem <- struct{}{} // every capture slot is held until the connections are served
	captureHookSem = sem

	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "2")
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(app.Close)
	appAddr := app.Listener.Addr().String()

	var (
		mu       sync.Mutex
		captured []string
	)
	stubCaptureHook(t, func(_ context.Context, _ *zap.Logger, _ chan *models.TestCase,
		req *http.Request, _ *http.Response, _, _ time.Time,
		_ models.IncomingOptions, _ bool, _ bool, _ uint16) {
		mu.Lock()
		captured = append(captured, req.URL.Path)
		mu.Unlock()
	})

	cfg := &config.Config{}
	cfg.Agent.RecordRequests = true
	cfg.Agent.EnableSampling = 1
	pm := New(zap.NewNop(), nil, cfg)
	freed := false
	serve := servedConns(t, pm, func() {
		if !freed {
			freed = true
			<-sem
		}
	})

	const conns = 4
	var served []chan struct{}
	for i := 1; i <= conns; i++ {
		served = append(served, serve.exchange(t, appAddr, "/"+strconv.Itoa(i), ""))
	}
	freed = true
	<-sem
	for _, s := range served {
		<-s
	}
	pm.captures.Wait()

	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(captured, []string{"/1"}) {
		t.Fatalf("with one sampling slot and no capture slot free, %d sequential connections had %v captured, want only the first's [/1]: a connection sampled while the one before waited for a capture slot waits as well", conns, captured)
	}
}

// servedConnSet serves connections to pm, under a context that ends at the
// latest as the test does.
type servedConnSet struct {
	pm  *IngressProxyManager
	ctx context.Context

	mu     sync.Mutex
	served []chan struct{}
}

// servedConns is a servedConnSet whose teardown, registered after the test's
// restores so it runs before them, lets the held go (letGo), ends the
// context, and waits for every handler and capture: what the test swapped is
// restored only once nothing reads it.
func servedConns(t *testing.T, pm *IngressProxyManager, letGo func()) *servedConnSet {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	s := &servedConnSet{pm: pm, ctx: ctx}
	t.Cleanup(func() {
		letGo()
		cancel()
		s.mu.Lock()
		served := s.served
		s.mu.Unlock()
		for _, c := range served {
			<-c
		}
		pm.captures.Wait()
	})
	return s
}

// exchange sends one request on a new connection, a GET or, with a body, a
// POST, and reads its answer whole before it closes the connection. served
// closes as its handler returns.
func (s *servedConnSet) exchange(t *testing.T, appAddr, path, body string) (served chan struct{}) {
	t.Helper()
	proxied, client := tcpPair(t)
	served = make(chan struct{})
	s.mu.Lock()
	s.served = append(s.served, served)
	s.mu.Unlock()
	go func() {
		defer close(served)
		defer func() { _ = proxied.Close() }()
		s.pm.ServeLive(s.ctx, proxied, appAddr, 8080)
	}()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	request := "GET " + path + " HTTP/1.1\r\nHost: app\r\n\r\n"
	if body != "" {
		request = "POST " + path + " HTTP/1.1\r\nHost: app\r\nContent-Type: application/json\r\nContent-Length: " +
			strconv.Itoa(len(body)) + "\r\n\r\n" + body
	}
	if _, err := client.Write([]byte(request)); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("%s: the client had no answer: %v", path, err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("%s: the client did not have the answer whole: %v", path, err)
	}
	_ = resp.Body.Close()
	_ = client.Close()
	return served
}
