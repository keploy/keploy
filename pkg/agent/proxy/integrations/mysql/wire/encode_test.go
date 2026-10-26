package wire

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	yamlLib "gopkg.in/yaml.v3"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.uber.org/zap"
)

// encodeConn returns a DecodeContext that knows a server greeting for the
// connection it returns, as replay's handshake leaves it.
func encodeConn(t testing.TB) (*DecodeContext, net.Conn) {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() { c1.Close(); c2.Close() })
	dc := &DecodeContext{LastOp: NewLastOpMap(), ServerGreetings: NewGreetings()}
	dc.ServerGreetings.Store(c1, &mysql.HandshakeV10Packet{CapabilityFlags: 0x81bff7ff})
	return dc, c1
}

// uncached returns a result set with msg's recorded fields and an empty cache,
// so EncodeToBinary encodes it from its decoded rows as main does on every
// serve. Other messages are returned as they are (they are never cached).
func uncached(msg interface{}) interface{} {
	switch rs := msg.(type) {
	case *mysql.TextResultSet:
		return &mysql.TextResultSet{ColumnCount: rs.ColumnCount, Columns: rs.Columns, EOFAfterColumns: rs.EOFAfterColumns, Rows: rs.Rows, FinalResponse: rs.FinalResponse}
	case *mysql.BinaryProtocolResultSet:
		return &mysql.BinaryProtocolResultSet{ColumnCount: rs.ColumnCount, Columns: rs.Columns, EOFAfterColumns: rs.EOFAfterColumns, Rows: rs.Rows, FinalResponse: rs.FinalResponse}
	}
	return msg
}

func isResultSet(msg interface{}) bool {
	switch msg.(type) {
	case *mysql.TextResultSet, *mysql.BinaryProtocolResultSet:
		return true
	}
	return false
}

// recordedAnswers decodes the MySQL answers of the recorded mock files in
// mockdb's testdata.
func recordedAnswers(t *testing.T) []mysql.PacketBundle {
	t.Helper()
	dir := filepath.Join("..", "..", "..", "..", "..", "platform", "yaml", "mockdb", "testdata")
	return decodeAnswers(t, filepath.Join(dir, "mysql_capture_starved_mocks.yaml"), filepath.Join(dir, "no_connfailure_golden", "recorded.yaml"))
}

// decodeAnswers decodes the MySQL answers of the mock files at paths, in
// order. Documents of another kind (a test case, another parser's mock) are
// skipped.
func decodeAnswers(t *testing.T, paths ...string) []mysql.PacketBundle {
	t.Helper()
	var out []mysql.PacketBundle
	for _, name := range paths {
		f, err := os.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		dec := yamlLib.NewDecoder(f)
		var docs []*yaml.NetworkTrafficDoc
		for {
			var d yaml.NetworkTrafficDoc
			if err := dec.Decode(&d); err != nil {
				if !errors.Is(err, io.EOF) {
					t.Fatalf("%s: %v", name, err)
				}
				break
			}
			if d.Kind == models.MySQL {
				docs = append(docs, &d)
			}
		}
		f.Close()
		mocks, err := mockdb.DecodeMocks(docs, zap.NewNop())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, m := range mocks {
			for _, r := range m.Spec.MySQLResponses {
				out = append(out, r.PacketBundle)
			}
		}
	}
	return out
}

// pktHeader is a packet header with the given length and sequence id.
func pktHeader(n int, seq byte) mysql.Header {
	return mysql.Header{PayloadLength: uint32(n), SequenceID: seq}
}

