package proxy

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgproto3"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	testgrpc "google.golang.org/grpc/interop/grpc_testing"
)

// Every registered parser's recorder stamps each mock it emits with its
// request time (Spec.ReqTimestampMock). A test's mock without one cannot be
// placed in any test case's window, so its loss leaves out every later test
// case of its recording, and the --sync or static-dedup buffer's stale cutoff
// drops it at the next resolve (syncMock's warnNoRequestTime, which says one
// at WARN naming its parser). The row runs each parser registered in this
// build, OSS keploy's and any a build adds (the integrations parsers that
// pkg/agent/proxy/extraparsers.go registers), through the recorder as
// handleConnection dispatches it, over one exchange of its protocol between a
// real or scripted client and server, and over each of its other emit paths
// listed here (stampPaths: HTTP's call that got no answer, recorded with its
// fault), under either switch (the package's TestMain clears KEPLOY_PROVEN,
// so each run sets it). A registered parser with no exchange here fails the
// row, so a new parser cannot go unchecked.
func TestEveryRegisteredParsersRecorderStampsEachMocksRequestTime(t *testing.T) {
	kinds := make([]integrations.IntegrationType, 0, len(integrations.Registered))
	for k := range integrations.Registered {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	t.Logf("registered parsers: %v", kinds)
	for _, kind := range kinds {
		t.Run(string(kind), func(t *testing.T) {
			ex, ok := stampExchanges[kind]
			if !ok {
				t.Fatalf("the %s parser is registered with no exchange in stampExchanges: add one, so this row "+
					"checks that its recorder stamps each mock's request time", kind)
			}
			rec := kind
			if ex.recordedBy != "" {
				above, ok := integrations.Registered[ex.recordedBy]
				if !ok || above.Priority <= integrations.Registered[kind].Priority {
					t.Fatalf("the row records the %s exchange through %s, as the parser registered above %s that "+
						"claims its connections, but %s is not registered above it: check %s's own recorder", kind,
						ex.recordedBy, kind, ex.recordedBy, kind)
				}
				rec = ex.recordedBy
			}
			paths := stampPaths[kind]
			names := make([]string, 0, len(paths))
			for name := range paths {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, on := range []string{"0", "1"} {
				t.Run(provenswitch.Env+"="+on, func(t *testing.T) {
					t.Setenv(provenswitch.Env, on)
					checkStamped(t, kind, rec, ex)
					for _, name := range names {
						t.Run(name, func(t *testing.T) { checkStamped(t, kind, rec, paths[name]) })
					}
				})
			}
		})
	}
}

// checkStamped records ex through rec, the parser that records kind's
// connections, and fails unless it emitted a mock, every mock it emitted
// carries its request time (and its fault, for an exchange of a fault path),
// and the manager said no WARN of a mock with none.
func checkStamped(t *testing.T, kind, rec integrations.IntegrationType, ex stampExchange) {
	t.Helper()
	mocks, warned := recordStampExchange(t, rec, ex)
	if len(mocks) == 0 {
		t.Fatalf("the %s parser's recorder emitted no mock for its exchange, so the row checked nothing", kind)
	}
	for _, mk := range mocks {
		if mk.Spec.ReqTimestampMock.IsZero() {
			t.Errorf("the %s parser's recorder emitted %s mock %q (lifetime %v) with no request time",
				kind, mk.Kind, mk.Name, mk.TestModeInfo.Lifetime)
		}
		if ex.faulted && mk.Spec.Fault == nil {
			t.Errorf("the %s parser's recorder emitted %s mock %q with no fault for an exchange of its fault path, "+
				"so the row did not check that path", kind, mk.Kind, mk.Name)
		}
	}
	for _, e := range warned {
		if strings.Contains(e.Message, "no request time") {
			t.Errorf("the manager said for the %s parser: %s (%v)", kind, e.Message, e.ContextMap())
		}
	}
}

// stampExchange is one exchange of a parser's protocol. serve answers every
// connection ln accepts, until ln closes (the row closes it). call makes the
// client's calls to addr, the proxy's listener, and returns once its
// connection is closed.
type stampExchange struct {
	serve func(t *testing.T, ln net.Listener)
	call  func(ctx context.Context, addr string) error
	// recordedBy, when set, is the parser handleConnection hands this
	// exchange's connection to instead: one registered above this one that
	// claims its connections, so this one never records (it only replays).
	// The row checks that it is registered above.
	recordedBy integrations.IntegrationType
	// faulted is set on an exchange of a recorder's fault path: each mock
	// it emits must record its fault, or the row did not reach that path.
	faulted bool
}

var stampExchanges = map[integrations.IntegrationType]stampExchange{
	integrations.HTTP:        {serve: serveHTTP(false), call: callHTTP(false)},
	integrations.HTTP2:       {serve: serveHTTP(true), call: callHTTP(true)},
	integrations.GENERIC:     {serve: serveConns(serveGenericLines), call: callGeneric},
	integrations.MYSQL:       {serve: serveConns(serveMySQLOKs), call: callMySQL},
	integrations.POSTGRES_V3: {serve: serveConns(servePostgresSelect), call: callPostgres},
	integrations.MONGO_V2:    {serve: serveConns(serveMongoReplies), call: callMongo},
	// http2 registers above grpc and claims every HTTP/2 connection, gRPC's
	// too: grpc only replays.
	integrations.GRPC: {serve: serveGRPC, call: callGRPC, recordedBy: integrations.HTTP2},
}

// stampPaths are exchanges over a recorder's other emit paths, by parser and
// name. The runtime WARN (syncMock's warnNoRequestTime) names a recorder whose
// path not listed here hands over a test's mock with no request time.
var stampPaths = map[integrations.IntegrationType]map[string]stampExchange{
	// A call the upstream closed its connection on with no answer, recorded
	// with that fault (recordV2's noAnswerFault, recordFault).
	integrations.HTTP: {"no-answer": {serve: serveConns(serveHTTPNoAnswer), call: callHTTPNoAnswer, faulted: true}},
}

// recordStampExchange records ex through kind's registered parser: one
// client connection, accepted on the proxy's listener, recorded on its way to
// ex's server, its mocks routed through a syncMock manager as production
// routes them. It returns every mock the recorder emitted and the manager's
// lines at WARN or above.
func recordStampExchange(t *testing.T, kind integrations.IntegrationType, ex stampExchange) ([]*models.Mock, []observer.LoggedEntry) {
	t.Helper()
	parser := integrations.Registered[kind].Initializer(zap.NewNop())
	srv, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ex.serve(t, srv)
	px, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = px.Close() })

	p := &Proxy{
		logger:                     zap.NewNop(),
		Integrations:               map[integrations.IntegrationType]integrations.Integrations{kind: parser},
		recordBufferCap:            8 << 20,
		recordBufferQueueSize:      64,
		recordBufferStallGrace:     10 * time.Second,
		recordBufferHalfCloseGrace: 10 * time.Second,
	}
	core, logs := observer.New(zapcore.WarnLevel)
	mgr := syncMock.New(zap.New(core))
	out := make(chan *models.Mock, 256)
	mgr.SetOutputChannel(out)
	ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), mgr), 60*time.Second)
	t.Cleanup(cancel)

	// Mocks a recorder sends down its session's channel rather than through
	// the manager are checked too.
	raw := make(chan *models.Mock, 256)
	recorded := make(chan error, 1)
	go func() {
		src, err := px.Accept()
		if err != nil {
			recorded <- err
			return
		}
		defer func() { _ = src.Close() }()
		// As the proxy dials a destination: the relay reads it through a
		// connseq.Upstream.
		dst, err := util.DialUpstream(ctx, &net.Dialer{Timeout: 5 * time.Second}, "tcp", srv.Addr().String())
		if err != nil {
			recorded <- err
			return
		}
		defer func() { _ = dst.Close() }()
		errGrp, gctx := errgroup.WithContext(ctx)
		gctx = context.WithValue(gctx, models.ErrGroupKey, errGrp)
		gctx = context.WithValue(gctx, models.ClientConnectionIDKey, "1")
		gctx = context.WithValue(gctx, models.DestConnectionIDKey, "2")
		opts := models.OutgoingOptions{DstCfg: &models.ConditionalDstCfg{Addr: srv.Addr().String()}}
		// As handleConnection dispatches a matched parser.
		if shouldRecordViaSupervisor(parser) {
			err = p.recordViaSupervisor(gctx, src, dst, parser, kind, raw, errGrp, zap.NewNop(), 1, 2, opts)
		} else {
			err = parser.RecordOutgoing(gctx, p.buildRecordSession(src, dst, raw, errGrp, zap.NewNop(), 1, 2, opts, nil))
		}
		_ = src.Close()
		_ = dst.Close()
		if werr := errGrp.Wait(); err == nil {
			err = werr
		}
		recorded <- err
	}()

	callCtx, callCancel := context.WithTimeout(ctx, 20*time.Second)
	defer callCancel()
	if err := ex.call(callCtx, px.Addr().String()); err != nil {
		select {
		case rerr := <-recorded:
			t.Fatalf("the %s exchange's client: %v (the recorder returned %v)", kind, err, rerr)
		case <-time.After(5 * time.Second):
			t.Fatalf("the %s exchange's client: %v (the recorder still running)", kind, err)
		}
	}
	select {
	case err := <-recorded:
		t.Logf("the %s recorder returned %v", kind, err)
	case <-time.After(30 * time.Second):
		t.Fatalf("the %s recorder did not return within 30 s of its client's end", kind)
	}

	var mocks []*models.Mock
	for quiet := time.After(500 * time.Millisecond); ; {
		select {
		case mk := <-out:
			mocks = append(mocks, mk)
		case mk := <-raw:
			mocks = append(mocks, mk)
		case <-quiet:
			return mocks, logs.All()
		}
	}
}

