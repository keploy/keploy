package replayer

import (
	"context"
	"fmt"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// MatchCommandForTest exports matchCommandWith to this package's external
// tests, which run it against the real mock manager. With fullScan every
// command scans the whole pool: the reference the first passes are checked
// against. The miss diagnostics come back as text.
func MatchCommandForTest(ctx context.Context, logger *zap.Logger, req mysql.Request, db integrations.MockMemDb, dctx *wire.DecodeContext, eng *schemanoise.Engine, fullScan bool) (*mysql.Response, bool, string, error) {
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
func MatchWithEngineForTest(ctx context.Context, logger *zap.Logger, req mysql.Request, db integrations.MockMemDb, dctx *wire.DecodeContext, eng *schemanoise.Engine) (*mysql.Response, bool, string, error) {
	resp, ok, miss, err := matchCommand(ctx, logger, req, db, dctx, eng, nil)
	missText := ""
	if miss != nil {
		missText = fmt.Sprintf("%+v", *miss)
	}
	return resp, ok, missText, err
}

// StrictEngineForTest is the schema-noise engine matchCommand runs with under
// strict enforcement.
func StrictEngineForTest() *schemanoise.Engine {
	return schemanoise.New(mysqlNoiseAdapter{}, false, true)
}

// MatchCommandExaminedForTest is MatchCommandForTest that also returns how
// many candidates the scan visited (matchOptions.examined).
func MatchCommandExaminedForTest(ctx context.Context, req mysql.Request, db integrations.MockMemDb, dctx *wire.DecodeContext, eng *schemanoise.Engine, fullScan bool) (*mysql.Response, bool, int, error) {
	examined := 0
	resp, ok, _, err := matchCommandWith(ctx, zap.NewNop(), req, db, dctx, eng, nil, matchOptions{fullScan: fullScan, examined: &examined})
	return resp, ok, examined, err
}

// MatchCommandBodiesForTest is MatchCommandForTest that also returns how many
// live request bodies the command built (matchOptions.bodiesBuilt). With eager
// it builds the body before the scan, as the matcher did before it built the
// body on first read (matchOptions.eagerBody).
func MatchCommandBodiesForTest(ctx context.Context, logger *zap.Logger, req mysql.Request, db integrations.MockMemDb, dctx *wire.DecodeContext, eng *schemanoise.Engine, eager bool) (*mysql.Response, bool, string, int, error) {
	built := 0
	resp, ok, miss, err := matchCommandWith(ctx, logger, req, db, dctx, eng, nil, matchOptions{bodiesBuilt: &built, eagerBody: eager})
	missText := ""
	if miss != nil {
		missText = fmt.Sprintf("%+v", *miss)
	}
	return resp, ok, missText, built, err
}

// NoiseEngineForTest is the schema-noise engine matchCommand runs with, with
// detection (auto-replay learning request-body noise) and strict enforcement
// on or off.
func NoiseEngineForTest(detection, strict bool) *schemanoise.Engine {
	return schemanoise.New(mysqlNoiseAdapter{}, detection, strict)
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

// CountPreparedKeysForTest runs f and returns how many times a mock's
// preparedIndex keys were computed meanwhile, by the store's indexes or by a
// walk. f must not run matches on other goroutines past its return.
func CountPreparedKeysForTest(f func()) int {
	var n atomic.Int64
	keys := preparedIndex.Keys
	preparedIndex.Keys = func(m *models.Mock) []string {
		n.Add(1)
		return keys(m)
	}
	defer func() { preparedIndex.Keys = keys }()
	f()
	return int(n.Load())
}
