package replayer

import (
	"context"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// ownRepeats are the recordings of answered, the open test's MySQL
// recordings that answered (integrations.OwnRepeatRead), that the command
// matches as the matcher judges it under exact serving: the command is one
// call more than the test recorded of it unless the test recorded it again.
// The match is a dry one (matchOptions.probed) on a view that holds only
// answered and the connection's prepared statements: it consumes nothing,
// binds no statement id and sends no served event. Each recording it finds
// is passed over by the next, so it ends with every one the command matches.
func ownRepeats(ctx context.Context, req mysql.Request, db integrations.MockMemDb, answered []*models.Mock, decodeCtx *wire.DecodeContext, noiseEngine *schemanoise.Engine, userBodyNoise map[string][]string) []*models.Mock {
	view := answeredOnly{MockMemDb: db, mocks: answered}
	own := make(map[*models.Mock]struct{}, len(answered))
	for _, mk := range answered {
		own[mk] = struct{}{}
	}
	opts := matchOptions{refused: make(map[*models.Mock]struct{})}
	var out []*models.Mock
	for {
		var probed *models.Mock
		opts.probed = &probed
		if _, _, _, err := matchCommandWith(ctx, zap.NewNop(), req, view, decodeCtx, noiseEngine, userBodyNoise, opts); err != nil || probed == nil {
			return out
		}
		// A connection's prepared statement the scan reads too is no
		// recording of the test's: passed over, as each answered one is.
		if _, ok := own[probed]; ok {
			out = append(out, probed)
		}
		opts.refused[probed] = struct{}{}
	}
}

// answeredOnly is the store ownRepeats matches a command on: the open test's
// answered recordings as its only per-test mocks, under exact serving, each
// of them the test's own, with the connection's prepared statements and the
// test's window read from the store; no session, start-up or background
// recording, and no consume (the dry match reaches none).
type answeredOnly struct {
	integrations.MockMemDb
	mocks []*models.Mock
}

func (answeredOnly) ExactServing() bool { return true }

func (a answeredOnly) InFlightIntoOpenTest(m *models.Mock) bool {
	return integrations.InFlightIntoOpenTest(a.MockMemDb, m)
}

func (a answeredOnly) GetPerTestMocksInWindow() ([]*models.Mock, error)  { return a.mocks, nil }
func (a answeredOnly) GetFilteredMocksInWindow() ([]*models.Mock, error) { return a.mocks, nil }
func (a answeredOnly) GetFilteredMocks() ([]*models.Mock, error)         { return a.mocks, nil }
func (answeredOnly) GetSessionMocks() ([]*models.Mock, error)            { return nil, nil }
func (answeredOnly) GetUnFilteredMocks() ([]*models.Mock, error)         { return nil, nil }
func (answeredOnly) GetStartupMocks() ([]*models.Mock, error)            { return nil, nil }
func (answeredOnly) GetStartupMocksByKind(models.Kind) ([]*models.Mock, error) {
	return nil, nil
}
func (answeredOnly) GetSessionScopedMocks() ([]*models.Mock, error) { return nil, nil }
func (answeredOnly) GetSessionMocksInWindow(time.Time, time.Time) ([]*models.Mock, error) {
	return nil, nil
}
func (answeredOnly) UpdateUnFilteredMock(*models.Mock, *models.Mock) bool { return false }
func (answeredOnly) DeleteFilteredMock(models.Mock) bool                  { return false }
func (answeredOnly) DeleteUnFilteredMock(models.Mock) bool                { return false }
func (answeredOnly) DeleteStartupMock(models.Mock) bool                   { return false }
func (answeredOnly) MarkMockAsUsed(models.Mock) bool                      { return false }
