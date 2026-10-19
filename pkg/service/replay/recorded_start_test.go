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

// A set's startup cutoff is the earliest test the set ever recorded,
// not the earliest of the test cases this run holds.
func TestSetShapeStartsAtTheSetsFirstRecordedTest(t *testing.T) {
	held := heldTestCases()
	r := &Replayer{logger: zap.NewNop(), testDB: &recordedStartDB{prTestDB: prTestDB{cases: held}, at: recordedT0}}
	got := r.recordedSetShape(context.Background(), "test-set-0", held)
	if !got.firstTestStart.Equal(recordedT0) {
		t.Fatalf("the set's cutoff is %v; want the set's first recorded test %v", got.firstTestStart, recordedT0)
	}
	if want := recordedSetShapeOf(held).windows; len(got.windows) != len(want) {
		t.Fatalf("the set's begin sends %d recorded windows; want the held tests' %d", len(got.windows), len(want))
	}
}

// Without a recorded start earlier than the held tests, the first held test is
// the cutoff, as before.
func TestSetShapeKeepsTheFirstHeldTestOtherwise(t *testing.T) {
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
				t.Fatalf("the set's cutoff is %v; want the first held test %v", got, first)
			}
		})
	}
}

// A set's first recorded test is the earliest window any of its tests opens,
// gRPC tests' included. It was read from the HTTP request alone, so a gRPC set
// began with no first test: the agent read every mock of the recording back
// from disk to begin it.
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

// The cutoff is the earliest recorded test of any kind that opens a window,
// whatever order the cases come in: a set of gRPC tests has one (its staging
// read the whole recording when only an HTTP case's request time was read),
// and a set whose first test is a gRPC one cuts there, not at its first HTTP
// test.
func TestTheStagingCutoffIsTheEarliestTestOfAnyKind(t *testing.T) {
	grpcAt := func(at time.Time) *models.TestCase {
		return &models.TestCase{Kind: models.GRPC_EXPORT, GrpcReq: models.GrpcReq{Timestamp: at}}
	}
	httpAt := func(at time.Time) *models.TestCase {
		return &models.TestCase{Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: at}}
	}
	for name, c := range map[string]struct {
		cases []*models.TestCase
		want  time.Time
	}{
		"gRPC only":              {[]*models.TestCase{grpcAt(recordedT0.Add(time.Minute)), grpcAt(recordedT0)}, recordedT0},
		"gRPC first, then HTTP":  {[]*models.TestCase{httpAt(recordedT0.Add(time.Minute)), grpcAt(recordedT0)}, recordedT0},
		"HTTP out of order":      {[]*models.TestCase{httpAt(recordedT0.Add(time.Minute)), httpAt(recordedT0)}, recordedT0},
		"a case of neither kind": {[]*models.TestCase{testCaseAt(recordedT0)}, recordedT0},
		"no request time":        {[]*models.TestCase{httpAt(time.Time{}), nil}, time.Time{}},
	} {
		if got := firstRecordedTestStart(c.cases); !got.Equal(c.want) {
			t.Errorf("%s: cutoff %v, want %v", name, got, c.want)
		}
	}
}