// serveConns serves each connection ln accepts with one, on its own
// goroutine, and closes it after.
func serveConns(one func(c net.Conn) error) func(t *testing.T, ln net.Listener) {
	return func(t *testing.T, ln net.Listener) {
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					defer func() { _ = c.Close() }()
					_ = c.SetDeadline(time.Now().Add(30 * time.Second))
					_ = one(c)
				}()
			}
		}()
	}
}

// serveHTTP serves HTTP/1.1, or HTTP/2 with prior knowledge (h2c) when h2.
func serveHTTP(h2 bool) func(t *testing.T, ln net.Listener) {
	return func(t *testing.T, ln net.Listener) {
		s := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				_, _ = w.Write([]byte("stamped"))
			}),
			ReadHeaderTimeout: 10 * time.Second,
		}
		if h2 {
			s.Protocols = new(http.Protocols)
			s.Protocols.SetUnencryptedHTTP2(true)
		}
		go func() { _ = s.Serve(ln) }()
		t.Cleanup(func() { _ = s.Close() })
	}
}

// callHTTP makes two calls on one connection, then closes it.
func callHTTP(h2 bool) func(ctx context.Context, addr string) error {
	return func(ctx context.Context, addr string) error {
		tr := &http.Transport{MaxConnsPerHost: 1}
		if h2 {
			tr.Protocols = new(http.Protocols)
			tr.Protocols.SetUnencryptedHTTP2(true)
		}
		defer tr.CloseIdleConnections()
		c := &http.Client{Transport: tr}
		for i := 0; i < 2; i++ {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s/stamp/%d", addr, i), nil)
			if err != nil {
				return err
			}
			resp, err := c.Do(req)
			if err != nil {
				return err
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		tr.CloseIdleConnections()
		return nil
	}
}

// serveHTTPNoAnswer reads a request's head whole, then closes the connection
// with no answer: nothing it was sent is left unread, so its close is a FIN.
func serveHTTPNoAnswer(c net.Conn) error {
	var got []byte
	buf := make([]byte, 4096)
	for !strings.Contains(string(got), "\r\n\r\n") {
		n, err := c.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			return err
		}
	}
	return nil
}

// callHTTPNoAnswer sends one request on its own connection and reads until
// the connection ends: it must end with no byte of an answer.
func callHTTPNoAnswer(ctx context.Context, addr string) error {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	if _, err := io.WriteString(c, "GET /stamp/no-answer HTTP/1.1\r\nHost: stamp.example\r\n\r\n"); err != nil {
		return err
	}
	got, err := io.ReadAll(c)
	if err != nil {
		return err
	}
	if len(got) != 0 {
		return fmt.Errorf("the call was answered %q; want its connection closed with no answer", got)
	}
	return nil
}

// serveGenericLines answers each line with "pong".
func serveGenericLines(c net.Conn) error {
	buf := make([]byte, 5)
	for {
		if _, err := io.ReadFull(c, buf); err != nil {
			return err
		}
		if _, err := c.Write([]byte("pong\n")); err != nil {
			return err
		}
	}
}

// callGeneric makes two exchanges: a connection's first is its preamble, a
// session's mock; the second is a test's.
func callGeneric(ctx context.Context, addr string) error {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	for i := 0; i < 2; i++ {
		if _, err := c.Write([]byte("ping\n")); err != nil {
			return err
		}
		if _, err := io.ReadFull(c, make([]byte, 5)); err != nil {
			return err
		}
	}
	return c.Close()
}

// serveMySQLOKs greets, takes the client's handshake response, and answers it
// and every command but COM_QUIT with an OK packet.
func serveMySQLOKs(c net.Conn) error {
	ok := func(seq byte) error {
		_, err := c.Write([]byte{0x07, 0x00, 0x00, seq, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00})
		return err
	}
	if _, err := c.Write(stampMySQLGreeting()); err != nil {
		return err
	}
	pkt, err := readOneMySQLPacket(c)
	if err != nil {
		return err
	}
	if err := ok(pkt[3] + 1); err != nil {
		return err
	}
	for {
		pkt, err := readOneMySQLPacket(c)
		if err != nil {
			return err
		}
		if len(pkt) > 4 && pkt[4] == 0x01 { // COM_QUIT
			return nil
		}
		if err := ok(pkt[3] + 1); err != nil {
			return err
		}
	}
}

// stampMySQLGreeting is a MySQL 8 greeting asking for mysql_native_password,
// with no CLIENT_SSL, so a client with no password answers it with an empty
// scramble and takes an OK packet as its authentication's end.
func stampMySQLGreeting() []byte {
	p := []byte{0x0a}
	p = append(p, "8.0.36\x00"...)
	p = append(p, 0x0a, 0x00, 0x00, 0x00)                         // connection id
	p = append(p, 0x38, 0x0a, 0x6e, 0x21, 0x50, 0x54, 0x57, 0x65) // scramble, part 1
	p = append(p, 0x00, 0xff, 0xf7, 0xff, 0x02, 0x00, 0xff, 0xdf, 0x15)
	p = append(p, make([]byte, 10)...)
	p = append(p, 0x59, 0x3a, 0x3e, 0x0f, 0x69, 0x2b, 0x1e, 0x51, 0x77, 0x1a, 0x2f, 0x59, 0x00) // part 2
	p = append(p, "mysql_native_password\x00"...)
	return append([]byte{byte(len(p)), 0x00, 0x00, 0x00}, p...)
}

// callMySQL connects with go-sql-driver/mysql and runs two statements.
func callMySQL(ctx context.Context, addr string) error {
	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(%s)/?timeout=5s&readTimeout=10s&writeTimeout=10s", addr))
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{"DO 1", "DO 2"} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			_ = db.Close()
			return err
		}
	}
	return db.Close()
}

