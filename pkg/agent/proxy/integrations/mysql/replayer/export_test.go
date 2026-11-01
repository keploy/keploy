package replayer

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	mysqlUtils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// MatchCommandForTest exports matchCommandWith to this package's external
// tests, which run it against the real mock manager. With fullScan every
// command scans the whole pool: the reference the first passes are checked
// against. The miss diagnostics come back as text.
func MatchCommandForTest(ctx context.Context, logger *zap.Logger, req mysql.Request, db integrations.MockMemDb, dctx *wire.DecodeContext, eng *mocknoise.Engine, fullScan bool) (*mysql.Response, bool, string, error) {
	resp, ok, miss, err := matchCommandWith(ctx, logger, req, db, dctx, eng, nil, matchOptions{fullScan: fullScan})
	missText := ""
	if miss != nil {
		missText = fmt.Sprintf("%+v", *miss)
	}
	return resp, ok, missText, err
}

// MatchExaminedForTest is MatchCommandForTest's match (no full scan) that
// also returns how many candidates the scan visited (matchOptions.examined).
func MatchExaminedForTest(ctx context.Context, logger *zap.Logger, req mysql.Request, db integrations.MockMemDb, dctx *wire.DecodeContext) (*mysql.Response, bool, int, error) {
	examined := 0
	resp, ok, _, err := matchCommandWith(ctx, logger, req, db, dctx, nil, nil, matchOptions{examined: &examined})
	return resp, ok, examined, err
}

// MatchForTest is the command phase's match of one command
// (matchCommand), as simulateCommandPhase runs it for each command it reads.
// The miss comes back as text.
func MatchForTest(ctx context.Context, logger *zap.Logger, req mysql.Request, db integrations.MockMemDb, dctx *wire.DecodeContext) (*mysql.Response, bool, string, error) {
	return MatchWithEngineForTest(ctx, logger, req, db, dctx, nil)
}

// MatchWithEngineForTest is MatchForTest with the schema-noise engine eng
// (StrictEngineForTest: strict enforcement).
func MatchWithEngineForTest(ctx context.Context, logger *zap.Logger, req mysql.Request, db integrations.MockMemDb, dctx *wire.DecodeContext, eng *mocknoise.Engine) (*mysql.Response, bool, string, error) {
	resp, ok, miss, err := matchCommand(ctx, logger, req, db, dctx, eng, nil)
	missText := ""
	if miss != nil {
		missText = fmt.Sprintf("%+v", *miss)
	}
	return resp, ok, missText, err
}

// MatchWithNoiseConfigForTest is MatchForTest with the user's noise
// configuration (test.globalNoise), its requestBody bucket read as the
// command phase reads it (requestBodyNoise).
func MatchWithNoiseConfigForTest(ctx context.Context, logger *zap.Logger, req mysql.Request, db integrations.MockMemDb, dctx *wire.DecodeContext, cfg map[string]map[string][]string) (*mysql.Response, bool, string, error) {
	resp, ok, miss, err := matchCommand(ctx, logger, req, db, dctx, nil, requestBodyNoise(cfg))
	missText := ""
	if miss != nil {
		missText = fmt.Sprintf("%+v", *miss)
	}
	return resp, ok, missText, err
}

// StrictEngineForTest is the schema-noise engine matchCommand runs with under
// strict enforcement.
func StrictEngineForTest() *mocknoise.Engine {
	return mocknoise.New(mysqlNoiseAdapter{}, false, true)
}

// MatchCommandExaminedForTest is MatchCommandForTest that also returns how
// many candidates the scan visited (matchOptions.examined).
func MatchCommandExaminedForTest(ctx context.Context, req mysql.Request, db integrations.MockMemDb, dctx *wire.DecodeContext, eng *mocknoise.Engine, fullScan bool) (*mysql.Response, bool, int, error) {
	examined := 0
	resp, ok, _, err := matchCommandWith(ctx, zap.NewNop(), req, db, dctx, eng, nil, matchOptions{fullScan: fullScan, examined: &examined})
	return resp, ok, examined, err
}

// MatchCommandBodiesForTest is MatchCommandForTest that also returns how many
// live request bodies the command built (matchOptions.bodiesBuilt). With eager
// it builds the body before the scan, as the matcher did before it built the
// body on first read (matchOptions.eagerBody).
func MatchCommandBodiesForTest(ctx context.Context, logger *zap.Logger, req mysql.Request, db integrations.MockMemDb, dctx *wire.DecodeContext, eng *mocknoise.Engine, eager bool) (*mysql.Response, bool, string, int, error) {
	built := 0
	resp, ok, miss, err := matchCommandWith(ctx, logger, req, db, dctx, eng, nil, matchOptions{bodiesBuilt: &built, eagerBody: eager})
	missText := ""
	if miss != nil {
		missText = fmt.Sprintf("%+v", *miss)
	}
	return resp, ok, missText, built, err
}

// MatchCommandGateForTest is MatchCommandForTest that also returns how many
// candidates the strict gate compared with the live request
// (matchOptions.compared). With eager the gate decides on each fallback of a
// COM_STMT_EXECUTE as the walk meets it, as the matcher did before it
// deferred the gate past the walk (matchOptions.eagerGate).
func MatchCommandGateForTest(ctx context.Context, logger *zap.Logger, req mysql.Request, db integrations.MockMemDb, dctx *wire.DecodeContext, eng *mocknoise.Engine, eager bool) (*mysql.Response, bool, string, int, error) {
	compared := 0
	resp, ok, miss, err := matchCommandWith(ctx, logger, req, db, dctx, eng, nil, matchOptions{compared: &compared, eagerGate: eager})
	missText := ""
	if miss != nil {
		missText = fmt.Sprintf("%+v", *miss)
	}
	return resp, ok, missText, compared, err
}

