// Mismatch-report diagnostics for the MySQL replayer: the data matchCommand
// hands back on a "no matching mock" outcome, the formatting helpers
// simulateCommandPhase uses to render it into a models.MockMismatchReport, and
// the misses of the connection phase.
package replayer

import (
	"errors"
	"fmt"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
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

// restagedConnectionNote is what a connection-phase miss says when the
// replay staged a test set again after the connection first read the round
// (markConnectionMiss).
const restagedConnectionNote = "keploy replay staged a test set again while it answered this connection's handshake, so keploy answered it with no recording: a connection is never answered with, nor compared with, a recording of a staging made after it opened."

// markConnectionMiss is err, a miss of the connection phase, carrying where
// the connection first read the round (call: integrations.StampMark): the
// proxy says from it whether the miss was made while the set's mocks were
// being staged, and in a test or outside every test, never from the view
// serving as the miss reaches it. When the replay staged a test set again
// after that read (restaged), or the store marked the connection's round but
// could not hold it (integrations.UnheldWhy of held), the connection phase
// read nothing of the set for it (heldAway): the miss says so in place of its
// next steps, as no shortfall of the recording. Any other error is err.
func markConnectionMiss(err error, call integrations.CallRound, restaged func() bool, held integrations.MockMemDb) error {
	var r interface {
		MismatchReport() *models.MockMismatchReport
	}
	if !errors.Is(err, models.ErrNoMockMatched) || !errors.As(err, &r) || r.MismatchReport() == nil {
		return err
	}
	report := r.MismatchReport()
	integrations.StampMark(report, call)
	if unheld := integrations.UnheldWhy(held); unheld != "" {
		report.NextSteps = unheld
	} else if restaged() {
		report.NextSteps = restagedConnectionNote
	}
	return err
}

// heldAway reports whether the connection phase's store, held to its
// connection's mark, read nothing of the set because of the hold: the replay
// staged a test set again after the mark (restaged), or the store could not
// hold it (integrations.UnheldWhy).
func heldAway(restaged func() bool, held integrations.MockMemDb) bool {
	return restaged() || integrations.UnheldWhy(held) != ""
}

// matchedHandshakeMiss is a miss of the connection phase at step after the
// client's HandshakeResponse41 matched one recorded handshake: that one mock
// was compared, and it holds no answer for step, for the reason why.
func matchedHandshakeMiss(dst, step, why string) error {
	return newConnectionMiss(dst, step, why, models.MatchPhaseExhausted, 1)
}

// unencodableMiss is a miss of the connection phase at step: the recorded
// packet the client is to be answered with (what) cannot be encoded, err says
// why, so the recording holds no answer replay can write. Nothing is written
// for it.
func unencodableMiss(dst, step, what string, err error) error {
	return newConnectionMiss(dst, step, fmt.Sprintf("the recorded %s cannot be encoded: %v", what, err), models.MatchPhaseProtoError, 1)
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
	// why and nextSteps, when set, are what the report says about the miss
	// in place of the generic diff note and advice.
	why       string
	nextSteps string
	// restaged is set when the command's matcher, held to its first read,
	// read nothing because of the hold (matchCommand): the replay staged a
	// test set again under the command, or its store could not hold it
	// (integrations.UnheldWhy). The miss is no recording-side shortfall.
	restaged bool
	// call is the command's first read of the round (matchCommand), for its
	// report to say whether that read found a staging (integrations.StampMark).
	call integrations.CallRound
	// extraCall is, for a command one more than the open test recorded (its
	// own recordings of it answered, ownRepeats), how many times the test
	// recorded it (models.MissExtraCall); 0 for every other miss.
	extraCall int
}

// maxUnknownStatementsNamed caps how many mocks a miss of an unknown statement
// names.
const maxUnknownStatementsNamed = 5

// unknownStatementsMiss is the miss of a COM_STMT_EXECUTE whose test's
// recording holds EXECUTE mocks that lost their statement (unknown, in pool
// order): refused because the mock it would have served instead (instead)
// was recorded for another test, or, with instead nil, matched by no mock.
// candidates is the scan's candidate count.
func unknownStatementsMiss(unknown []*models.Mock, instead *models.Mock, candidates int) *mockMiss {
	names := make([]string, 0, min(len(unknown), maxUnknownStatementsNamed))
	for _, m := range unknown[:min(len(unknown), maxUnknownStatementsNamed)] {
		names = append(names, m.Name)
	}
	list := strings.Join(names, ", ")
	if len(unknown) > len(names) {
		list += fmt.Sprintf(" and %d more", len(unknown)-len(names))
	}
	why := fmt.Sprintf("this test's recording holds COM_STMT_EXECUTE mocks whose statement keploy cannot tell (%s): the mock does not record its query, and the COM_STMT_PREPARE of each is gone from the test set, which holds that connection's handshake or an earlier statement of it. One of them may be this command's own", list)
	if instead != nil {
		why += fmt.Sprintf(", so keploy does not answer it with %s, which was recorded for another test", instead.Name)
	}
	return &mockMiss{
		closestMock:    unknown[0].Name,
		candidateCount: candidates,
		matchPhase:     models.MatchPhaseExhausted,
		why:            why,
		nextSteps:      "The COM_STMT_PREPARE mocks these EXECUTEs need were most likely deleted by an older keploy's mock prune (--remove-unused-mocks, or the prune after an auto-replay), which did not keep the mocks a kept mock needs. Re-record the test set with '" + mismatch.RecordCommand() + "', or restore its mocks from a copy made before the prune.",
	}
}

// formatExecParams renders bound parameter values for mismatch reports,
// bounded per-value and overall so a blob param can't flood the report.
func formatExecParams(params []mysql.Parameter) string {
	if len(params) == 0 {
		return ""
	}
	vals := make([]string, 0, len(params))
	for _, p := range params {
		v := p.Value
		if p.Name != "" && v != nil {
			// A query attribute: its name, not its value's shape, may be
			// what makes it a secret (executeForLog).
			v = maskedAttributeValue
		}
		vals = append(vals, truncate(fmt.Sprintf("%v", v), 48))
	}
	return "params=[" + strings.Join(vals, ", ") + "]"
}
