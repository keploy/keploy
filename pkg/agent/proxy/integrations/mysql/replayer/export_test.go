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

// StrictEngineForTest is the schema-noise engine matchCommand runs with under
// strict enforcement.
func StrictEngineForTest() *schemanoise.Engine {
	return schemanoise.New(mysqlNoiseAdapter{}, false, true)
}
