package replayer

import (
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// ONE COMMAND MORE THAN THE TEST RECORDED (under exact serving, the proven
// switch; keploy's HTTP and Generic parsers do the same).
//
// Under exact serving each test recording answers one command, and is then
// in no read: a command the test runs once more than it recorded finds none
// and misses. Its miss is filed as one call more than the test recorded
// (models.MissExtraCall), naming the recording it repeats
// (integrations.RepeatedRecording) and how often the test recorded it, and
// saying what that recording's command writes (callWrite): judging reads
// them to tell the application's own clock running a write once more (a
// timer, a delayed write) from a test that changed.
//
// A command repeats a recording as backgroundAgain reads it (repeats): what
// the matcher's exact match reads, never its scores. So only a COM_QUERY or
// a COM_STMT_EXECUTE whose recording kept its query is told one: any other
// command that misses stays a miss in the test (models.MissInTest).

// extraCall tells, for a command no recording answered under exact serving,
// whether it is one command more than the open test recorded: identical (as
// repeats reads it) to a command the test's own recordings already answered
// (integrations.ExactConsumedReader, which lists none while exact serving is
// off). It returns how many of them the command repeats, 0 when it is no
// such command, and the recording it repeats: of them, the one recorded last
// (integrations.RepeatedRecording). It reads the answered recordings only:
// nothing is consumed or bound.
func extraCall(logger *zap.Logger, req mysql.Request, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext) (int, *models.Mock) {
	er, ok := mockDb.(integrations.ExactConsumedReader)
	if !ok {
		return 0, nil
	}
	var copies []*models.Mock
	for _, mk := range er.ConsumedTestMocks() {
		// A recording of one MySQL command: another kind's records none.
		if len(mk.Spec.MySQLRequests) == 1 && repeats(logger, mk, req, decodeCtx) {
			copies = append(copies, mk)
		}
	}
	return len(copies), integrations.RepeatedRecording(copies)
}
