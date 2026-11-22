//go:build linux

package proxy

import (
	"context"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
)

// A parser that marks its holds can say that, for a while, whether it holds a
// request cannot be told (integrations.MarkUnknown: Mongo's moreToCome group,
// open until a later frame ends it): while marked, its connection is unknown
// in a replay, under the protocol it names, though it holds nothing a wait
// could wait for; released, it is not. A record never reads it, and a
// connection the proxy does not keep marks nothing.
func TestAParserCanMarkItsConnectionUnknownForAWhile(t *testing.T) {
	tr := &inflight{}
	_, proxy := acceptedPair(t, tr)
	ctx := integrations.WithRequestHolder(context.Background(), proxy)
	release := integrations.MarkUnknown(ctx, "mongo_v2 (a moreToCome group open)")
	if got := tr.Unknown(true); strings.Join(got, ",") != "mongo_v2 (a moreToCome group open)" {
		t.Fatalf("while marked: unknown %v", got)
	}
	if got := tr.Unknown(false); len(got) != 0 {
		t.Fatalf("a record waits on no answer: unknown %v", got)
	}
	inner := integrations.MarkUnknown(ctx, "mongo_v2 (a moreToCome group open)")
	release()
	if got := tr.Unknown(true); len(got) != 1 {
		t.Fatalf("one of two marks released: unknown %v, want still marked", got)
	}
	inner()
	release() // a second release changes nothing
	if got := tr.Unknown(true); len(got) != 0 {
		t.Fatalf("every mark released: unknown %v", got)
	}
	if n := proxy.unknownFor.Load(); n != 0 {
		t.Fatalf("marks left: %d", n)
	}
	integrations.MarkUnknown(context.Background(), "x")()
}

// A marked connection whose handler is blocked reading holds nothing a wait
// could wait for: a test's end does not wait for the mark, it is unknown.
func TestAMarkedConnectionIsNotPending(t *testing.T) {
	tr := &inflight{}
	_, proxy := acceptedPair(t, tr)
	go func() {
		buf := make([]byte, 64)
		for {
			if _, err := proxy.Read(buf); err != nil {
				return
			}
		}
	}()
	release := integrations.MarkUnknown(integrations.WithRequestHolder(context.Background(), proxy), "mongo_v2")
	defer release()
	waitSettled(t, tr, func() bool { return proxy.reading.Load() == 1 })
	if n := tr.Pending(true); n != 0 {
		t.Fatalf("a marked connection blocked reading: pending %d, want 0", n)
	}
	if pending, unknown := tr.State(true); pending != 0 || strings.Join(unknown, ",") != "mongo_v2" {
		t.Fatalf("state %d %v, want 0 [mongo_v2]", pending, unknown)
	}
}

// State reads a connection as one view: marked and blocked reading, it is
// unknown; once its read returned with the state's last frame, it is busy
// (and still unknown until the parser releases the mark); released and
// answering, busy; back reading, neither.
func TestStateFollowsAConnectionThroughTheEndOfAMarkedState(t *testing.T) {
	tr := &inflight{}
	app, proxy := acceptedPair(t, tr)
	ctx := integrations.WithRequestHolder(context.Background(), proxy)
	release := integrations.MarkUnknown(ctx, "mongo_v2")
	read := make(chan error)
	again := make(chan struct{})
	go func() {
		buf := make([]byte, 64)
		_, err := proxy.Read(buf)
		read <- err
		if err != nil {
			return
		}
		<-again
		_, _ = proxy.Read(buf)
	}()
	waitSettled(t, tr, func() bool { return proxy.reading.Load() == 1 })
	if pending, unknown := tr.State(true); pending != 0 || len(unknown) != 1 {
		t.Fatalf("marked, blocked reading: state %d %v, want 0 and unknown", pending, unknown)
	}
	if _, err := app.Write([]byte("last frame")); err != nil {
		t.Fatal(err)
	}
	if err := <-read; err != nil {
		t.Fatalf("the last frame's read: %v", err)
	}
	if pending, unknown := tr.State(true); pending != 1 || len(unknown) != 1 {
		t.Fatalf("its last frame read, mark held: state %d %v, want busy and unknown", pending, unknown)
	}
	release()
	if pending, unknown := tr.State(true); pending != 1 || len(unknown) != 0 {
		t.Fatalf("released, answering: state %d %v, want busy and known", pending, unknown)
	}
	close(again)
	waitSettled(t, tr, func() bool { return proxy.reading.Load() == 1 })
	if pending, unknown := tr.State(true); pending != 0 || len(unknown) != 0 {
		t.Fatalf("back reading: state %d %v, want neither", pending, unknown)
	}
}
