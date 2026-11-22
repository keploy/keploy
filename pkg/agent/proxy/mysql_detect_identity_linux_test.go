//go:build linux

package proxy

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

// A moved MySQL endpoint is MySQL connection by connection, never as a port:
// a client of it that has already spoken — a MySQL client never does — is not
// greeted, however its port was judged before.
func TestReplayMovedEndpointDoesNotGreetAClientThatSpoke(t *testing.T) {
	p := testProxy(t)
	recordedMySQL(p, "172.20.0.2:3306", "")

	first := verdict(t, p, newSilentClient(), "172.20.0.2:8288")
	if !first.IsMySQL {
		t.Fatalf("reason %q: a silent client of the recorded host on a moved port is MySQL", first.Reason)
	}
	if p.mysqlPorts.Has(8288) {
		t.Fatal("a moved endpoint must not enter the fast-path set")
	}

	app, srcConn := clientPair(t)
	if _, err := app.Write([]byte{0x16, 0x03, 0x01, 0x00, 0x2f}); err != nil {
		t.Fatal(err)
	}
	arrived(t, srcConn)
	second := awaitEvent(t, replayProbe(context.Background(), p, srcConn, "172.20.0.2:8288", zap.NewNop()), "the second probe's verdict")
	if second.IsMySQL || second.Reason != "client-spoke-first" {
		t.Fatalf("IsMySQL=%v reason %q: a client that spoke first must never be greeted", second.IsMySQL, second.Reason)
	}
	if p.mysqlPorts.IsKnownNotMysql(8288) {
		t.Fatal("a client that spoke first to a moved endpoint marked its port not MySQL: the next silent client of it would never be greeted")
	}
	third := verdict(t, p, newSilentClient(), "172.20.0.2:8288")
	if !third.IsMySQL {
		t.Fatalf("reason %q: a silent client after one that spoke is MySQL again", third.Reason)
	}
}