// servePostgresSelect starts a session with no authentication and answers
// each simple query with one row.
func servePostgresSelect(c net.Conn) error {
	be := pgproto3.NewBackend(c, c)
	for {
		msg, err := be.ReceiveStartupMessage()
		if err != nil {
			return err
		}
		if _, ok := msg.(*pgproto3.SSLRequest); ok {
			if _, err := c.Write([]byte{'N'}); err != nil {
				return err
			}
			continue
		}
		break
	}
	be.Send(&pgproto3.AuthenticationOk{})
	for _, ps := range [][2]string{{"server_version", "16.4"}, {"client_encoding", "UTF8"}, {"standard_conforming_strings", "on"}} {
		be.Send(&pgproto3.ParameterStatus{Name: ps[0], Value: ps[1]})
	}
	be.Send(&pgproto3.BackendKeyData{ProcessID: 7, SecretKey: []byte{0, 0, 0, 9}})
	be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := be.Flush(); err != nil {
		return err
	}
	for {
		msg, err := be.Receive()
		if err != nil {
			return err
		}
		switch msg.(type) {
		case *pgproto3.Query:
			be.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{
				Name: []byte("?column?"), DataTypeOID: 23, DataTypeSize: 4, TypeModifier: -1,
			}}})
			be.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("1")}})
			be.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
			be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		case *pgproto3.Sync:
			be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		case *pgproto3.Terminate:
			return nil
		default:
			continue
		}
		if err := be.Flush(); err != nil {
			return err
		}
	}
}

