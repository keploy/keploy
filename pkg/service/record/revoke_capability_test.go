package record

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// revokeAskInstr records the revoke capability the recording asks the
// agent's mock stream for.
type revokeAskInstr struct {
	*fakeInstr
	mu    sync.Mutex
	asked []bool
	// byRequest is what it asked for SupportsRevokeByRequest.
	byRequest []bool
}

func (d *revokeAskInstr) GetOutgoing(ctx context.Context, o models.OutgoingOptions) (<-chan *models.Mock, error) {
	d.mu.Lock()
	d.asked = append(d.asked, o.SupportsDroppedRevoke)
	d.byRequest = append(d.byRequest, o.SupportsRevokeByRequest)
	d.mu.Unlock()
	return d.fakeInstr.GetOutgoing(ctx, o)
}

// insertOnlyTestDB saves test cases and cannot delete one: the shape of an
// embedding whose store has no DeleteTests.
type insertOnlyTestDB struct{}

func (insertOnlyTestDB) GetAllTestSetIDs(context.Context) ([]string, error) { return nil, nil }
func (insertOnlyTestDB) InsertTestCase(context.Context, *models.TestCase, string, bool) error {
	return nil
}

// The recording tells the agent it can apply a revoke only when its store
// can delete a test case it saved: with a store that cannot, a revoked test
// case stays in the set, and the agent, told so, says the sent ones stay
// instead of that they were revoked.
func TestGetTestAndMockChans_AsksForRevokesOnlyWhenItsStoreDeletes(t *testing.T) {
	for _, tc := range []struct {
		name string
		db   TestDB
		want bool
	}{
		{"a store that deletes", &recTestDB{}, true},
		{"a store that cannot delete", insertOnlyTestDB{}, false},
		{"no store", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeInstr{mappings: make(chan models.TestMockMapping), incoming: make(chan *models.TestCase), outgoing: make(chan *models.Mock)}
			inst := &revokeAskInstr{fakeInstr: f}
			r := &Recorder{logger: zap.NewNop(), testDB: tc.db, instrumentation: inst, config: &config.Config{}, frameQuiet: time.Hour}
			g, gctx := errgroup.WithContext(context.Background())
			ctx, cancel := context.WithCancel(gctx)
			defer cancel()
			ctx = context.WithValue(ctx, models.ErrGroupKey, g)
			frames, err := r.GetTestAndMockChans(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer frames.Abandon()
			deadline := time.Now().Add(30 * time.Second)
			for {
				inst.mu.Lock()
				n := len(inst.asked)
				inst.mu.Unlock()
				if n > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the mock stream was never opened")
				}
				time.Sleep(time.Millisecond)
			}
			inst.mu.Lock()
			got, byReq := inst.asked, inst.byRequest
			inst.mu.Unlock()
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("the mock stream was asked SupportsDroppedRevoke %v, want [%v]", got, tc.want)
			}
			if len(byReq) != 1 || byReq[0] != tc.want {
				t.Errorf("the mock stream was asked SupportsRevokeByRequest %v, want [%v]: only a store that deletes finds a revoked test case by its request time", byReq, tc.want)
			}
			close(f.incoming)
			close(f.outgoing)
			close(f.mappings)
		})
	}
}
