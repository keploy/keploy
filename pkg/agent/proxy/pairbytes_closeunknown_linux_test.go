//go:build linux

package proxy

import (
	"errors"
	"net"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/pkg/models"
)

// closeUnknownLines is p's log from now on, where wantCloseUnknown finds the
// lines a close that cannot tell what the application had written says
// (closeUnknown).
func closeUnknownLines(p *Proxy) *observer.ObservedLogs {
	core, logs := observer.New(zapcore.DebugLevel)
	p.logger = zap.New(core)
	return logs
}

// wantCloseUnknown checks that the close of app's connection to the proxy
// answered unknown, said in one line at level naming the connection and an
// error that says why (has).
func wantCloseUnknown(t *testing.T, p *Proxy, lines *observer.ObservedLogs, app, proxySide net.Conn, level zapcore.Level, has string) {
	t.Helper()
	if err := p.PairedBytesErr(); !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), has) {
		t.Fatalf("PairedBytesErr = %v, want unknown, saying %q", err, has)
	}
	all := lines.FilterMessageSnippet("keploy cannot tell what the application had written").All()
	if len(all) != 1 {
		t.Fatalf("%d lines say the close cannot tell what was written, want one: %+v", len(all), all)
	}
	if all[0].Level != level {
		t.Errorf("the line is at level %v, want %v", all[0].Level, level)
	}
	f := all[0].ContextMap()
	if want := app.LocalAddr().String() + " -> " + proxySide.LocalAddr().String(); f["connection"] != want {
		t.Errorf("the line names the connection %v, want %s", f["connection"], want)
	}
	if e, _ := f["error"].(string); !strings.Contains(e, has) {
		t.Errorf("the line's error is %q, want it to say %q", e, has)
	}
}

// A close whose read of the application's socket fails, not one that does not
// find it, cannot tell what was on its way: before, it recorded nothing and
// answered nothing, and a wait judged the connection by what was written when
// it began, so bytes on their way at the close passed unseen. It answers
// unknown now, and says so in one line naming the connection, the read and
// its error.
func TestACloseWhoseReadFailsIsUnknown(t *testing.T) {
	t.Run("no sock_diag socket", func(t *testing.T) {
		p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
		p.settleReads = true
		lines := closeUnknownLines(p)
		emptyDiagFree(t)
		failOpenDiag(t)
		_ = reader.Close()
		wantCloseUnknown(t, p, lines, app, proxySide, zapcore.WarnLevel, "sock_diag socket: too many open files")
	})
	t.Run("no new socket once a kept one's lookup failed", func(t *testing.T) {
		p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
		p.settleReads = true
		lines := closeUnknownLines(p)
		emptyDiagFree(t)
		ds, err := openDiag()
		if err != nil {
			t.Fatal(err)
		}
		diagFree <- ds
		failLookupWritten(t)
		failOpenDiag(t)
		_ = reader.Close()
		wantCloseUnknown(t, p, lines, app, proxySide, zapcore.WarnLevel, "sock_diag socket: too many open files")
	})
	t.Run("a lookup that fails", func(t *testing.T) {
		p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
		p.settleReads = true
		lines := closeUnknownLines(p)
		emptyDiagFree(t)
		failLookupWritten(t)
		_ = reader.Close()
		wantCloseUnknown(t, p, lines, app, proxySide, zapcore.WarnLevel, "netlink: no buffer space")
	})
}

// The line is said once: every wait is unknown from the first such close,
// and a later one tells no more. A proxy whose reads do not wait for the
// paired connections' bytes vouches for none, and says it at debug level.
func TestACloseThatCannotTellSaysSoOnce(t *testing.T) {
	for _, settle := range []bool{true, false} {
		level := zapcore.WarnLevel
		if !settle {
			level = zapcore.DebugLevel
		}
		t.Run(level.String(), func(t *testing.T) {
			p := &Proxy{logger: zap.NewNop(), settleReads: settle}
			app, proxySide, reader := heldPairOn(t, p, 1, "tcp4", "127.0.0.1:0")
			_, _, reader2 := heldPairOn(t, p, 2, "tcp4", "127.0.0.1:0")
			lines := closeUnknownLines(p)
			emptyDiagFree(t)
			failLookupWritten(t)
			_ = reader.Close()
			_ = reader2.Close()
			wantCloseUnknown(t, p, lines, app, proxySide, level, "netlink: no buffer space")
		})
	}
}

func failOpenDiag(t *testing.T) {
	t.Helper()
	was := openDiag
	openDiag = func() (*diagSock, error) { return nil, errors.New("sock_diag socket: too many open files") }
	t.Cleanup(func() { openDiag = was })
}

func failLookupWritten(t *testing.T) {
	t.Helper()
	was := lookupWritten
	lookupWritten = func(*diagSock, *pairedConn, bool) (diagTCP, error) {
		return diagTCP{}, errors.New("netlink: no buffer space")
	}
	t.Cleanup(func() { lookupWritten = was })
}
