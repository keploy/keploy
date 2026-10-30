package generic

import (
	"bytes"
	"context"
	"crypto/sha256"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// stateServed says what F's serve of m, the recording fuzzyMatch just
// consumed for the request sent (in the pieces the connection's reads
// returned), gave the application, as the learn step reads a recording F
// served (learn.Order.ServedView). F answers with m's recorded answer whole,
// so the serve is m's recording byte for byte
// (learn.Tracker.ServedMockAsRecorded) where sent is m's recorded request
// byte for byte (sentAsRecorded); otherwise the view of m with the request
// as sent (learn.Tracker.ServedMockLive, servedView). Under the switch every
// serve is exact, from whichever view is in force: a test's own exchange
// while its test is open; a connection's first stated session, which is
// protocol-phase and shown in every view (the pool's between tests, an open
// test's alike); any other reusable recording a view shows (between tests
// the pool's slice; in an open test its worker scope, or the pool under the
// background rules). Exact serving answers only the recorded bytes by this
// same compare (findExactMatch), so there every serve is stated as recorded.
// Without an exact view (never under the switch once the agent has staged a
// set) the view is stated for a request only like the recording (a
// similarity), one that differs from it where its recorded piece is
// obfuscated (noise, never compared there), or one whose bytes differ from
// the recorded piece's though they read as the text the recording stores it
// in (a binary piece's base64 text: the text compare serves it). Without a
// statement a later call of the set reads m as a serve no adapter stated,
// and refuses every changed value that may stand there. It is told after the
// consume, which numbered the serve, before F writes the answer: a write
// that fails gives the application less than the stated view, which only
// refuses more. Only under the proven switch (integrations.TellServed).
func stateServed(ctx context.Context, m *models.Mock, sent [][]byte) {
	integrations.TellServed(ctx, m, func() (func() (*models.Mock, error), string, bool) {
		if sentAsRecorded(m, sent) {
			return nil, "", true
		}
		view, key := servedView(m, sent)
		return view, key, false
	})
}

// sentAsRecorded reports whether sent is m's recorded request byte for
// byte, in its own pieces or cut where the recorded ones end: each piece the
// bytes its recorded piece holds (isRecordedBytes), as exact serving
// compares them, never the text the recording stores it in. A live request
// that is a binary recording's base64 text, or whose bytes' base64 is a
// text recording, sent other bytes than recorded: stated as recorded, the
// learn step would read the recorded bytes where the application sent
// others.
func sentAsRecorded(m *models.Mock, sent [][]byte) bool {
	return newSentRequest(sent).isRecorded(m.Spec.GenericRequests, nil, true)
}

// servedView is F's view of m, which F served to the request sent with m's
// recorded answer (stateServed): m with the request as the application sent
// it, its bytes as one piece in the form the recorder keeps a piece
// (encodePayload: text as it is, other bytes base64), and the key that names
// it among m's views: those bytes, hashed. Two serves of m whose requests
// sent the same bytes, in whatever pieces they were read, gave the same
// view. What a set keeps of it is the request, never the answer.
func servedView(m *models.Mock, sent [][]byte) (func() (*models.Mock, error), string) {
	req := bytes.Join(sent, nil)
	sum := sha256.Sum256(req)
	return func() (*models.Mock, error) {
		v := m.ShallowCopy()
		v.Spec.GenericRequests = []models.Payload{encodePayload(req, models.FromClient)}
		return v, nil
	}, string(sum[:])
}
