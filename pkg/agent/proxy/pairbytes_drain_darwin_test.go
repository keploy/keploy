//go:build darwin

package proxy

import (
	"bytes"
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// On macOS, the drain a session's last read and keploy test's set end wait on
// holds for a connection whose application end ended until the proxy has
// counted its close and is done with it: whether the application's FIN was
// taken by the proxy's side, or the application closed with bytes the
// proxy's full receive buffer had not taken. macOS's proxy side can take that
// FIN too (Linux's holds it behind the bytes,
// TestADrainWaitsForTheCloseOfAConnectionTheApplicationsStopEnded). Until
// that FIN is taken, the application's socket, read from macOS's TCP table,
// shows it on its way: one of the two tells, so the row asks only that the
// drain holds. A connection the application holds open never holds it.
func TestADrainWaitsForTheCloseOfAConnectionTheApplicationsStopEndedOnDarwin(t *testing.T) {
	t.Run("the application's FIN taken", func(t *testing.T) {
		p, app, proxySide, reader := darwinPair(t)
		if _, err := app.Write([]byte("publish 1\n")); err != nil {
			t.Fatal(err)
		}
		untilUnread(t, proxySide, 10)
		_ = app.Close()
		awaitState(t, proxySide, peerEnded, "the application's FIN")
		got := drainReturns(context.Background(), p)
		drainHolds(t, got, "while the proxy held a connection whose FIN it had taken")
		_ = reader.Close() // the handler's close counts the bytes it tosses
		if n := lossesOf(p, models.CaptureLossAppBytesLost); n != 1 {
			t.Fatalf("premise: the close counted %d app_bytes_lost, want 1", n)
		}
		drainHolds(t, got, "before the proxy's handler was done with the connection")
		p.paired.remove(proxySide)
		if err := drainAnswers(t, got, "once the proxy was done with the connection"); err != nil {
			t.Fatalf("the drain: %v", err)
		}
	})
	t.Run("the application closed with the proxy's buffer full", func(t *testing.T) {
		p, app, proxySide, reader := darwinPair(t)
		_ = proxySide.SetReadBuffer(4096)
		_ = app.SetWriteBuffer(4096)
		_ = app.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
		if n, err := app.Write(bytes.Repeat([]byte("x"), 8<<20)); err == nil || n == 0 {
			t.Fatalf("premise: the write fills the buffers and times out, wrote %d, %v", n, err)
		}
		_ = app.Close()
		ps := readProxySide(proxySide)
		t.Logf("the proxy's side at the drain: state %d, peer ended %v, %v", ps.state, ps.err == nil && peerEnded(ps.state), ps.err)
		got := drainReturns(context.Background(), p)
		drainHolds(t, got, "while the application's socket had closed and the proxy had not counted its close")
		_ = reader.Close() // its close counts the bytes it tosses, or that it cannot tell
		if n := lossesOf(p, models.CaptureLossAppBytesLost) + lossesOf(p, models.CaptureLossAppBytesUntold); n != 1 {
			t.Fatalf("premise: the close counted %d app_bytes_lost or app_bytes_untold, want 1", n)
		}
		drainHolds(t, got, "before the proxy's handler was done with the connection")
		p.paired.remove(proxySide)
		if err := drainAnswers(t, got, "once the proxy was done with the connection"); err != nil {
			t.Fatalf("the drain: %v", err)
		}
	})
	t.Run("a connection the application holds open", func(t *testing.T) {
		p, app, _, reader := darwinPair(t)
		if _, err := app.Write([]byte("publish 1\n")); err != nil {
			t.Fatal(err)
		}
		if _, err := reader.Read(make([]byte, 10)); err != nil {
			t.Fatal(err)
		}
		if err := drainAnswers(t, drainReturns(context.Background(), p), "with only a connection the application holds open"); err != nil {
			t.Fatal(err)
		}
	})
}