// randomAnswer builds a text or binary result set of rows rows: strings, NULLs,
// integers and doubles, with either an EOF or an OK-replacing-EOF terminator,
// or none.
func randomAnswer(r *rand.Rand, binaryRows bool, rows int) mysql.PacketBundle {
	ncols := 1 + r.Intn(5)
	types := []mysql.FieldType{mysql.FieldTypeVarString, mysql.FieldTypeLongLong, mysql.FieldTypeLong, mysql.FieldTypeDouble}
	cols := make([]*mysql.ColumnDefinition41, ncols)
	for i := range cols {
		cols[i] = &mysql.ColumnDefinition41{Header: pktHeader(30, byte(2+i)), Catalog: "def", Schema: "app", Table: "t", OrgTable: "t",
			Name: fmt.Sprintf("c%d", i), OrgName: fmt.Sprintf("c%d", i), FixedLength: 0x0c, CharacterSet: 33, ColumnLength: 255,
			Type: byte(types[r.Intn(len(types))]), Filler: []byte{0, 0}}
	}
	seq := byte(2 + ncols)
	var eof []byte
	if binaryRows || r.Intn(2) == 0 {
		eof = []byte{5, 0, 0, seq, 0xfe, 0, 0, 2, 0}
		seq++
	}
	val := func(c *mysql.ColumnDefinition41, i int) interface{} {
		switch mysql.FieldType(c.Type) {
		case mysql.FieldTypeLongLong:
			return int64(r.Intn(1<<30) - i)
		case mysql.FieldTypeLong:
			return int32(r.Intn(1 << 20))
		case mysql.FieldTypeDouble:
			return r.Float64()
		}
		return fmt.Sprintf("value-%d-%d", i, r.Intn(1<<20))
	}
	var final *mysql.GenericResponse
	switch r.Intn(3) {
	case 0:
		final = &mysql.GenericResponse{Type: "EOF", Data: []byte{5, 0, 0, byte(seq + byte(rows)), 0xfe, 0, 0, 0x22, 0}}
	case 1:
		final = &mysql.GenericResponse{Type: "OK", Data: []byte{7, 0, 0, byte(seq + byte(rows)), 0xfe, 0, 0, 2, 0, 0, 0}}
	}
	hdr := &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 1}}
	if binaryRows {
		rs := &mysql.BinaryProtocolResultSet{ColumnCount: uint64(ncols), Columns: cols, EOFAfterColumns: eof, FinalResponse: final}
		for i := 0; i < rows; i++ {
			row := &mysql.BinaryRow{Header: pktHeader(0, seq+byte(i)), RowNullBuffer: make([]byte, (ncols+7+2)/8)}
			for j, c := range cols {
				if r.Intn(6) == 0 {
					row.RowNullBuffer[(j+2)/8] |= 1 << ((j + 2) % 8)
					row.Values = append(row.Values, mysql.ColumnEntry{Type: mysql.FieldType(c.Type), Name: c.Name})
					continue
				}
				row.Values = append(row.Values, mysql.ColumnEntry{Type: mysql.FieldType(c.Type), Name: c.Name, Value: val(c, i)})
			}
			rs.Rows = append(rs.Rows, row)
		}
		hdr.Type = "BinaryProtocolResultSet"
		return mysql.PacketBundle{Header: hdr, Message: rs}
	}
	rs := &mysql.TextResultSet{ColumnCount: uint64(ncols), Columns: cols, EOFAfterColumns: eof, FinalResponse: final}
	for i := 0; i < rows; i++ {
		row := &mysql.TextRow{Header: pktHeader(0, seq+byte(i))}
		for _, c := range cols {
			if r.Intn(6) == 0 {
				row.Values = append(row.Values, mysql.ColumnEntry{Type: mysql.FieldType(c.Type), Name: c.Name})
				continue
			}
			row.Values = append(row.Values, mysql.ColumnEntry{Type: mysql.FieldType(c.Type), Name: c.Name, Value: fmt.Sprint(val(c, i))})
		}
		rs.Rows = append(rs.Rows, row)
	}
	hdr.Type = "TextResultSet"
	return mysql.PacketBundle{Header: hdr, Message: rs}
}

