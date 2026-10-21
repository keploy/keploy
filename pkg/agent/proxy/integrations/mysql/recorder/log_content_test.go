package recorder

import (
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/utils/logcontent/logtest"
)

// canary is in the message of the server's own ERR: a server's error names
// what it refused, a duplicate key's value say.
const canary = "kp-canary-mysql-record-6a4f"

func ownErr() []byte {
	return wrapPacket(append([]byte{0xff, 0xbf, 0x0f, '#'}, "HY000"+canary...), 0)
}

// The server's own ERR, read as the answer to the client's next command or
// after a PREPARE's definitions, is logged without its message, unless
// KEPLOY_LOG_CONTENT is set.
func TestRecordV2_TheServersOwnErrorIsLoggedWithoutItsMessage(t *testing.T) {
	const msg = "V2: the server dropped the connection with an error of its own; its recording ends here"
	t.Run("as the next command's answer", func(t *testing.T) {
		logtest.Each(t, func(t *testing.T, on bool) {
			resetWarnLimiters()
			s := layOut(kitTraffic(t, []int{7}, false))
			client := append(append([]byte{}, s.client...), wrapPacket([]byte{0x0e}, 0)...)
			server := append(append([]byte{}, s.server...), ownErr()...)
			rec := recordConn(t, chunked(client, 64), chunked(server, 64), false, 5*time.Second)
			if !rec.returned || rec.err != nil {
				t.Fatalf("RecordV2 returned=%v err=%v", rec.returned, rec.err)
			}
			logtest.Check(t, rec.logs, canary, on, msg)
		})
	})
	t.Run("after a PREPARE's definitions, as the next command's answer", func(t *testing.T) {
		def := func() []byte {
			return columnPayload(t, &mysql.ColumnDefinition41{Catalog: "def", Name: "?", OrgName: "?", FixedLength: 0x0c, CharacterSet: 0x3f, ColumnLength: 20, Type: byte(mysql.FieldTypeLongLong)})
		}
		prepare := wrapPacket(append([]byte{mysql.COM_STMT_PREPARE}, "SELECT id FROM t WHERE a = ? AND b = ?"...), 0)
		logtest.Each(t, func(t *testing.T, on bool) {
			j := newJoinedMidStream(t)
			logger, logs := logtest.Observe()
			j.h.logger, j.h.sess.Logger = logger, logger
			j.exchange(prepare, prepareOK(true, [][]byte{def(), def()}, nil))
			// The client's next command, answered by the ERR: the PREPARE's
			// framing is settled on it.
			j.exchange(wrapPacket([]byte{0x0e}, 0), [][]byte{ownErr()})
			if _, err := j.run(t); err != nil {
				t.Fatalf("RecordV2 = %v", err)
			}
			logtest.Check(t, logs, canary, on, msg)
		})
	})
}

// An exchange the recorder leaves out, or stops recording in, is reported
// with its error beside the cause, which is logged at WARN: a decoder's error
// quotes the row value it could not read, and lost framing the bytes a header
// was misread from, a row's as often as not. The WARN carries neither unless
// KEPLOY_LOG_CONTENT is set.
func TestLeaveOut_TheWarningCarriesNoRowItsErrorQuotes(t *testing.T) {
	for name, leave := range map[string]func(sess *supervisor.Session, err error){
		"left out": func(sess *supervisor.Session, err error) {
			at := time.Now().Add(-time.Minute)
			leaveOut(sess, at, at.Add(time.Millisecond), err)
		},
		"stopped in": func(sess *supervisor.Session, err error) {
			_ = leaveOutInFlight(sess, time.Now().Add(-time.Minute), err)
		},
	} {
		t.Run(name, func(t *testing.T) {
			logtest.Each(t, func(t *testing.T, on bool) {
				resetWarnLimiters()
				logger, logs := logtest.Observe()
				sess := &supervisor.Session{Mgr: syncMock.New(nil), Orphans: &orphanSpans{}, ClientConnID: "c1", Logger: logger}
				row := &strconv.NumError{Func: "ParseInt", Num: canary, Err: strconv.ErrSyntax}
				leave(sess, notRecorded("row %d of the %s response does not decode: %v", 3, "COM_QUERY", row))
				logtest.Check(t, logs, canary, on, supervisor.LeftOutWarnMsg)
			})
		})
	}
}
