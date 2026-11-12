package replayer

import (
	"bytes"
	"io"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// readResultSet reads one text result set from the replayer: the column
// count, that many column definitions, then rows up to the terminator (an EOF
// or an OK-replacing-EOF; the client negotiated no deprecate-EOF, so a column
// EOF comes first). It returns the raw bytes.
func (c mysqlClient) readResultSet() []byte {
	c.t.Helper()
	var raw bytes.Buffer
	readPkt := func() []byte {
		var hdr [4]byte
		if _, err := io.ReadFull(c.c, hdr[:]); err != nil {
			c.t.Fatalf("the client waited for a result-set packet and got %v", err)
		}
		p := make([]byte, int(hdr[0])|int(hdr[1])<<8|int(hdr[2])<<16)
		if _, err := io.ReadFull(c.c, p); err != nil {
			c.t.Fatalf("the client read a short result-set packet: %v", err)
		}
		raw.Write(hdr[:])
		raw.Write(p)
		return p
	}
	cols := readPkt()
	for i := 0; i < int(cols[0]); i++ {
		readPkt()
	}
	if p := readPkt(); p[0] != 0xfe {
		c.t.Fatalf("the client read % x after the columns, want their EOF", p)
	}
	for {
		if p := readPkt(); p[0] == 0xfe && len(p) < 9 {
			return raw.Bytes()
		}
	}
}

// A session mock the app's queries hit again and again is encoded once: its
// first serve encodes the recorded result set, every later one sends the
// cached bytes after a fresh header, byte for byte the first serve's bytes.
func TestASessionAnswerServedAgainIsEncodedOnce(t *testing.T) {
	rs := &mysql.TextResultSet{
		ColumnCount: 1,
		Columns: []*mysql.ColumnDefinition41{{Header: mysql.Header{PayloadLength: 22, SequenceID: 2}, Catalog: "def", Name: "v", OrgName: "v",
			FixedLength: 0x0c, CharacterSet: 33, ColumnLength: 255, Type: byte(mysql.FieldTypeVarString), Filler: []byte{0, 0}}},
		EOFAfterColumns: []byte{5, 0, 0, 3, 0xfe, 0, 0, 2, 0},
		Rows:            []*mysql.TextRow{{Header: mysql.Header{SequenceID: 4}, Values: []mysql.ColumnEntry{{Type: mysql.FieldTypeVarString, Name: "v", Value: "8.0.36"}}}},
		FinalResponse:   &mysql.GenericResponse{Type: "EOF", Data: []byte{5, 0, 0, 5, 0xfe, 0, 0, 2, 0}},
	}
	q := &models.Mock{Version: "api.keploy.io/v1beta1", Name: "version", Kind: models.MySQL}
	q.TestModeInfo.Lifetime = models.LifetimeSession
	q.Spec.Metadata = map[string]string{"type": "config"}
	q.Spec.MySQLRequests = []mysql.Request{comQueryReq("SELECT @@version")}
	q.Spec.MySQLResponses = []mysql.Response{{PacketBundle: mysql.PacketBundle{Header: &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 1}, Type: "TextResultSet"}, Message: rs}}}

	client, _ := replay(t, &fakeMockDb{session: []*models.Mock{nativeHandshake(), q}})
	client.greetAndAnswer("app", mysql.Native)
	if ok := client.read(); len(ok) == 0 || ok[0] != mysql.OK {
		t.Fatalf("the client read %x after its handshake; want the recorded OK", ok)
	}
	before := wire.ResultSetEncodes()
	var first []byte
	for i := 0; i < 5; i++ {
		client.send(0, append([]byte{mysql.COM_QUERY}, "SELECT @@version"...))
		got := client.readResultSet()
		if i == 0 {
			first = got
			if !bytes.Contains(got, []byte("8.0.36")) {
				t.Fatalf("the first serve sent % x, want the recorded row", got)
			}
		} else if !bytes.Equal(got, first) {
			t.Fatalf("serve %d sent\n% x\nthe first sent\n% x", i, got, first)
		}
	}
	if n := wire.ResultSetEncodes() - before; n != 1 {
		t.Fatalf("five serves of one recorded answer encoded it %d times, want once", n)
	}
}