// Every recorded answer, and random text and binary result sets up to 300
// rows, is served byte for byte as main serves it: the first serve encodes
// the result set and caches its payload, every later serve copies that payload
// after a fresh header. A result set is encoded once however often it is
// served, and the bytes one serve returns are its own: writing to them leaves
// the next serve's bytes as they were.
func TestEncodeToBinaryServesEachAnswerAsMainDoesAndEncodesItOnce(t *testing.T) {
	ctx, logger := context.Background(), zap.NewNop()
	answers := recordedAnswers(t)
	recordedSets := 0
	for _, a := range answers {
		if isResultSet(a.Message) {
			recordedSets++
		}
	}
	if recordedSets < 2 || len(answers) < 8 {
		t.Fatalf("the recorded fixtures gave %d answers, %d of them result sets: the row would test nothing", len(answers), recordedSets)
	}
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 400; i++ {
		rows := []int{0, 1, 3, 17}[i%4]
		if i%50 == 0 {
			rows = 300
		}
		answers = append(answers, randomAnswer(r, i%2 == 1, rows))
	}
	dc, conn := encodeConn(t)
	cached, erred := 0, 0
	for i, a := range answers {
		old := mysql.PacketBundle{Header: a.Header, Message: uncached(a.Message)}
		want, wantErr := EncodeToBinary(ctx, logger, &old, conn, dc)
		before := resultSetEncodes.Load()
		first, err1 := EncodeToBinary(ctx, logger, &a, conn, dc)
		second, err2 := EncodeToBinary(ctx, logger, &a, conn, dc)
		encodes := resultSetEncodes.Load() - before
		if (wantErr != nil) != (err1 != nil) || (wantErr != nil) != (err2 != nil) {
			t.Fatalf("answer %d (%T): main's path erred %v, the cached path %v then %v", i, a.Message, wantErr, err1, err2)
		}
		if wantErr != nil {
			erred++
			continue
		}
		if !bytes.Equal(first, want) || !bytes.Equal(second, want) {
			t.Fatalf("answer %d (%T): served\n% x\nthen\n% x\nmain serves\n% x", i, a.Message, first, second, want)
		}
		for j := range first {
			first[j] ^= 0xff
		}
		third, err := EncodeToBinary(ctx, logger, &a, conn, dc)
		if err != nil || !bytes.Equal(third, want) {
			t.Fatalf("answer %d (%T): after the first serve's bytes were overwritten, the next serve gave % x (%v), want % x", i, a.Message, third, err, want)
		}
		if isResultSet(a.Message) {
			cached++
			if encodes != 1 {
				t.Fatalf("answer %d (%T): two serves encoded it %d times, want once", i, a.Message, encodes)
			}
		} else if encodes != 0 {
			t.Fatalf("answer %d (%T) is not a result set but counted %d result-set encodes", i, a.Message, encodes)
		}
	}
	t.Logf("%d answers (%d recorded result sets): %d result sets served from the cache, %d erred on both paths", len(answers), recordedSets, cached, erred)
	if cached < recordedSets+300 {
		t.Fatalf("only %d answers took the cached path", cached)
	}
}

