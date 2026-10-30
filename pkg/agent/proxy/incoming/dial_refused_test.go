package proxy

import (
	"context"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/internal/testnet"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func neverFinal() bool { return false }

// refusedErr is the error of a real dial that was refused, as this
// platform spells it.
func refusedErr(t *testing.T) error {
	t.Helper()
	_, err := net.DialTimeout("tcp4", testnet.RefusedAddr(t, "tcp"), 10*time.Second)
	require.Error(t, err)
	require.True(t, isConnRefused(err), "the dial of a refused address failed with %v, not a refusal", err)
	return err
}

// slowRefusal is an app that refuses every dial, and whose refusal comes back
// lag after the dial: a dial given less time than lag times out first, as a
// dial of a refusing app does on a host too loaded to bring the refusal back
// in time. It counts the dials made of it.
type slowRefusal struct {
	lag     time.Duration
	refused error
	mu      sync.Mutex
	dials   int
}

func (s *slowRefusal) attempt(network, _ string, timeout time.Duration) (net.Conn, error) {
	s.mu.Lock()
	s.dials++
	s.mu.Unlock()
	if timeout < s.lag {
		return nil, &net.OpError{Op: "dial", Net: network, Err: os.ErrDeadlineExceeded}
	}
	return nil, s.refused
}

func (s *slowRefusal) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dials
}

// An app that refuses every dial is reported as one that refused, however
// late its refusal comes back: the dial's deadline decides whether a refused
// dial is made again, and never cuts one short. Given what was left of the
// deadline, the last dial had 1 ms to 25 ms (the wait between dials) and timed
// out before a refusal that took longer: an app that refused was reported as
// one that did not answer.
func TestAnAppRefusingEveryDialIsReportedRefusedHoweverLateItsRefusal(t *testing.T) {
	app := &slowRefusal{lag: ingressTargetPollInterval + time.Millisecond, refused: refusedErr(t)}
	_, err := dialIngressTarget(context.Background(), "127.0.0.1:1", 200*time.Millisecond, neverFinal, app.attempt)
	require.True(t, isConnRefused(err), "after %d dials of an app that refused each, the dial failed with %v, not a refusal", app.count(), err)
	require.Greater(t, app.count(), 1, "the dial did not dial a refusing app again before its deadline")
}

// A refusal while the recording stops ends the dial: its client stops the
// app on purpose then, and the refusal is the app's last word. Before that,
// a refusal is dialled again (the app is not listening yet).
func TestARefusalOnceTheRecordingStopsEndsTheDial(t *testing.T) {
	app := &slowRefusal{refused: refusedErr(t)}
	stopping := func() bool { return app.count() >= 3 }
	_, err := dialIngressTarget(context.Background(), "127.0.0.1:1", time.Minute, stopping, app.attempt)
	require.True(t, isConnRefused(err), "the dial failed with %v, not the refusal", err)
	require.Equal(t, 3, app.count(), "dials of an app refusing each: want the two before the stop and the one at it")
}

// The ingress's dial of its app is held to its recording's stop: while the
// recording stops, an app that refuses ends the dial at its first refusal,
// said at DEBUG however late the refusal came back. Before the stop, the
// same app is dialled again until the deadline, and its refusal (a refusal
// still, not a timeout) is an ERROR. The lane's shape: the grpc-linux-debian11
// lane (enterprise pipeline 12645) failed on an ERROR "dial tcp4 ...: i/o
// timeout" for an app its recording had stopped.
func TestTheIngressSaysAnAppRefusingWhileItsRecordingStopsAtDebug(t *testing.T) {
	const msg = "Failed to connect to upstream application. Verify the application is listening on the resolved address."
	refused := refusedErr(t)
	for _, step := range []struct {
		stopping bool
		level    zapcore.Level
	}{{true, zapcore.DebugLevel}, {false, zapcore.ErrorLevel}} {
		core, logs := observer.New(zapcore.DebugLevel)
		pm := New(zap.New(core), nil, &config.Config{})
		app := &slowRefusal{lag: ingressTargetPollInterval + time.Millisecond, refused: refused}
		pm.attempt = app.attempt
		pm.SetStopping(step.stopping)
		client, peer := net.Pipe()
		pm.handleHttp1Connection(context.Background(), client, "127.0.0.1:1", pm.logger, make(chan *models.TestCase, 1), make(chan struct{}, 1), 8080)
		_ = client.Close()
		_ = peer.Close()
		var said []observer.LoggedEntry
		for _, e := range logs.TakeAll() {
			if e.Message == msg {
				said = append(said, e)
			}
		}
		require.Len(t, said, 1, "stopping=%v: the failed dial was said %d times", step.stopping, len(said))
		require.Equal(t, step.level, said[0].Level, "stopping=%v: the refused dial was said at %v", step.stopping, said[0].Level)
		err, _ := said[0].ContextMap()["error"].(string)
		require.Equal(t, refused.Error(), err, "stopping=%v: the dial was said to fail with %q, not the app's refusal", step.stopping, err)
		if step.stopping {
			require.Equal(t, 1, app.count(), "while the recording stops, the dial went on after the app's refusal")
		} else {
			require.Greater(t, app.count(), 1, "before the stop, the dial did not dial the refusing app again")
		}
	}
}
