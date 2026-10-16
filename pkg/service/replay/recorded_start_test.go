package replay

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// recordedStartDB is a TestDB that also knows when its set's first test was
// recorded (RecordedStartReader).
type recordedStartDB struct {
	prTestDB
	at  time.Time
	err error
}

func (f *recordedStartDB) RecordedFirstTestStart(context.Context, string) (time.Time, error) {
	return f.at, f.err
}

func testCaseAt(at time.Time) *models.TestCase {
	return &models.TestCase{Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: at}, HTTPResp: models.HTTPResp{Timestamp: at.Add(time.Second)}}
}

var recordedT0 = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

// held is what a later auto-replay round hands the replayer: only the tests
// recorded since the previous round, while the set's first test, at recordedT0,
// was written back and deleted.
func heldTestCases() []*models.TestCase {
	return []*models.TestCase{testCaseAt(recordedT0.Add(9 * time.Minute)), testCaseAt(recordedT0.Add(10 * time.Minute))}
}

// A staging call's startup cutoff is the earliest test the set ever recorded,
// not the earliest of the test cases this run holds.
func TestStagingShapeStartsAtTheSetsFirstRecordedTest(t *testing.T) {
	held := heldTestCases()
	r := &Replayer{logger: zap.NewNop(), testDB: &recordedStartDB{prTestDB: prTestDB{cases: held}, at: recordedT0}}
	got := r.recordedSetShape(context.Background(), "test-set-0", held)
	if !got.firstTestStart.Equal(recordedT0) {
		t.Fatalf("the staging cutoff is %v; want the set's first recorded test %v", got.firstTestStart, recordedT0)
	}
	if want := recordedSetShapeOf(held).windows; len(got.windows) != len(want) {
		t.Fatalf("the staging call sends %d recorded windows; want the held tests' %d", len(got.windows), len(want))
	}
}

// Without a recorded start earlier than the held tests, the first held test is
// the cutoff, as before.
func TestStagingShapeKeepsTheFirstHeldTestOtherwise(t *testing.T) {
	held := heldTestCases()
	first := held[0].HTTPReq.Timestamp
	for name, db := range map[string]TestDB{
		"no RecordedStartReader": &prTestDB{cases: held},
		"zero recorded start":    &recordedStartDB{prTestDB: prTestDB{cases: held}},
		"later recorded start":   &recordedStartDB{prTestDB: prTestDB{cases: held}, at: first.Add(time.Minute)},
		"recorded start error":   &recordedStartDB{prTestDB: prTestDB{cases: held}, at: recordedT0, err: errors.New("unreadable")},
	} {
		t.Run(name, func(t *testing.T) {
			r := &Replayer{logger: zap.NewNop(), testDB: db}
			if got := r.recordedSetShape(context.Background(), "test-set-0", held).firstTestStart; !got.Equal(first) {
				t.Fatalf("the staging cutoff is %v; want the first held test %v", got, first)
			}
		})
	}
}

// A set's first recorded test is the earliest window any of its tests opens,
// gRPC tests' included. It was read from the HTTP request alone, so a gRPC set
// was staged with no first test: the agent read every mock of the recording
// back from disk to stage it.
func TestASetsFirstRecordedTestIsItsEarliestWindowOfAnyKind(t *testing.T) {
	grpcAt := func(name string, at time.Time) *models.TestCase {
		return &models.TestCase{Name: name, Kind: models.GRPC_EXPORT, GrpcReq: models.GrpcReq{Timestamp: at}, GrpcResp: models.GrpcResp{Timestamp: at.Add(time.Second)}}
	}
	for name, tc := range map[string]struct {
		cases []*models.TestCase
		want  time.Time
	}{
		"gRPC only":              {[]*models.TestCase{grpcAt("g-1", recordedT0), grpcAt("g-2", recordedT0.Add(time.Minute))}, recordedT0},
		"gRPC before HTTP":       {[]*models.TestCase{testCaseAt(recordedT0.Add(time.Minute)), grpcAt("g-1", recordedT0)}, recordedT0},
		"HTTP before gRPC":       {[]*models.TestCase{grpcAt("g-1", recordedT0.Add(time.Minute)), testCaseAt(recordedT0)}, recordedT0},
		"none carries a request": {[]*models.TestCase{{Name: "untimed", Kind: models.HTTP}}, time.Time{}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := recordedSetShapeOf(tc.cases).firstTestStart; !got.Equal(tc.want) {
				t.Fatalf("the set's first recorded test is %v; want %v", got, tc.want)
			}
		})
	}
}