// One recorded answer is shared by the mock and every copy replay makes of it
// (models.Mock.DeepCopy shares the message). Each serve still frames it with
// its own packet header: the cache holds the payload, never the header.
func TestEncodeToBinaryFramesASharedAnswerWithEachServesHeader(t *testing.T) {
	ctx, logger := context.Background(), zap.NewNop()
	dc, conn := encodeConn(t)
	a := randomAnswer(rand.New(rand.NewSource(3)), false, 5)
	mock := &models.Mock{Kind: models.MySQL, Spec: models.MockSpec{MySQLResponses: []mysql.Response{{PacketBundle: a}}}}
	cp := mock.DeepCopy()
	cp.Spec.MySQLResponses[0].Header = &mysql.PacketInfo{Type: a.Header.Type, Header: &mysql.Header{PayloadLength: 1, SequenceID: 9}}

	want1, err := EncodeToBinary(ctx, logger, &mysql.PacketBundle{Header: a.Header, Message: uncached(a.Message)}, conn, dc)
	if err != nil {
		t.Fatal(err)
	}
	want9 := append([]byte(nil), want1...)
	want9[3] = 9

	before := resultSetEncodes.Load()
	got1, err1 := EncodeToBinary(ctx, logger, &mock.Spec.MySQLResponses[0].PacketBundle, conn, dc)
	got9, err9 := EncodeToBinary(ctx, logger, &cp.Spec.MySQLResponses[0].PacketBundle, conn, dc)
	if err1 != nil || err9 != nil {
		t.Fatal(err1, err9)
	}
	if !bytes.Equal(got1, want1) || !bytes.Equal(got9, want9) {
		t.Fatalf("served\n% x\n% x\nwant\n% x\n% x", got1, got9, want1, want9)
	}
	if n := resultSetEncodes.Load() - before; n != 1 {
		t.Fatalf("the mock and its copy encoded the answer %d times, want once", n)
	}
}

// Connections that serve one session mock at once all get main's bytes.
func TestEncodeToBinaryConcurrentServesOfOneAnswer(t *testing.T) {
	ctx, logger := context.Background(), zap.NewNop()
	dc, conn := encodeConn(t)
	for seed := int64(0); seed < 20; seed++ {
		a := randomAnswer(rand.New(rand.NewSource(seed)), seed%2 == 0, 40)
		want, err := EncodeToBinary(ctx, logger, &mysql.PacketBundle{Header: a.Header, Message: uncached(a.Message)}, conn, dc)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		got := make([][]byte, 8)
		for g := range got {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got[g], _ = EncodeToBinary(ctx, logger, &a, conn, dc)
			}()
		}
		wg.Wait()
		for g := range got {
			if !bytes.Equal(got[g], want) {
				t.Fatalf("seed %d serve %d: % x, want % x", seed, g, got[g], want)
			}
		}
	}
}

// Serving a recorded answer again costs a copy, not an encode: a later serve
// allocates only the bytes it returns, whatever the answer's rows, and builds
// no debug line's fields while debug is off (on main a 300-row text answer
// allocates 919 times on every serve, a 1-row one 16 times).
func TestEncodeToBinaryAllocationsDoNotGrowWithTheAnswer(t *testing.T) {
	ctx, logger := context.Background(), zap.NewNop()
	dc, conn := encodeConn(t)
	for _, binaryRows := range []bool{false, true} {
		allocs := map[int]float64{}
		for _, rows := range []int{1, 300} {
			a := randomAnswer(rand.New(rand.NewSource(11)), binaryRows, rows)
			if _, err := EncodeToBinary(ctx, logger, &a, conn, dc); err != nil {
				t.Fatal(err)
			}
			allocs[rows] = testing.AllocsPerRun(50, func() {
				if _, err := EncodeToBinary(ctx, logger, &a, conn, dc); err != nil {
					t.Fatal(err)
				}
			})
		}
		if allocs[300] > allocs[1] || allocs[300] > 1 {
			t.Fatalf("binary=%v: a later serve allocates %v times for 1 row and %v for 300 rows, want at most 1 for either", binaryRows, allocs[1], allocs[300])
		}
	}
}

// wireCacheOf is the encode cache of a result set; nil for other messages.
func wireCacheOf(msg interface{}) *mysql.EncodedPayload {
	switch rs := msg.(type) {
	case *mysql.TextResultSet:
		return rs.WireCache()
	case *mysql.BinaryProtocolResultSet:
		return rs.WireCache()
	}
	return nil
}

// holdsMocks reports whether the .yaml file at p is a file of recorded
// documents (mocks or test cases) and not, say, a run's config or verdicts:
// its first document decodes as one and names its kind.
func holdsMocks(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	var d yaml.NetworkTrafficDoc
	return yamlLib.NewDecoder(f).Decode(&d) == nil && d.Kind != ""
}

