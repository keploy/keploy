package supervisor

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A parser that stamps a mock answered before it was asked (its request time
// taken from its own clock, behind the wire, while the answer's is the wire's)
// records a mock the replay leaves out. The recording says so as it emits it,
// for every kind, naming the mock, its kind and both times, so the parser's
// bug is found at record time instead of as a miss at replay. A run of them is
// said once per kind per interval, with how many it stands for.
func TestAMockAParserStampedAnsweredBeforeItWasAskedIsSaidAtRecord(t *testing.T) {
	answeredBeforeAskedWarns.Reset()
	core, logs := observer.New(zapcore.InfoLevel)
	ch := make(chan *models.Mock, 4)
	sess := &Session{Mocks: ch, Ctx: context.Background(), Logger: zap.New(core), ClientConnID: "conn-4"}
	asked := time.Unix(1_700_000_000, 64_475_410).UTC()
	answered := asked.Add(-37 * time.Microsecond)
	for i := 0; i < 2; i++ {
		if err := sess.EmitMock(&models.Mock{Name: "PostgresV3Query", Kind: models.PostgresV3,
			Spec: models.MockSpec{ReqTimestampMock: asked.Add(time.Duration(i) * time.Millisecond), ResTimestampMock: answered.Add(time.Duration(i) * time.Millisecond)}}); err != nil {
			t.Fatalf("EmitMock: %v", err)
		}
	}
	if err := sess.EmitMock(&models.Mock{Name: "PostgresV3Query", Kind: models.PostgresV3,
		Spec: models.MockSpec{ReqTimestampMock: asked.Add(time.Second), ResTimestampMock: asked.Add(time.Second + time.Millisecond)}}); err != nil {
		t.Fatalf("EmitMock: %v", err)
	}
	if got := len(ch); got != 3 {
		t.Fatalf("%d mocks recorded; want all 3: the mock is recorded as it was stamped, and said of", got)
	}
	warns := logs.FilterLevelExact(zapcore.WarnLevel).All()
	if len(warns) != 1 {
		t.Fatalf("the recording warned %d times; want once for the run of two: %v", len(warns), warns)
	}
	f := warns[0].ContextMap()
	if f["mockName"] != "PostgresV3Query" || f["kind"] != string(models.PostgresV3) || f["connID"] != "conn-4" {
		t.Errorf("the warning names mock %v of kind %v on %v; want PostgresV3Query of kind %s on conn-4", f["mockName"], f["kind"], f["connID"], models.PostgresV3)
	}
	if req, ok := f["reqTimestampMock"].(time.Time); !ok || !req.Equal(asked) {
		t.Errorf("the warning says it was asked at %v; want %v", f["reqTimestampMock"], asked)
	}
	if res, ok := f["resTimestampMock"].(time.Time); !ok || !res.Equal(answered) {
		t.Errorf("the warning says it was answered at %v; want %v", f["resTimestampMock"], answered)
	}
}
