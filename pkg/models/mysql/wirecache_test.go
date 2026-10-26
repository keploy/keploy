package mysql_test

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"weak"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// serveConn returns a DecodeContext that knows a server greeting for the
// connection it returns, as replay's handshake leaves it.
func serveConn(t testing.TB) (*wire.DecodeContext, net.Conn) {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() { c1.Close(); c2.Close() })
	dc := &wire.DecodeContext{LastOp: wire.NewLastOpMap(), ServerGreetings: wire.NewGreetings()}
	dc.ServerGreetings.Store(c1, &mysql.HandshakeV10Packet{CapabilityFlags: 0x81bff7ff})
	return dc, c1
}

// answer is a text or binary result set of rows rows of string columns about
// width bytes wide, some NULL, with an EOF terminator.
func answer(r *rand.Rand, binaryRows bool, rows, width int) mysql.PacketBundle {
	ncols := 1 + r.Intn(3)
	cols := make([]*mysql.ColumnDefinition41, ncols)
	for i := range cols {
		cols[i] = &mysql.ColumnDefinition41{Header: mysql.Header{PayloadLength: 30, SequenceID: byte(2 + i)}, Catalog: "def", Schema: "app", Table: "t", OrgTable: "t",
			Name: fmt.Sprintf("c%d", i), OrgName: fmt.Sprintf("c%d", i), FixedLength: 0x0c, CharacterSet: 33, ColumnLength: 255,
			Type: byte(mysql.FieldTypeVarString), Filler: []byte{0, 0}}
	}
	seq := byte(2 + ncols)
	eof := []byte{5, 0, 0, seq, 0xfe, 0, 0, 2, 0}
	seq++
	final := &mysql.GenericResponse{Type: "EOF", Data: []byte{5, 0, 0, seq + byte(rows), 0xfe, 0, 0, 0x22, 0}}
	val := func(i int) string { return fmt.Sprintf("%d-%s", i, strings.Repeat("v", width)) }
	hdr := &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 1}}
	if binaryRows {
		rs := &mysql.BinaryProtocolResultSet{ColumnCount: uint64(ncols), Columns: cols, EOFAfterColumns: eof, FinalResponse: final}
		for i := 0; i < rows; i++ {
			row := &mysql.BinaryRow{Header: mysql.Header{SequenceID: seq + byte(i)}, RowNullBuffer: make([]byte, (ncols+7+2)/8)}
			for j, c := range cols {
				if r.Intn(5) == 0 {
					row.RowNullBuffer[(j+2)/8] |= 1 << ((j + 2) % 8)
					row.Values = append(row.Values, mysql.ColumnEntry{Type: mysql.FieldTypeVarString, Name: c.Name})
					continue
				}
				row.Values = append(row.Values, mysql.ColumnEntry{Type: mysql.FieldTypeVarString, Name: c.Name, Value: val(i)})
			}
			rs.Rows = append(rs.Rows, row)
		}
		hdr.Type = "BinaryProtocolResultSet"
		return mysql.PacketBundle{Header: hdr, Message: rs}
	}
	rs := &mysql.TextResultSet{ColumnCount: uint64(ncols), Columns: cols, EOFAfterColumns: eof, FinalResponse: final}
	for i := 0; i < rows; i++ {
		row := &mysql.TextRow{Header: mysql.Header{SequenceID: seq + byte(i)}}
		for _, c := range cols {
			if r.Intn(5) == 0 {
				row.Values = append(row.Values, mysql.ColumnEntry{Type: mysql.FieldTypeVarString, Name: c.Name})
				continue
			}
			row.Values = append(row.Values, mysql.ColumnEntry{Type: mysql.FieldTypeVarString, Name: c.Name, Value: val(i)})
		}
		rs.Rows = append(rs.Rows, row)
	}
	hdr.Type = "TextResultSet"
	return mysql.PacketBundle{Header: hdr, Message: rs}
}

// cacheOf is the encode cache of a result set.
func cacheOf(msg interface{}) *mysql.EncodedPayload {
	switch rs := msg.(type) {
	case *mysql.TextResultSet:
		return rs.WireCache()
	case *mysql.BinaryProtocolResultSet:
		return rs.WireCache()
	}
	panic(fmt.Sprintf("%T is not a result set", msg))
}

