package pkg

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/internal/testnet"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A test request refused by an application keploy started, whose exit ends
// the run's context (EndsWithApp): the request waits for its address to
// accept a connection and is then sent again. Only the context ends the
// wait, never a count of re-sends or a time. The application listens only
// when the row says so (serve), long after every re-send the bounded path
// would have made (connRefusedRetryBackoff * (1+2+3) = 900 ms).

// refusedPastTheBound is how long a row holds the address refused: past
// every re-send of the bounded path, with room for a loaded host.
const refusedPastTheBound = 2 * time.Second

func refusingAddr(t *testing.T) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(testnet.SharedPort(t)))
}

// serve begins answering on addr: 200 to a request whose body is the one
// sendInBackground sends (or none), 400 to any other.
func serve(t *testing.T, addr string) {
	ln := testnet.ListenShared(t, "tcp", addr)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if len(b) > 0 && string(b) != "body" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

type sendResult struct {
	resp *http.Response
	err  error
}

func sendInBackground(ctx context.Context, logger *zap.Logger, addr string) <-chan sendResult {
	done := make(chan sendResult, 1)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+addr+"/a", strings.NewReader("body"))
	go func() {
		resp, err := doRequestWithConnRefusedRetry(ctx, logger, &http.Client{}, req, nil, 0)
		done <- sendResult{resp, err}
	}()
	return done
}

func waitingLines(logs *observer.ObservedLogs) int {
	return logs.FilterMessageSnippet("waiting for it to accept a connection").Len()
}

// The failure: the first tests went to a Spring application still starting,
// were refused four times in about a second, and failed with status 0.
func TestARefusedTestRequestWaitsForItsAddressWhileTheAppRuns(t *testing.T) {
	addr := refusingAddr(t)
	core, logs := observer.New(zapcore.InfoLevel)
	ctx, cancel := context.WithCancel(EndsWithApp(context.Background()))
	defer cancel()
	done := sendInBackground(ctx, zap.New(core), addr)

	select {
	case r := <-done:
		t.Fatalf("the request ended (%v, %v) while its address refused and the application ran", r.resp, r.err)
	case <-time.After(refusedPastTheBound):
	}
	if n := waitingLines(logs); n != 1 {
		t.Fatalf("said %d times that the request waits for %s, want once", n, addr)
	}
	if line := logs.FilterMessageSnippet("waiting for it to accept a connection").All()[0].Message; !strings.Contains(line, addr) {
		t.Errorf("the waiting line does not name the address it waits for, %s: %q", addr, line)
	}

	serve(t, addr)
	select {
	case r := <-done:
		if r.err != nil || r.resp.StatusCode != http.StatusOK {
			t.Fatalf("want the application's 200 to the whole body once it accepted, got (%v, %v)", r.resp, r.err)
		}
		_ = r.resp.Body.Close()
	case <-time.After(10 * time.Second):
		t.Fatal("the request was not sent again once its address accepted a connection")
	}
	if n := waitingLines(logs); n != 1 {
		t.Errorf("said %d times that the request waits, want once", n)
	}
}

