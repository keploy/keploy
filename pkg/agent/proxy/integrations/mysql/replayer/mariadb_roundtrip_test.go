package replayer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/directive"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/recorder"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// capturedPacket is one packet of a captured conversation, with its header,
// and whether the server sent it.
type capturedPacket struct {
	fromServer bool
	bytes      []byte
}

// readCapture reads a conversation captured between a client and a server
// (testdata), in the order its packets were sent.
func readCapture(t *testing.T, path string) []capturedPacket {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []capturedPacket
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		dir, h, ok := strings.Cut(line, " ")
		b, err := hex.DecodeString(h)
		if !ok || (dir != "S" && dir != "C") || err != nil || len(b) < 4 {
			t.Fatalf("%s: malformed line %q", path, line)
		}
		out = append(out, capturedPacket{fromServer: dir == "S", bytes: b})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// recordCapture records conv as keploy's recorder does from the capture
// layer's chunk streams, and returns the mocks it emits.
func recordCapture(t *testing.T, conv []capturedPacket) []*models.Mock {
	t.Helper()
	clientCh := make(chan fakeconn.Chunk, len(conv))
	destCh := make(chan fakeconn.Chunk, len(conv))
	mocks := make(chan *models.Mock, len(conv)+1)
	base := time.Date(2026, 11, 3, 19, 0, 0, 0, time.UTC)
	for i, p := range conv {
		at := base.Add(time.Duration(i) * time.Millisecond)
		c := fakeconn.Chunk{ConnSeq: fakeconn.ConnSeqOf(uint64(i + 1)), Bytes: p.bytes, ReadAt: at, WrittenAt: at}
		if p.fromServer {
			c.Dir = fakeconn.FromDest
			destCh <- c
		} else {
			c.Dir = fakeconn.FromClient
			clientCh <- c
		}
	}
	close(clientCh)
	close(destCh)
	sess := &supervisor.Session{
		ClientStream: fakeconn.New(clientCh, nil, nil),
		DestStream:   fakeconn.New(destCh, nil, nil),
		Directives:   make(chan directive.Directive, 4),
		Acks:         make(chan directive.Ack, 4),
		Mocks:        mocks,
		Logger:       zap.NewNop(),
		Ctx:          context.Background(),
		ClientConnID: "1",
		DestConnID:   "2",
		Opts:         models.OutgoingOptions{DstCfg: &models.ConditionalDstCfg{Addr: testDBAddr, Port: 3306}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := recorder.RecordV2(ctx, zap.NewNop(), sess); err != nil && err != io.EOF {
		t.Logf("RecordV2 ended with %v", err)
	}
	close(mocks)
	var got []*models.Mock
	for m := range mocks {
		got = append(got, m)
	}
	return got
}

// readWholePacket reads one packet, with its header, from the replayer.
func (c mysqlClient) readWholePacket() []byte {
	c.t.Helper()
	if err := c.c.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		c.t.Fatal(err)
	}
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c.c, hdr); err != nil {
		c.t.Fatalf("the client waited for a packet and got %v", err)
	}
	pkt := make([]byte, 4+(int(hdr[0])|int(hdr[1])<<8|int(hdr[2])<<16))
	copy(pkt, hdr)
	if _, err := io.ReadFull(c.c, pkt[4:]); err != nil {
		c.t.Fatalf("the client read a packet header and then %v", err)
	}
	return pkt
}

// A mysqlclient application on MariaDB is recorded and then replayed: every
// packet replay sends is the one the server sent. MariaDB Connector/C answers
// the greeting with the extended capabilities it offers, so a greeting
// replayed without them is answered with none and no recorded
// HandshakeResponse41 matches the client's; and both sides frame result-set
// metadata by those capabilities, so a column definition read with MySQL's
// layout is misread (the JSON column's fails to decode, and its exchange is
// not recorded).
func TestAMariaDBConnectorCConnectionIsRecordedAndReplayedAsTheServerSentIt(t *testing.T) {
	conv := readCapture(t, "testdata/mariadb-10.6-connector-c.txt")
	recorded := recordCapture(t, conv)

	// The recorder keeps the JSON column's result set, and reads its
	// columns where MariaDB put them.
	var rs *mysql.TextResultSet
	for _, m := range recorded {
		if len(m.Spec.MySQLRequests) == 1 && len(m.Spec.MySQLResponses) == 1 {
			if q, ok := m.Spec.MySQLRequests[0].Message.(*mysql.QueryPacket); ok && strings.HasPrefix(q.Query, "SELECT id, name, data, note") {
				rs, _ = m.Spec.MySQLResponses[0].Message.(*mysql.TextResultSet)
			}
		}
	}
	// Each check below goes on past a failure, so a row that fails says
	// what both record and replay got wrong.
	if rs == nil {
		t.Errorf("the SELECT of the JSON column was not recorded (%d mocks recorded)", len(recorded))
		rs = &mysql.TextResultSet{}
	}
	want := []struct {
		name   string
		typ    byte
		length uint32
	}{{"id", 0x03, 11}, {"name", 0xfd, 560}, {"data", 0xfc, 0xffffffff}, {"note", 0xfc, 0xffffffff}}
	if len(rs.Columns) != len(want) {
		t.Errorf("recorded %d columns, want %d", len(rs.Columns), len(want))
	}
	for i, w := range want[:min(len(want), len(rs.Columns))] {
		c := rs.Columns[i]
		if c.Name != w.name || c.Type != w.typ || c.ColumnLength != w.length || c.CharacterSet == 0 {
			t.Errorf("column %d recorded as %q type 0x%02x length %d charset %d, want %q type 0x%02x length %d",
				i, c.Name, c.Type, c.ColumnLength, c.CharacterSet, w.name, w.typ, w.length)
		}
	}

	// Replay serves the recording, as written to its mocks.yaml and read
	// back, to the same client: each answer is the server's, byte for byte.
	var buf bytes.Buffer
	enc := yamlLib.NewEncoder(&buf)
	for _, m := range recorded {
		doc, err := mockdb.EncodeMock(m, zap.NewNop())
		if err != nil {
			t.Fatal(err)
		}
		if err := enc.Encode(doc); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	var docs []*yaml.NetworkTrafficDoc
	for dec := yamlLib.NewDecoder(&buf); ; {
		doc := &yaml.NetworkTrafficDoc{}
		if err := dec.Decode(doc); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, doc)
	}
	read, err := mockdb.DecodeMocks(docs, zap.NewNop())
	if err != nil || len(read) != len(recorded) {
		t.Fatalf("read back %d of the %d mocks recorded: %v", len(read), len(recorded), err)
	}
	db := &fakeMockDb{}
	for _, m := range read {
		m.DeriveLifetime()
		if m.TestModeInfo.Lifetime == models.LifetimePerTest {
			db.perTest = append(db.perTest, m)
		} else {
			db.session = append(db.session, m)
		}
	}
	client, done := replay(t, db)
	for i, p := range conv {
		if p.fromServer {
			if got := client.readWholePacket(); !bytes.Equal(got, p.bytes) {
				t.Fatalf("packet %d: replay sent\n% x\nthe server sent\n% x", i, got, p.bytes)
			}
			continue
		}
		if err := client.c.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := client.c.Write(p.bytes); err != nil {
			t.Fatalf("packet %d: the client's write: %v", i, err)
		}
	}
	_ = client.c.Close()
	if err := <-done; err != nil && strings.Contains(err.Error(), "no mysql mocks matched") {
		t.Fatalf("Replay missed: %v", err)
	}
}