// mainsBytes is what main serves for each answer: the encode of a new result
// set with its recorded fields. It leaves the cache empty, its budget as it
// was.
func mainsBytes(t *testing.T, answers []mysql.PacketBundle) [][]byte {
	t.Helper()
	dc, conn := serveConn(t)
	out := make([][]byte, len(answers))
	for i, a := range answers {
		var fresh interface{}
		switch rs := a.Message.(type) {
		case *mysql.TextResultSet:
			fresh = &mysql.TextResultSet{ColumnCount: rs.ColumnCount, Columns: rs.Columns, EOFAfterColumns: rs.EOFAfterColumns, Rows: rs.Rows, FinalResponse: rs.FinalResponse}
		case *mysql.BinaryProtocolResultSet:
			fresh = &mysql.BinaryProtocolResultSet{ColumnCount: rs.ColumnCount, Columns: rs.Columns, EOFAfterColumns: rs.EOFAfterColumns, Rows: rs.Rows, FinalResponse: rs.FinalResponse}
		}
		b, err := wire.EncodeToBinary(context.Background(), zap.NewNop(), &mysql.PacketBundle{Header: a.Header, Message: fresh}, conn, dc)
		if err != nil {
			t.Fatalf("answer %d: %v", i, err)
		}
		out[i] = b
	}
	_, budget := mysql.WireCacheUsage()
	mysql.ResetWireCache(budget)
	return out
}

// kept is what the budget should count for the payloads answers' caches hold.
func kept(answers []mysql.PacketBundle) int64 {
	var n int64
	for _, a := range answers {
		if b := cacheOf(a.Message).Load(); b != nil {
			n += int64(cap(b)) + mysql.WireEntryCost
		}
	}
	return n
}

// serve serves answer i and fails unless it gets main's bytes; it returns how
// many result sets the serve encoded.
func serve(t *testing.T, dc *wire.DecodeContext, conn net.Conn, a *mysql.PacketBundle, want []byte, what string) uint64 {
	t.Helper()
	before := wire.ResultSetEncodes()
	got, err := wire.EncodeToBinary(context.Background(), zap.NewNop(), a, conn, dc)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s: served\n% x\n(%v), main serves\n% x", what, got, err, want)
	}
	return wire.ResultSetEncodes() - before
}

// Answers whose payloads add up to seven times the budget, each served twice
// and then all once more: the bytes counted never pass the budget and are
// exactly the payloads the caches hold, eviction leaves the cache full to
// within one payload, the newest payload stays cached for its next serve, an
// evicted answer is encoded again at its next serve, and every serve, before
// and after eviction, gives main's bytes.
func TestWireCacheHoldsItsBudgetAndServesMainsBytesPastIt(t *testing.T) {
	r := rand.New(rand.NewSource(5))
	var answers []mysql.PacketBundle
	for i := 0; i < 200; i++ {
		answers = append(answers, answer(r, i%2 == 1, 1+r.Intn(12), 8+r.Intn(200)))
	}
	want := mainsBytes(t, answers)
	var total int
	for _, w := range want {
		total += len(w)
	}
	const budget = 24 << 10
	if total < 7*budget {
		t.Fatalf("the answers encode to %d bytes, under seven budgets of %d: the row would evict too little", total, budget)
	}
	defer mysql.ResetWireCache(budget)()
	dc, conn := serveConn(t)
	check := func(what string) {
		t.Helper()
		held, b := mysql.WireCacheUsage()
		if b != budget || held > budget || held != kept(answers) {
			t.Fatalf("%s: the cache counts %d bytes of a %d budget (want %d), the caches hold %d", what, held, b, budget, kept(answers))
		}
	}
	var largest, served int64
	for i := range answers {
		what := fmt.Sprintf("answer %d (%T), first pass", i, answers[i].Message)
		if n := serve(t, dc, conn, &answers[i], want[i], what) + serve(t, dc, conn, &answers[i], want[i], what); n != 1 {
			t.Fatalf("%s: two serves encoded it %d times, want once: the newest payload must stay cached", what, n)
		}
		cost := int64(cap(cacheOf(answers[i].Message).Load())) + mysql.WireEntryCost
		largest, served = max(largest, cost), served+cost
		check(what)
		if held, _ := mysql.WireCacheUsage(); served > budget && held <= budget-largest {
			t.Fatalf("%s: the cache counts %d bytes of %d: eviction emptied more than the new payload needed (largest %d)", what, held, budget, largest)
		}
	}
	evicted := 0
	for i := range answers {
		what := fmt.Sprintf("answer %d (%T), second pass", i, answers[i].Message)
		cached := cacheOf(answers[i].Message).Load() != nil
		n := serve(t, dc, conn, &answers[i], want[i], what)
		if cached && n != 0 || !cached && n != 1 {
			t.Fatalf("%s: cached %v before the serve, which encoded it %d times", what, cached, n)
		}
		if !cached {
			evicted++
		}
		check(what)
	}
	if evicted < len(answers)/2 {
		t.Fatalf("only %d of %d answers were evicted before their next serve: the row tested too little eviction", evicted, len(answers))
	}
}

