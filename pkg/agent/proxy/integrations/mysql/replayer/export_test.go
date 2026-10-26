package replayer

import (
	"context"
	"fmt"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
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