// NoiseEngineForTest is the schema-noise engine matchCommand runs with, with
// detection (auto-replay learning request-body noise) and strict enforcement
// on or off.
func NoiseEngineForTest(detection, strict bool) *mocknoise.Engine {
	return mocknoise.New(mysqlNoiseAdapter{}, detection, strict)
}

// ConfigTagReadsForTest runs fn and returns how many times it read a mock's
// config tag (hasConfigTag), a map lookup on the mock's metadata.
func ConfigTagReadsForTest(fn func()) int {
	var n atomic.Int64
	h := func() { n.Add(1) }
	configTagReadHook.Store(&h)
	defer configTagReadHook.Store(nil)
	fn()
	return int(n.Load())
}

// CountStatementKeysForTest runs f and returns how many times a mock's
// statement keys (preparedIndex's, queryIndex's or executeIndex's) were
// computed meanwhile, by the store's indexes or by a walk, and how many keys
// of each kind they filed, by the key's first byte: P (a PREPARE), S (a
// PREPARE's entry), C (a CLOSE), Q (a COM_QUERY) or E (an EXECUTE). f must
// not run matches on other goroutines past its return.
func CountStatementKeysForTest(f func()) (int, map[string]int) {
	var (
		mu     sync.Mutex
		calls  int
		filed  = map[string]int{}
		counts = func(keys func(*models.Mock) []string) func(*models.Mock) []string {
			return func(m *models.Mock) []string {
				ks := keys(m)
				mu.Lock()
				calls++
				for _, k := range ks {
					filed[k[:1]]++
				}
				mu.Unlock()
				return ks
			}
		}
	)
	for _, ix := range []*integrations.MockIndex{preparedIndex, queryIndex, executeIndex} {
		keys := ix.Keys
		ix.Keys = counts(keys)
		defer func() { ix.Keys = keys }()
	}
	f()
	mu.Lock()
	defer mu.Unlock()
	return calls, filed
}

// CountUnnamedExecKeysForTest runs f and returns how many times a mock's
// unnamedExecIndex keys were computed meanwhile, by the store's indexes or by
// an exact view's index of the mocks it owns. f must not run matches on other
// goroutines past its return.
func CountUnnamedExecKeysForTest(f func()) int {
	var n atomic.Int64
	keys := unnamedExecIndex.Keys
	unnamedExecIndex.Keys = func(m *models.Mock) []string {
		n.Add(1)
		return keys(m)
	}
	defer func() { unnamedExecIndex.Keys = keys }()
	f()
	return int(n.Load())
}

// HandshakeDstForTest is the server the recorded handshakes of this
// package's tests were recorded for (their destAddr).
const HandshakeDstForTest = testDBAddr

// HandshakeMockForTest is a recorded native-password login that the server
// let in (nativeHandshake), named mock-0: the greeting, the client's
// HandshakeResponse41 and the OK.
func HandshakeMockForTest() *models.Mock { return nativeHandshake() }

// ClientHandshakeForTest is the client's HandshakeResponse41 the recorded
// handshakes of this package's tests hold (clientHandshake), by user app.
func ClientHandshakeForTest() *mysql.HandshakeResponse41Packet {
	return clientHandshake("app", mysql.Native)
}

// SwitchedHandshakeForTest is a recorded native-password login that the
// server let in after asking the client to switch (switchedHandshake), named
// mock-0: the client's AuthSwitchResponse is recorded by its length, n bytes,
// only (mysqlUtils.ClientAuthData), as the recorder holds it.
func SwitchedHandshakeForTest(n int) *models.Mock {
	m := switchedHandshake(okAnswer(4))
	m.Spec.MySQLRequests[1] = authData(3, n, mysql.AuthSwithResponse)
	return m
}

// FullAuthHandshakeForTest is a recorded caching_sha2_password login in full
// without TLS that the server let in (fullAuthHandshake), named mock-0: the
// client's encrypted password is recorded by its length, n bytes, only.
func FullAuthHandshakeForTest(n int) *models.Mock {
	m := fullAuthHandshake(okAnswer(6))
	m.Spec.MySQLRequests[2] = authData(5, n, mysql.EncryptedPassword)
	return m
}

// authData is a packet of n bytes of authentication data of type typ,
// numbered seq, as a recording holds it (mysqlUtils.ClientAuthData).
func authData(seq uint8, n int, typ string) mysql.Request {
	return mysqlUtils.ClientAuthData(mysql.Packet{Header: mysql.Header{PayloadLength: uint32(n), SequenceID: seq}, Payload: make([]byte, n)}, typ)
}

// NativeHandshakeForTest is the recorded handshake of a native-password
// login (nativeHandshake), for this package's external tests.
func NativeHandshakeForTest() *models.Mock { return nativeHandshake() }

// ReplayCommandsForTest serves one client connection from db as the proxy
// does (replay): the client logs in as NativeHandshakeForTest recorded, then
// sends each command payload and reads the first packet of its reply, but
// for the last command's, which it leaves unread. It returns those packets,
// and what Replay returns once it ends the connection.
func ReplayCommandsForTest(t *testing.T, db integrations.MockMemDb, commands ...[]byte) ([][]byte, <-chan error) {
	t.Helper()
	client, done := replay(t, db)
	client.greetAndAnswer("app", mysql.Native)
	if ok := client.read(); len(ok) == 0 || ok[0] != mysql.OK {
		t.Fatalf("the client read % x after its handshake; want the recorded OK", ok)
	}
	var replies [][]byte
	for i, cmd := range commands {
		client.send(0, cmd)
		if i < len(commands)-1 {
			replies = append(replies, client.read())
		}
	}
	return replies, done
}