// callPostgres starts a session and runs two simple queries, as a client by
// pgproto3's frontend.
func callPostgres(ctx context.Context, addr string) error {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	fe := pgproto3.NewFrontend(c, c)
	ready := func() error {
		for {
			msg, err := fe.Receive()
			if err != nil {
				return err
			}
			switch m := msg.(type) {
			case *pgproto3.ReadyForQuery:
				return nil
			case *pgproto3.ErrorResponse:
				return fmt.Errorf("the server said %s", m.Message)
			}
		}
	}
	fe.Send(&pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters: map[string]string{"user": "keploy", "database": "stamp"}})
	if err := fe.Flush(); err != nil {
		return err
	}
	if err := ready(); err != nil {
		return err
	}
	for _, q := range []string{"SELECT 1", "SELECT 2"} {
		fe.Send(&pgproto3.Query{String: q})
		if err := fe.Flush(); err != nil {
			return err
		}
		if err := ready(); err != nil {
			return err
		}
	}
	fe.Send(&pgproto3.Terminate{})
	if err := fe.Flush(); err != nil {
		return err
	}
	return c.Close()
}

// mongoOpMsg is an OP_MSG with doc as its one body section.
func mongoOpMsg(requestID, responseTo int32, doc any) ([]byte, error) {
	body, err := bson.Marshal(doc)
	if err != nil {
		return nil, err
	}
	b := make([]byte, 0, 21+len(body))
	b = binary.LittleEndian.AppendUint32(b, uint32(21+len(body)))
	b = binary.LittleEndian.AppendUint32(b, uint32(requestID))
	b = binary.LittleEndian.AppendUint32(b, uint32(responseTo))
	b = binary.LittleEndian.AppendUint32(b, 2013) // OP_MSG
	b = binary.LittleEndian.AppendUint32(b, 0)    // flag bits
	b = append(b, 0)                              // section kind 0: body
	return append(b, body...), nil
}