// The application's exit cancels the run's context: the waiting request
// ends at once, with the refusal, and is not sent.
func TestARefusedTestRequestStopsWaitingWhenTheAppExits(t *testing.T) {
	addr := refusingAddr(t)
	ctx, appExited := context.WithCancel(EndsWithApp(context.Background()))
	defer appExited()
	done := sendInBackground(ctx, zap.NewNop(), addr)

	select {
	case r := <-done:
		t.Fatalf("the request ended (%v, %v) before the application exited", r.resp, r.err)
	case <-time.After(refusedPastTheBound):
	}
	appExited()
	select {
	case r := <-done:
		if r.err == nil || !errors.Is(r.err, syscall.ECONNREFUSED) {
			t.Fatalf("want the refusal once the application exited, got (%v, %v)", r.resp, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not end when the application exited")
	}
}

// A context that the application's exit does not end (no command: keploy did
// not start the application) keeps the bounded re-send: nothing else would
// ever end the wait.
func TestARefusedTestRequestIsReSentABoundedNumberOfTimesWithoutAnAppKeployStarted(t *testing.T) {
	addr := refusingAddr(t)
	done := sendInBackground(context.Background(), zap.NewNop(), addr)
	select {
	case r := <-done:
		if r.err == nil || !errors.Is(r.err, syscall.ECONNREFUSED) {
			t.Fatalf("want the refusal after the bounded re-sends, got (%v, %v)", r.resp, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the bounded re-sends did not end")
	}
}

// An address the application can never be reached at still fails at once,
// before any wait: waiting there would wait for the whole run.
func TestARefusalAtAnUnreachableAddressFailsAtOnceWhileTheAppRuns(t *testing.T) {
	addr := refusingAddr(t)
	ctx, cancel := context.WithCancel(EndsWithApp(context.Background()))
	defer cancel()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/a", nil)
	reach := &answerReach{reason: "it is not published on the host"}
	start := time.Now()
	_, err := doRequestWithConnRefusedRetry(ctx, zap.NewNop(), &http.Client{}, req, reach, 0)
	var unreachable *UnreachableAppPortError
	if !errors.As(err, &unreachable) {
		t.Fatalf("want an UnreachableAppPortError, got %v", err)
	}
	if took := time.Since(start); took > connRefusedRetryBackoff {
		t.Errorf("waited %v before failing at an unreachable address", took)
	}
}

// A gRPC test's dial waits the same way.
func TestARefusedGRPCDialWaitsForItsAddressWhileTheAppRuns(t *testing.T) {
	addr := refusingAddr(t)
	core, logs := observer.New(zapcore.InfoLevel)
	ctx, cancel := context.WithCancel(EndsWithApp(context.Background()))
	defer cancel()
	type dialResult struct {
		conn net.Conn
		err  error
	}
	done := make(chan dialResult, 1)
	go func() {
		conn, err := dialTCPWithConnRefusedRetry(ctx, zap.New(core), addr, nil, 0)
		done <- dialResult{conn, err}
	}()

	select {
	case r := <-done:
		t.Fatalf("the dial ended (%v) while its address refused and the application ran", r.err)
	case <-time.After(refusedPastTheBound):
	}
	if n := waitingLines(logs); n != 1 {
		t.Fatalf("said %d times that the dial waits for %s, want once", n, addr)
	}
	serve(t, addr)
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("want a connection once the address accepted, got %v", r.err)
		}
		_ = r.conn.Close()
	case <-time.After(10 * time.Second):
		t.Fatal("the dial was not made again once its address accepted a connection")
	}
}

// WaitForAccept says once that addr refused, however many tries it makes, and
// waits with no one to tell as well; on a context already ended it says
// nothing and tries no more.
func TestWaitForAcceptSaysOnceAndEndsOnlyByAcceptOrContext(t *testing.T) {
	addr := refusingAddr(t)
	said := 0
	done := make(chan error, 2)
	go func() { done <- WaitForAccept(context.Background(), addr, func(error) { said++ }) }()
	go func() { done <- WaitForAccept(context.Background(), addr, nil) }()
	select {
	case err := <-done:
		t.Fatalf("the wait ended (%v) before %s accepted a connection", err, addr)
	case <-time.After(time.Second):
	}
	serve(t, addr)
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("want nil once %s accepted, got %v", addr, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the wait did not end once the address accepted a connection")
		}
	}
	if said != 1 {
		t.Errorf("said %d times that %s refused, want once", said, addr)
	}

	ended, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	if err := WaitForAccept(ended, refusingAddr(t), func(error) { calls++ }); !errors.Is(err, context.Canceled) {
		t.Errorf("on an ended context: want its error, got %v", err)
	}
	if calls != 0 {
		t.Errorf("on an ended context it said %d times that the address refused", calls)
	}
}

// One request's waits are said once: a request refused again after its
// address accepted waits again, silently.
func TestWaitToReSendSaysOncePerRequest(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	logger := zap.New(core)
	addr := refusingAddr(t)
	said := false
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
		err := waitToReSend(ctx, logger, "test request", addr, &said)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("wait %d: want the context's end, got %v", i+1, err)
		}
	}
	if n := waitingLines(logs); n != 1 {
		t.Errorf("said %d times for one request, want once", n)
	}
}
