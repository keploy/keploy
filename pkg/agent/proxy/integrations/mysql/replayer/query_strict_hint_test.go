package replayer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// A query that schema-noise strict turns every recording away from, at a
// request field outside the noise, is a miss whose next steps say where to
// mark that field: under keploy.yml's key, test.globalNoise.global.requestBody,
// as the exact-match misses name it (exactDriftNextSteps), never under
// test.globalNoise.requestBody, a key keploy.yml does not have, where a
// field marked as told would never be noise.
func TestAStrictRejectionNamesTheNoiseKeyKeployYmlHas(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	q := &models.Mock{Version: "api.keploy.io/v1beta1", Name: "mock-1", Kind: models.MySQL}
	q.Spec.Metadata = map[string]string{"type": "mocks"}
	q.Spec.MySQLRequests = []mysql.Request{comQueryReq("SELECT v FROM t WHERE id = 1")}
	q.Spec.MySQLResponses = []mysql.Response{{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 7, SequenceID: 1}, Type: "OK"},
		Message: &mysql.OKPacket{Header: mysql.OK},
	}}}
	client, done := replayIn(t, context.Background(), &fakeMockDb{session: []*models.Mock{nativeHandshake()}, perTest: []*models.Mock{q}}, zap.NewNop(), models.OutgoingOptions{MockNoiseStrict: true})
	client.greetAndAnswer("app", mysql.Native)
	if ok := client.read(); len(ok) == 0 || ok[0] != mysql.OK {
		t.Fatalf("the client read %x after its handshake; want the recorded OK", ok)
	}
	client.send(0, append([]byte{mysql.COM_QUERY}, "SELECT v FROM t WHERE id = 2"...))
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the command was answered; want a miss that ends the connection")
	}
	var r reportCarrier
	if !errors.Is(err, models.ErrNoMockMatched) || !errors.As(err, &r) || r.MismatchReport() == nil {
		t.Fatalf("Replay returned %v; want a miss carrying a mismatch report", err)
	}
	rep := r.MismatchReport()
	if rep.MatchPhase != models.MatchPhaseStrict {
		t.Fatalf("the miss's phase is %q (next steps %q); want %q, schema-noise strict's rejection", rep.MatchPhase, rep.NextSteps, models.MatchPhaseStrict)
	}
	if !strings.Contains(rep.NextSteps, "test.globalNoise.global.requestBody") || strings.Contains(rep.NextSteps, "test.globalNoise.requestBody") {
		t.Fatalf("the miss's next steps (%q) do not name keploy.yml's noise key, test.globalNoise.global.requestBody", rep.NextSteps)
	}
}