// An answer larger than the whole budget is served with main's bytes and
// encoded at every serve, and caching it is never tried at the cost of the
// payloads already cached.
func TestWireCacheDoesNotCacheAnAnswerLargerThanItsBudget(t *testing.T) {
	r := rand.New(rand.NewSource(9))
	answers := []mysql.PacketBundle{answer(r, false, 2, 20), answer(r, true, 2, 20), answer(r, false, 40, 400)}
	want := mainsBytes(t, answers)
	budget := int64(len(want[0])+len(want[1]))*2 + 4*mysql.WireEntryCost
	if int64(len(want[2])) <= budget {
		t.Fatalf("the large answer encodes to %d bytes, within the %d budget", len(want[2]), budget)
	}
	defer mysql.ResetWireCache(budget)()
	dc, conn := serveConn(t)
	serve(t, dc, conn, &answers[0], want[0], "small text answer")
	serve(t, dc, conn, &answers[1], want[1], "small binary answer")
	held0, _ := mysql.WireCacheUsage()
	for k := 0; k < 3; k++ {
		if n := serve(t, dc, conn, &answers[2], want[2], fmt.Sprintf("large answer, serve %d", k)); n != 1 {
			t.Fatalf("serve %d of the large answer encoded %d result sets, want 1", k, n)
		}
	}
	if held, _ := mysql.WireCacheUsage(); held != held0 || cacheOf(answers[2].Message).Load() != nil ||
		cacheOf(answers[0].Message).Load() == nil || cacheOf(answers[1].Message).Load() == nil {
		t.Fatalf("after the large answer the cache counts %d bytes (was %d); the large answer is cached %v, the small ones %v %v",
			held, held0, cacheOf(answers[2].Message).Load() != nil, cacheOf(answers[0].Message).Load() != nil, cacheOf(answers[1].Message).Load() != nil)
	}
}

//go:noinline
func serveAndDrop(t *testing.T, dc *wire.DecodeContext, conn net.Conn) weak.Pointer[mysql.TextResultSet] {
	a := answer(rand.New(rand.NewSource(13)), false, 4, 300)
	want := mainsBytes(t, []mysql.PacketBundle{a})
	serve(t, dc, conn, &a, want[0], "dropped answer")
	if cacheOf(a.Message).Load() == nil {
		t.Fatal("the dropped answer was not cached")
	}
	return weak.Make(a.Message.(*mysql.TextResultSet))
}

// The budget keeps no mock alive: a served result set nothing else holds is
// collected with its payload. Its bytes stay counted, as an upper bound, until
// eviction reaches them, and then leave the count.
func TestWireCacheKeepsNoCollectedAnswerAlive(t *testing.T) {
	r := rand.New(rand.NewSource(17))
	var answers []mysql.PacketBundle
	for i := 0; i < 40; i++ {
		answers = append(answers, answer(r, i%2 == 0, 4, 300))
	}
	want := mainsBytes(t, answers)
	const budget = 16 << 10
	defer mysql.ResetWireCache(budget)()
	dc, conn := serveConn(t)
	gone := serveAndDrop(t, dc, conn)
	held0, _ := mysql.WireCacheUsage()
	for i := 0; i < 20 && gone.Value() != nil; i++ {
		runtime.GC()
	}
	if gone.Value() != nil {
		t.Fatal("a served answer nothing else holds was not collected after 20 collections: the cache keeps it alive")
	}
	if held, _ := mysql.WireCacheUsage(); held != held0 || held == 0 {
		t.Fatalf("after the collection the cache counts %d bytes, want the %d it counted", held, held0)
	}
	for i := range answers {
		serve(t, dc, conn, &answers[i], want[i], fmt.Sprintf("answer %d", i))
	}
	if held, _ := mysql.WireCacheUsage(); held > budget || held != kept(answers) {
		t.Fatalf("once later answers filled the cache it counts %d bytes of %d, want exactly the %d the live caches hold", held, budget, kept(answers))
	}
}

// Connections serving many answers at once, past the budget, all get main's
// bytes, and the count still matches what the caches hold.
func TestWireCacheConcurrentServesPastItsBudget(t *testing.T) {
	r := rand.New(rand.NewSource(21))
	var answers []mysql.PacketBundle
	for i := 0; i < 48; i++ {
		answers = append(answers, answer(r, i%2 == 0, 1+r.Intn(6), 8+r.Intn(120)))
	}
	want := mainsBytes(t, answers)
	const budget = 6 << 10
	defer mysql.ResetWireCache(budget)()
	dc, conn := serveConn(t)
	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 3*len(answers); k++ {
				i := (k*(g+1) + g) % len(answers)
				got, err := wire.EncodeToBinary(context.Background(), zap.NewNop(), &answers[i], conn, dc)
				if err != nil || !bytes.Equal(got, want[i]) {
					errs <- fmt.Sprintf("goroutine %d serve %d of answer %d: % x (%v), main serves % x", g, k, i, got, err, want[i])
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	if held, _ := mysql.WireCacheUsage(); held > budget || held != kept(answers) {
		t.Fatalf("the cache counts %d bytes of %d, the caches hold %d", held, budget, kept(answers))
	}
}
