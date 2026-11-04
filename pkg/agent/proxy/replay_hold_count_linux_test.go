//go:build linux

package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// What a replay of held answers keeps open: each held answer it serves holds
// one proxy connection past the app's close, until a reset or the replay's
// end. A test's own recordings are served once each, so an app that asks for
// the same call 2N times, closing each connection once it has read what it
// read at record, is served N held answers from N recorded ones, and the rest
// find no recording to serve: at most N connections stay open, however often
// the app asks again.
func TestReplay_HeldAnswersOfATestKeepAtMostOneConnectionEach(t *testing.T) {
	const request = "GET /charge HTTP/1.1\r\nHost: pay.example\r\n\r\n"
	const want = "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\n01234"
	const n = 3
	for _, kind := range []string{"timeout", "stopped_reading"} {
		for _, proven := range []string{"", "1"} {
			t.Run(kind+"/KEPLOY_PROVEN="+proven, func(t *testing.T) {
				t.Setenv("KEPLOY_PROVEN", proven)
				mocks := make([]*models.Mock, 0, n)
				for i := range n {
					m := heldMock(kind, true)
					m.Name = fmt.Sprintf("mock-held-%d", i)
					mocks = append(mocks, m)
				}
				p := replayProxyTo(t, "10.96.0.9:80", models.OutgoingOptions{})
				p.mockManager.SetFilteredMocks(mocks)
				p.errChannel = make(chan error, 4*n)
				p.Integrations = map[integrations.IntegrationType]integrations.Integrations{
					integrations.HTTP: httpint.New(zap.NewNop()),
				}
				p.integrationsPriority = []ParserPriority{{Priority: 1, ParserType: integrations.HTTP}}
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				var held []chan error
				served := 0
				for i := range 2 * n {
					app, src := acceptedConn(t)
					done := make(chan error, 1)
					go func() { done <- p.handleConnection(ctx, src) }()
					if _, err := io.WriteString(app, request); err != nil {
						t.Fatal(err)
					}
					_ = app.SetReadDeadline(time.Now().Add(10 * time.Second)) // turns a hang into a failure
					got := make([]byte, len(want))
					k, _ := io.ReadFull(app, got)
					isHeld := string(got[:k]) == want
					if isHeld {
						served++
						// Nothing more, and no end, comes: the app's own timeout fires.
						_ = app.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
						if m, err := app.Read(make([]byte, 1)); m != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
							t.Fatalf("request %d: after the held answer the app read %d byte(s), %v; want nothing", i, m, err)
						}
					}
					// The app gives up and closes its socket.
					_ = app.Close()
					if !isHeld {
						// Its error (no recording to serve) is not this row's.
						_ = awaitEvent(t, done, fmt.Sprintf("handleConnection returning after request %d, "+
							"which was served no held answer (%q)", i, got[:k]))
						continue
					}
					held = append(held, done)
				}
				var open []chan error
				for _, done := range held {
					select {
					case <-done:
					case <-time.After(200 * time.Millisecond):
						open = append(open, done)
					}
				}
				if served != n || len(open) > n {
					t.Fatalf("a replay of %d held answers served one to %d of %d requests and keeps %d connection(s) open; "+
						"want %d served and at most %d open", n, served, 2*n, len(open), n, n)
				}
				cancel()
				for i, done := range open {
					_ = awaitEvent(t, done, fmt.Sprintf("held connection %d returning at the replay's end", i))
				}
			})
		}
	}
}

// A recording the replay keeps for every test (here a session-lifetime config
// mock) is served each time the app asks for it, and each answer is held. An
// app that asks 10 times, closing each connection once it has read what it
// read at record, has each connection held past its close only until it asks
// again: the next answer held from the same recording ends the hold before
// it. So 10 asks leave exactly one connection open, the last, until the
// replay's end.
func TestReplay_ARetainedHeldRecordingKeepsOneConnectionHowEverOftenItIsAsked(t *testing.T) {
	const request = "GET /charge HTTP/1.1\r\nHost: pay.example\r\n\r\n"
	const want = "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\n01234"
	const asks = 10
	for _, kind := range []string{"timeout", "stopped_reading"} {
		for _, proven := range []string{"", "1"} {
			t.Run(kind+"/KEPLOY_PROVEN="+proven, func(t *testing.T) {
				t.Setenv("KEPLOY_PROVEN", proven)
				m := heldMock(kind, true)
				m.Spec.Metadata = map[string]string{"type": "config"}
				m.TestModeInfo.Lifetime = models.LifetimeSession
				p := replayProxyTo(t, "10.96.0.9:80", models.OutgoingOptions{})
				p.mockManager.SetUnFilteredMocks([]*models.Mock{m})
				p.errChannel = make(chan error, 4*asks)
				p.Integrations = map[integrations.IntegrationType]integrations.Integrations{
					integrations.HTTP: httpint.New(zap.NewNop()),
				}
				p.integrationsPriority = []ParserPriority{{Priority: 1, ParserType: integrations.HTTP}}
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				var prev chan error
				for i := range asks {
					app, src := acceptedConn(t)
					done := make(chan error, 1)
					go func() { done <- p.handleConnection(ctx, src) }()
					if _, err := io.WriteString(app, request); err != nil {
						t.Fatal(err)
					}
					_ = app.SetReadDeadline(time.Now().Add(10 * time.Second)) // turns a hang into a failure
					got := make([]byte, len(want))
					if k, err := io.ReadFull(app, got); err != nil {
						t.Fatalf("ask %d: the app read %q, then %v; want the held answer %q", i, got[:k], err, want)
					}
					if string(got) != want {
						t.Fatalf("ask %d: the app read %q; want the held answer %q", i, got, want)
					}
					// Nothing more, and no end, comes: the app's own timeout fires.
					_ = app.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
					if k, err := app.Read(make([]byte, 1)); k != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
						t.Fatalf("ask %d: after the held answer the app read %d byte(s), %v; want nothing", i, k, err)
					}
					// The app gives up and closes its socket.
					_ = app.Close()
					if prev != nil {
						_ = awaitEvent(t, prev, fmt.Sprintf("the connection of ask %d returning once ask %d "+
							"was served from the same recording", i-1, i))
					}
					prev = done
				}
				select {
				case err := <-prev:
					t.Fatalf("the last ask's connection returned %v before the replay's end; want it held", err)
				case <-time.After(200 * time.Millisecond):
				}
				cancel()
				_ = awaitEvent(t, prev, "the last ask's connection returning at the replay's end")
			})
		}
	}
}