// readMongoMessage reads one wire message whole.
func readMongoMessage(c net.Conn) ([]byte, error) {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(hdr)
	if n < 16 || n > 1<<20 {
		return nil, fmt.Errorf("a mongo message of %d bytes", n)
	}
	msg := make([]byte, n)
	copy(msg, hdr)
	_, err := io.ReadFull(c, msg[4:])
	return msg, err
}

// serveMongoReplies answers hello as a primary and any other command with an
// empty cursor.
func serveMongoReplies(c net.Conn) error {
	for id := int32(100); ; id++ {
		msg, err := readMongoMessage(c)
		if err != nil {
			return err
		}
		var reply bson.D
		if strings.Contains(string(msg[16:]), "hello\x00") {
			reply = bson.D{{Key: "isWritablePrimary", Value: true}, {Key: "maxBsonObjectSize", Value: int32(16 << 20)},
				{Key: "maxMessageSizeBytes", Value: int32(48 << 20)}, {Key: "minWireVersion", Value: int32(0)},
				{Key: "maxWireVersion", Value: int32(21)}, {Key: "ok", Value: 1.0}}
		} else {
			reply = bson.D{{Key: "cursor", Value: bson.D{{Key: "id", Value: int64(0)}, {Key: "ns", Value: "stamp.items"},
				{Key: "firstBatch", Value: bson.A{}}}}, {Key: "ok", Value: 1.0}}
		}
		out, err := mongoOpMsg(id, int32(binary.LittleEndian.Uint32(msg[4:8])), reply)
		if err != nil {
			return err
		}
		if _, err := c.Write(out); err != nil {
			return err
		}
	}
}

// callMongo says hello, then runs two finds.
func callMongo(ctx context.Context, addr string) error {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	cmds := []bson.D{
		{{Key: "hello", Value: int32(1)}, {Key: "$db", Value: "admin"}},
		{{Key: "find", Value: "items"}, {Key: "filter", Value: bson.D{}}, {Key: "$db", Value: "stamp"}},
		{{Key: "find", Value: "items"}, {Key: "filter", Value: bson.D{{Key: "n", Value: int32(2)}}}, {Key: "$db", Value: "stamp"}},
	}
	for i, cmd := range cmds {
		msg, err := mongoOpMsg(int32(i+1), 0, cmd)
		if err != nil {
			return err
		}
		if _, err := c.Write(msg); err != nil {
			return err
		}
		if _, err := readMongoMessage(c); err != nil {
			return err
		}
	}
	return c.Close()
}

// stampTestService answers grpc.testing.TestService's EmptyCall.
type stampTestService struct {
	testgrpc.UnimplementedTestServiceServer
}

func (stampTestService) EmptyCall(context.Context, *testgrpc.Empty) (*testgrpc.Empty, error) {
	return &testgrpc.Empty{}, nil
}

func serveGRPC(t *testing.T, ln net.Listener) {
	s := grpc.NewServer()
	testgrpc.RegisterTestServiceServer(s, stampTestService{})
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(s.Stop)
}

// callGRPC makes two unary calls on one connection, then closes it.
func callGRPC(ctx context.Context, addr string) error {
	cc, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	client := testgrpc.NewTestServiceClient(cc)
	for i := 0; i < 2; i++ {
		if _, err := client.EmptyCall(ctx, &testgrpc.Empty{}); err != nil {
			_ = cc.Close()
			return err
		}
	}
	return cc.Close()
}
