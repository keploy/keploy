package replayer

import (
	"context"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// ownCommands is the index of the open test's own MySQL recordings the
// own-call check reads (integrations.OwnRepeatRead), each filed under its
// command's type (commandType): the matcher never answers a command with a
// recording of another type, so a command reads only the test's recordings
// of its type, never every recording the test owns.
var ownCommands = integrations.NewOwnIndex(models.MySQL, func(mk *models.Mock) (string, bool) {
	if len(mk.Spec.MySQLRequests) == 0 {
		return "", false
	}
	return commandType(mk.Spec.MySQLRequests[0].PacketBundle), true
})

// commandType is a command's type, "" for one with no header.
func commandType(b mysql.PacketBundle) string {
	if b.Header == nil {
		return ""
	}
	return b.Header.Type
}

// ownRepeats are the recordings of own, the open test's own MySQL
// recordings of the command's type, answered or not, that the command
// matches as the matcher judges it under exact serving, in the order the
// matcher finds them, at most limit of them (0: all). The own-call check
// (integrations.OwnRepeatRead) asks for one; a command that then missed with
// the background hidden, one call more than the test recorded, for all of
// them: how often the test recorded it. The match is a dry one
// (matchOptions.probed) on a view that holds only own and the connection's
// recordings (its prepared statements, and a CLOSE it made between tests):
// it consumes nothing, binds no statement id and sends no served event. Each
// own recording it finds is passed over by the next, and the first it finds
// that is not the test's own ends it: the matcher prefers that recording to
// every own one left, so the command is its, not one more of the test's. Any
// recorded CLOSE matches a live one by its header alone, so the test's CLOSE
// of another statement matches a live CLOSE too, below the pool's CLOSE of
// the live statement: that one ends the scan, and the pool answers it.
func ownRepeats(ctx context.Context, req mysql.Request, db integrations.MockMemDb, own []*models.Mock, decodeCtx *wire.DecodeContext, noiseEngine *mocknoise.Engine, userBodyNoise map[string][]string, limit int) []*models.Mock {
	if len(own) == 0 {
		return nil
	}
	view := answeredOnly{MockMemDb: db, mocks: own}
	ownSet := make(map[*models.Mock]struct{}, len(own))
	for _, mk := range own {
		ownSet[mk] = struct{}{}
	}
	opts := matchOptions{refused: make(map[*models.Mock]struct{})}
	var out []*models.Mock
	for limit == 0 || len(out) < limit {
		var probed *models.Mock
		opts.probed = &probed
		if _, _, _, err := matchCommandWith(ctx, zap.NewNop(), req, view, decodeCtx, noiseEngine, userBodyNoise, opts); err != nil || probed == nil {
			return out
		}
		// A connection's recording the scan reads too is no recording of
		// the test's, and the matcher prefers it to the own ones left.
		if _, ok := ownSet[probed]; !ok {
			return out
		}
		out = append(out, probed)
		opts.refused[probed] = struct{}{}
	}
	return out
}

// answeredOnly is the store ownRepeats matches a command on: the open test's
// own recordings of its type as its only per-test mocks, under exact
// serving, with the connection's prepared statements and the test's window
// read from the store; no session, start-up or background
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
