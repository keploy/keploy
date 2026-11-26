package proxy

import (
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type countingGobValue struct {
	Label string
	Count *atomic.Int64
}

func (c countingGobValue) GobEncode() ([]byte, error) {
	if c.Count != nil {
		c.Count.Add(1)
	}
	return []byte(c.Label), nil
}

func (c *countingGobValue) GobDecode(b []byte) error {
	c.Label = string(b)
	return nil
}

// TestGobRecords_SteadyStateEncodesOnce proves that once gob type definitions
// have been taught on the initial value, steady-state encodeLocked encodes each
// subsequent value exactly once (not 2x).
func TestGobRecords_SteadyStateEncodesOnce(t *testing.T) {
	g := newGobRecords()
	var cnt atomic.Int64
	var off int64
	store := func(b []byte) (int64, int, error) {
		cur := off
		off += int64(len(b))
		return cur, len(b), nil
	}

	// Warmup value teaches the type definition.
	if _, err := g.encodeLocked(&countingGobValue{Label: "warmup", Count: &cnt}, false, off, store); err != nil {
		t.Fatalf("warmup encodeLocked: %v", err)
	}

	cnt.Store(0)
	const steadyN = 50
	for i := 0; i < steadyN; i++ {
		if _, err := g.encodeLocked(&countingGobValue{Label: "steady", Count: &cnt}, false, off, store); err != nil {
			t.Fatalf("steady encodeLocked %d: %v", i, err)
		}
	}

	if got := cnt.Load(); got != steadyN {
		t.Fatalf("steady-state GobEncode calls = %d for %d values, want %d (1x encode, not 2x)", got, steadyN, steadyN)
	}
}

// TestDiskMocks_SteadyStateHTTPAndSpilledEncodeCounts verifies that DiskMocks
// performs 1x encode per steady-state HTTP mock (not 2x) and 2x encodes per
// steady-state spilled Mongo/HTTP mock (1x response + 1x mock, not 4x), while
// round-tripping every mock kind accurately across both resident and buffered
// disk-backed modes.
func TestDiskMocks_SteadyStateHTTPAndSpilledEncodeCounts(t *testing.T) {
	for _, budget := range []int64{0, residentBudgetBytes} {
		t.Run(fmt.Sprintf("budget=%d", budget), func(t *testing.T) {
			d, err := newDiskMocks(zap.NewNop(), budget)
			if err != nil {
				t.Fatalf("newDiskMocks: %v", err)
			}
			defer func() { _ = d.Close() }()

			base := time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)

			// Warmup one small HTTP mock so models.Mock type definitions are taught.
			warmHTTP := bigMockAt("warm-http", base, 256)
			if err := d.Add(warmHTTP); err != nil {
				t.Fatalf("Add warmHTTP: %v", err)
			}

			// Add 100 steady-state HTTP mocks -> must be exactly 100 encodes (1x each).
			const httpN = 100
			beforeHTTP := d.Encodes()
			httpMocks := make([]*models.Mock, httpN)
			for i := 0; i < httpN; i++ {
				m := bigMockAt(fmt.Sprintf("http-%03d", i), base.Add(time.Duration(i+1)*time.Second), 512)
				httpMocks[i] = m
				if err := d.Add(m); err != nil {
					t.Fatalf("Add http %d: %v", i, err)
				}
			}
			if got := d.Encodes() - beforeHTTP; got != httpN {
				t.Fatalf("steady-state HTTP encodes = %d for %d mocks, want %d (1x)", got, httpN, httpN)
			}

			// Warmup one spilled HTTP mock + one spilled Mongo mock.
			if err := d.Add(bigMockAt("warm-big-http", base.Add(200*time.Second), 2*responseSpillMinBytes)); err != nil {
				t.Fatalf("Add warm-big-http: %v", err)
			}
			if err := d.Add(mongoAnswerMock("warm-big-mongo", base.Add(201*time.Second).Unix(), 2*responseSpillMinBytes)); err != nil {
				t.Fatalf("Add warm-big-mongo: %v", err)
			}

			// Add 25 steady-state spilled Mongo mocks -> 2 encodes each (1x resp + 1x mock, NOT 4x).
			const mongoN = 25
			beforeMongo := d.Encodes()
			mongoMocks := make([]*models.Mock, mongoN)
			for i := 0; i < mongoN; i++ {
				m := mongoAnswerMock(fmt.Sprintf("mongo-%03d", i), base.Add(time.Duration(300+i)*time.Second).Unix(), 2*responseSpillMinBytes)
				mongoMocks[i] = m
				if err := d.Add(m); err != nil {
					t.Fatalf("Add mongo %d: %v", i, err)
				}
			}
			if got := d.Encodes() - beforeMongo; got != 2*mongoN {
				t.Fatalf("steady-state spilled Mongo encodes = %d for %d mocks, want %d (2x, not 4x)", got, mongoN, 2*mongoN)
			}

			// Warmup nested-interface MySQL mocks so StmtExecutePacket and map[string]interface{}
			// inside PacketBundle.Message -> Parameter.Value are taught.
			if err := d.Add(mysqlExecMock("warm-mysql-int", base.Add(400*time.Second).Unix(), int64(7))); err != nil {
				t.Fatalf("Add warm-mysql-int: %v", err)
			}
			if err := d.Add(mysqlExecMock("warm-mysql-map", base.Add(401*time.Second).Unix(), map[string]interface{}{"k": "v"})); err != nil {
				t.Fatalf("Add warm-mysql-map: %v", err)
			}

			// Add 25 steady-state nested-interface MySQL mocks -> must be exactly 25 encodes (1x each).
			const mysqlN = 25
			beforeMySQL := d.Encodes()
			mysqlMocks := make([]*models.Mock, mysqlN)
			for i := 0; i < mysqlN; i++ {
				m := mysqlExecMock(fmt.Sprintf("mysql-%03d", i), base.Add(time.Duration(410+i)*time.Second).Unix(), map[string]interface{}{"k": fmt.Sprintf("v-%d", i)})
				mysqlMocks[i] = m
				if err := d.Add(m); err != nil {
					t.Fatalf("Add mysql %d: %v", i, err)
				}
			}
			if got := d.Encodes() - beforeMySQL; got != mysqlN {
				t.Fatalf("steady-state nested-interface MySQL encodes = %d for %d mocks, want %d (1x)", got, mysqlN, mysqlN)
			}

			// Verify pre-Finalize read works cleanly even with buffered disk writes (budget=0).
			midRead, err := d.LoadByNames([]string{"http-099", "mongo-024", "mysql-024"})
			if err != nil {
				t.Fatalf("LoadByNames before Finalize: %v", err)
			}
			if len(midRead) != 3 {
				t.Fatalf("LoadByNames returned %d mocks, want 3", len(midRead))
			}

			if err := d.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}
			d.Finalize()

			// Round-trip check HTTP, spilled Mongo, and nested-interface MySQL mocks against a fresh decode.
			checkMocks := append(append(append([]*models.Mock(nil), httpMocks[:5]...), mongoMocks[:5]...), mysqlMocks[:5]...)
			for _, m := range checkMocks {
				got := servedRead(t, d, m.Name)
				want := freshAsStored(t, d, m)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("mock %s round-trip mismatch", m.Name)
				}
			}
		})
	}
}