// heapInUse is the heap the live objects hold, after a collection. The
// differences of two readings are only as exact as the test's other garbage
// lets them be: a few KB.
func heapInUse() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// A recording made outside the repo: KEPLOY_MYSQL_WIRE_CORPUS names a
// directory (a keploy folder), and every MySQL mock in a .yaml file under it
// is read. Each recorded answer is served twice through the cache and must
// give, both times, the bytes main's path encodes for it. The row then says
// what the cache keeps: the payload bytes cached per served result set, what
// the corpus's answers cost the cache's budget (whether a replay of it keeps
// every answer encoded), the bytes the budget counts once every answer and
// its copy were served (at most the budget), and how much the heap held by
// the mocks grew by then. The replay-latency
// lane runs it on its 100, 1k and 10k recordings; with no corpus named it is
// skipped.
func TestEncodeToBinaryOverARecordedCorpus(t *testing.T) {
	dir := os.Getenv("KEPLOY_MYSQL_WIRE_CORPUS")
	if dir == "" {
		t.Skip("KEPLOY_MYSQL_WIRE_CORPUS names no directory of recorded mocks")
	}
	var paths []string
	var onDisk int64
	skipped := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".yaml") {
			return err
		}
		if !holdsMocks(p) {
			skipped++
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		paths = append(paths, p)
		onDisk += info.Size()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, logger := context.Background(), zap.NewNop()
	dc, conn := encodeConn(t)
	heap0 := heapInUse()
	answers := decodeAnswers(t, paths...)
	heap1 := heapInUse()
	sets, erred, payload, largest := 0, 0, 0, 0
	var cost int64
	for i := range answers {
		a := &answers[i]
		old := mysql.PacketBundle{Header: a.Header, Message: uncached(a.Message)}
		want, wantErr := EncodeToBinary(ctx, logger, &old, conn, dc)
		first, err1 := EncodeToBinary(ctx, logger, a, conn, dc)
		second, err2 := EncodeToBinary(ctx, logger, a, conn, dc)
		if (wantErr != nil) != (err1 != nil) || (wantErr != nil) != (err2 != nil) {
			t.Fatalf("answer %d (%T): main's path erred %v, the cached path %v then %v", i, a.Message, wantErr, err1, err2)
		}
		if wantErr != nil {
			erred++
			continue
		}
		if !bytes.Equal(first, want) || !bytes.Equal(second, want) {
			t.Fatalf("answer %d (%T): served\n% x\nthen\n% x\nmain serves\n% x", i, a.Message, first, second, want)
		}
		if c := wireCacheOf(a.Message); c != nil {
			sets++
			n := len(c.Load())
			payload += n
			cost += int64(cap(c.Load())) + mysql.WireEntryCost
			largest = max(largest, n)
		}
	}
	heap2 := heapInUse()
	held, budget := mysql.WireCacheUsage()
	runtime.KeepAlive(answers)
	if sets == 0 {
		t.Fatalf("%s holds no recorded result set: the row tested nothing", dir)
	}
	if held > budget {
		t.Fatalf("the encode cache counts %d bytes, past its %d budget", held, budget)
	}
	t.Logf("corpus %s: %d mock files (%d bytes; %d other .yaml files skipped), %d answers, %d result sets, %d erred on both paths; "+
		"payload %d bytes, %d per served result set, largest %d; cached alone they cost %d bytes of the %d budget (fit: %v), "+
		"and with main's copies served beside them the budget counts %d at the end; "+
		"heap held by the decoded answers %d bytes, by them once all were served %d bytes (+%d)",
		dir, len(paths), onDisk, skipped, len(answers), sets, erred,
		payload, payload/sets, largest, cost, budget, cost <= budget, held,
		int64(heap1)-int64(heap0), int64(heap2)-int64(heap0), int64(heap2)-int64(heap1))
}
