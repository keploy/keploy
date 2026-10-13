// Mismatch-report diagnostics for the MySQL replayer: the data matchCommand
// hands back on a "no matching mock" outcome, the formatting helpers
// simulateCommandPhase uses to render it into a models.MockMismatchReport, and
// the misses of the connection phase.
package replayer

import (
	"fmt"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// The steps of the connection phase a connection miss names: what the client
// was at when the recording held no answer for it.
const (
	stepGreeting           = "new connection" // the client waits to be greeted
	stepHandshakeResponse  = mysql.HandshakeResponse41
	stepAuthSwitchResponse = mysql.AuthSwithResponse
	stepCachingSha2        = "caching_sha2_password authentication"
	stepPublicKey          = "public key request"
	stepEncryptedPassword  = "encrypted password"
	stepPlainPassword      = "plain password"
)

// newConnectionMiss is a miss of the connection phase at step, of a client
// that dialled dst: the client opened a MySQL connection, or sent a packet of
// its handshake, and the recording holds no answer for it, for the reason
// why. phase and candidates are the report's MatchPhase and CandidateCount.
// why never holds a credential: a password mismatch names the step, not the
// values.
//
// The replayer returns it without answering the client, and the proxy closes
// the connection, so this miss is the only account of why the application's
// connection failed. It wraps models.ErrNoMockMatched and carries its report,
// as a command-phase miss does: the proxy keeps an error as a miss only then,
// and drops any other. Returned before anything more is written to the client,
// it reaches the proxy's reporter before the proxy closes the client's
// connection, so it is queued for its test before the application sees the
// connection fail.
func newConnectionMiss(dst, step, why, phase string, candidates int) error {
	next := "The application's MySQL handshake differs from the recorded one (user, database, client options or authentication exchange), or the recorded handshake ends before this step. Re-record with '" + mismatch.RecordCommand() + "' if the application's MySQL connection settings changed."
	if phase == models.MatchPhaseNoMocks {
		next = "The recording holds no complete MySQL handshake to answer this connection: it never recorded one, or the ones it holds are incomplete. Re-record with '" + mismatch.RecordCommand() + "' if the application's MySQL connections changed."
	}
	report := mismatch.NewReport(mismatch.ProtocolMySQL, "connection phase: "+step).
		WithDestination(dst).
		WithPhase(phase, candidates).
		WithDiff(why).
		WithNextSteps(next).
		Build()
	return models.NewMockMismatchError(fmt.Errorf("mysql connection phase, %s: %s: %w", step, why, models.ErrNoMockMatched), report)
}

// matchedHandshakeMiss is a miss of the connection phase at step after the
// client's HandshakeResponse41 matched one recorded handshake: that one mock
// was compared, and it holds no answer for step, for the reason why.
func matchedHandshakeMiss(dst, step, why string) error {
	return newConnectionMiss(dst, step, why, models.MatchPhaseExhausted, 1)
}

// destinationOf is the upstream a client dialled, as a miss's report names it:
// "" when unknown, or when the capture layer fabricated it (a report must not
// name a server the application never dialled).
func destinationOf(dstCfg *models.ConditionalDstCfg) string {
	if dstCfg == nil || dstCfg.AddrFabricated {
		return ""
	}
	return dstCfg.Addr
}

// mockMiss carries the diagnostics of a "no matching mock" outcome so the
// caller can build a meaningful mismatch report: the closest candidate (name +
// its recorded query), field-level diffs against the first strict-rejected
// candidate (noise-vocabulary "body."-paths with recorded/live values), and
// how many candidates strict enforcement ruled out.
type mockMiss struct {
	closestQuery   string
	closestMock    string
	fieldDiffs     []models.MockFieldDiff
	strictRejected int
	// candidateCount is how many MySQL mocks the scan actually considered, and
	// matchPhase how far the cascade got. Both must be SET, not left zero: the
	// mismatch report renders them verbatim, and a zero candidateCount reads as
	// "the pool was empty" — which is what turned a drifted-query miss into a
	// hunt for a lost recording.
	candidateCount int
	matchPhase     string
}

// formatExecParams renders bound parameter values for mismatch reports,
// bounded per-value and overall so a blob param can't flood the report.
func formatExecParams(params []mysql.Parameter) string {
	if len(params) == 0 {
		return ""
	}
	vals := make([]string, 0, len(params))
	for _, p := range params {
		vals = append(vals, truncate(fmt.Sprintf("%v", p.Value), 48))
	}
	return "params=[" + strings.Join(vals, ", ") + "]"
}
