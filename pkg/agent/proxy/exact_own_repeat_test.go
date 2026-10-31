package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/generic"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// ownRepeatExchange is a recorded opaque TCP exchange of req answered with
// resp, made at at.
func ownRepeatExchange(name, req, resp string, at time.Time) *models.Mock {
	return &models.Mock{Version: "api.keploy.io/v1beta1", Name: name, Kind: models.GENERIC, Spec: models.MockSpec{
		GenericRequests:  []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: models.String, Data: req}}}},
		GenericResponses: []models.Payload{{Origin: models.FromServer, Message: []models.OutputBinary{{Type: models.String, Data: resp}}}},
		ReqTimestampMock: at,
		ResTimestampMock: at.Add(time.Millisecond),
	}}
}

// genericCall sends req on a new connection through the generic parser and
// returns the four bytes it was answered with, or the miss's report when it
// was answered nothing.
func genericCall(t *testing.T, mm *MockManager, req string) (string, *models.MockMismatchReport) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callDeadline)
	defer cancel()
	ctx = context.WithValue(ctx, models.ClientConnectionIDKey, "1")
	ctx = context.WithValue(ctx, models.DestConnectionIDKey, "2")
	app, agentEnd := net.Pipe()
	defer app.Close()
	done := make(chan error, 1)
	go func() {
		err := generic.New(zap.NewNop()).MockOutgoing(ctx, agentEnd, &models.ConditionalDstCfg{}, mm, models.OutgoingOptions{})
		_ = agentEnd.Close()
		done <- err
	}()
	if _, err := io.WriteString(app, req); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(app, got); err == nil {
		_ = app.Close()
		<-done
		return string(got), nil
	}
	err := <-done
	var r interface {
		MismatchReport() *models.MockMismatchReport
	}
	if !errors.As(err, &r) || r.MismatchReport() == nil {
		t.Fatalf("%q was answered nothing, and its miss has no report: %v", req, err)
	}
	return "", r.MismatchReport()
}

// Under the proven switch, on a set served by the background rules, an
// exchange identical to one the open test's own recording already answered
// is one exchange more than the test recorded: the background never answers
// it, though the pool holds a recording of it (one the application made
// between tests). It misses as an extra call, and the pool's recording stays
// the background's: between tests it answers the exchange. Answered by the
// pool's copy, a test that sent a recorded write twice passed.
func TestTheBackgroundNeverAnswersAGenericExchangeOneMoreThanTheTestRecorded(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	now := time.Now()
	start := now.Add(time.Second)
	pool := ownRepeatExchange("mock-pool-ping", "PING", "POOL", now)
	// other is the pool's recording of an exchange the test never recorded:
	// it answers one.
	other := ownRepeatExchange("mock-pool-info", "INFO", "INF0", now.Add(time.Microsecond))
	own := ownRepeatExchange("mock-own-ping", "PING", "OWN1", start.Add(100*time.Millisecond))
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-0", Start: start, End: start.Add(900 * time.Millisecond)}})
	mm.SetMocksWithWindow([]*models.Mock{own}, []*models.Mock{pool, other}, models.BaseTime, now.Add(time.Hour))
	poolNames := []string{pool.Name, other.Name}
	mm.ServeExactBackground(models.RecordedWithS12Routing, poolNames, nil)
	mm.ServeExactPool(models.PoolKeyNone, poolNames)
	mm.ServeExactTest("test-0", []*models.Mock{roundMock(mm, own.Name)})
	if !mm.ExactServing() {
		t.Fatal("precondition: test-0 is not served exactly")
	}

	if got, miss := genericCall(t, mm, "PING"); got != "OWN1" {
		t.Fatalf("test-0's PING was answered %q (miss %+v); want its own OWN1", got, miss)
	}
	got, miss := genericCall(t, mm, "PING")
	if got != "" {
		t.Fatalf("test-0's second PING, one more than it recorded, was answered %q", got)
	}
	if miss.MissKind != models.MissExtraCall || miss.Recorded != 1 {
		t.Fatalf("the second PING missed as %q, recorded %d; want %q, recorded 1", miss.MissKind, miss.Recorded, models.MissExtraCall)
	}
	if got, miss := genericCall(t, mm, "INFO"); got != "INF0" {
		t.Fatalf("test-0's INFO, an exchange it never recorded, was answered %q (miss %+v); want the pool's INF0", got, miss)
	}

	mm.ServeExactPool(models.PoolKeyNone, poolNames)
	if got, miss := genericCall(t, mm, "PING"); got != "POOL" {
		t.Fatalf("the PING between tests was answered %q (miss %+v); want the pool's POOL", got, miss)
	}
}
