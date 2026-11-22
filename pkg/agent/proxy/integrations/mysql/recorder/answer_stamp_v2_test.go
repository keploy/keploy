package recorder

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap/zapcore"
)

// What orders a connection's two directions is the capture's numbering
// (fakeconn.Chunk.ConnSeq), never the chunks' times: a producer stamps each
// direction as it reads it, and a response's chunk can carry a time before the
// time of the command it answers, though it was captured after it. This
// package's own harness does, under CPU contention: it stamps the two
// directions' chunks as two goroutines push them
// (TestRecordV2_TheServersOwnErrorEndsTheRecording flaked so). The recorder stamps a
// mock with its command's read time and its response's, and stamped so, the
// mock was answered before it was asked: the replay leaves such a mock out (a
// replay without KEPLOY_PROVEN), and the session says so at record. A response
// is read after its command, so its mock is stamped answered no earlier than
// asked: the stamps follow the order the bytes were read in. Each mock so
// moved is said at Debug.
func TestRecordV2_AnAnswerStampedBeforeItsCommandIsAnsweredWhenItWasAsked(t *testing.T) {
	resetWarnLimiters()
	xs := kitTraffic(t, []int{7}, false)
	client, server := onTheWire(xs, 64)
	// Every answer's chunks are stamped a millisecond before the command
	// they answer, their capture order unchanged.
	for i := range server {
		server[i].ReadAt = server[i].ReadAt.Add(-time.Millisecond)
	}
	rec := recordChunks(t, client, server, true, time.Minute)
	if !rec.returned || rec.err != nil {
		t.Fatalf("RecordV2 returned=%v err=%v", rec.returned, rec.err)
	}
	if got := queryMocks(rec.mocks); got != len(xs) {
		t.Fatalf("%d query mocks, want %d", got, len(xs))
	}
	for _, m := range rec.mocks {
		if m.Spec.ResTimestampMock.Before(m.Spec.ReqTimestampMock) {
			t.Errorf("the mock of %s is answered at %s, before it was asked at %s", m.Spec.Metadata["requestOperation"],
				m.Spec.ResTimestampMock.Format(time.RFC3339Nano), m.Spec.ReqTimestampMock.Format(time.RFC3339Nano))
		}
	}
	if n := rec.logs.FilterMessageSnippet("answered before it was asked").FilterLevelExact(zapcore.WarnLevel).Len(); n != 0 {
		t.Fatalf("%d WARN lines say a mock was answered before it was asked, want none", n)
	}
	// Each mock it moved is said, at Debug, so a stamping bug stays findable.
	if n := rec.logs.FilterMessageSnippet("is stamped answered when the command was read").FilterLevelExact(zapcore.DebugLevel).Len(); n != len(xs) {
		t.Fatalf("%d Debug lines say a response's stamp was moved, want one for each of the %d mocks", n, len(xs))
	}
}

// A server can answer a command before it has read all of it: MySQL's ERR to a
// packet larger than its max_allowed_packet comes once it has read the
// packet's header, while the client is still sending the rest. The answer is
// then captured, and stamped, between the command's first chunk and its last,
// with the capture's times agreeing with its order, by keploy's relay and by a
// kernel capture alike. The recorder stamps a command when it has read all of
// it (its last chunk), so the mock was answered before it was asked. It is
// stamped answered when the command was read.
func TestRecordV2_AnErrorBeforeTheCommandIsReadWholeIsAnsweredWhenItWasRead(t *testing.T) {
	resetWarnLimiters()
	xs := kitTraffic(t, []int{7}, false)[:1]
	x := xs[0]
	errPkt := wrapPacket(append([]byte{0xff, 0x99, 0x04, '#'}, "08S01Got a packet bigger than 'max_allowed_packet' bytes"...), 1)
	half := len(x.command) / 2
	base := time.Date(2026, 10, 2, 10, 47, 0, 0, time.UTC)
	seq := uint64(handshakeChunks)
	next := func() uint32 { seq++; return fakeconn.ConnSeqOf(seq) }
	client := []fakeconn.Chunk{{Dir: fakeconn.FromClient, ConnSeq: next(), Bytes: x.command[:half], ReadAt: base.Add(1 * time.Microsecond)}}
	server := []fakeconn.Chunk{{Dir: fakeconn.FromDest, ConnSeq: next(), Bytes: errPkt, ReadAt: base.Add(2 * time.Microsecond)}}
	client = append(client, fakeconn.Chunk{Dir: fakeconn.FromClient, ConnSeq: next(), Bytes: x.command[half:], ReadAt: base.Add(3 * time.Microsecond)})
	rec := recordChunks(t, client, server, true, time.Minute)
	if !rec.returned || rec.err != nil {
		t.Fatalf("RecordV2 returned=%v err=%v", rec.returned, rec.err)
	}
	var m *models.Mock
	for _, mk := range rec.mocks {
		if mk.Spec.Metadata["type"] != "config" {
			m = mk
		}
	}
	if m == nil || m.Spec.Metadata["responseOperation"] != mysql.StatusToString(mysql.ERR) {
		t.Fatalf("recorded %+v; want the command's mock, answered with the ERR", m)
	}
	if got, want := m.Spec.ReqTimestampMock, base.Add(3*time.Microsecond); !got.Equal(want) {
		t.Errorf("the command is stamped asked at %s; want %s, when it was read whole", got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
	if m.Spec.ResTimestampMock.Before(m.Spec.ReqTimestampMock) {
		t.Errorf("the mock is answered at %s, before it was asked at %s", m.Spec.ResTimestampMock.Format(time.RFC3339Nano), m.Spec.ReqTimestampMock.Format(time.RFC3339Nano))
	}
	if n := rec.logs.FilterMessageSnippet("answered before it was asked").FilterLevelExact(zapcore.WarnLevel).Len(); n != 0 {
		t.Fatalf("%d WARN lines say a mock was answered before it was asked, want none", n)
	}
}
