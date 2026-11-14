package mapdb

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
)

// meetCtx is a writer's context whose first Done() waits until the other
// writer's context has been asked too, or the other writer has returned. A
// write of a set's mapping file asks its context first while it reads the
// file it opened (the yaml package's ctxReader), so two writers that meet
// there have both read the file before either writes it back. A writer that
// waits for the set's write lock asks its context while it waits, so with the
// lock the one that holds it goes on, and the other reads what it wrote.
// A lock whose wait never asks the context deadlocks here: go test's
// -timeout ends it.
type meetCtx struct {
	context.Context
	once     sync.Once
	asked    chan struct{} // closed at the first Done()
	returned chan struct{} // closed when the writer returned
	other    *meetCtx
}

func meetPair() (*meetCtx, *meetCtx) {
	a := &meetCtx{Context: context.Background(), asked: make(chan struct{}), returned: make(chan struct{})}
	b := &meetCtx{Context: context.Background(), asked: make(chan struct{}), returned: make(chan struct{})}
	a.other, b.other = b, a
	return a, b
}

func (c *meetCtx) Done() <-chan struct{} {
	c.once.Do(func() {
		close(c.asked)
		select {
		case <-c.other.asked:
		case <-c.other.returned:
		}
	})
	return c.Context.Done()
}

// pausedCtx is a writer's context whose first Done() waits for release: the
// writer has opened the file and stops there.
type pausedCtx struct {
	context.Context
	once    sync.Once
	asked   chan struct{} // closed at the first Done()
	release chan struct{}
}

func (c *pausedCtx) Done() <-chan struct{} {
	c.once.Do(func() {
		close(c.asked)
		<-c.release
	})
	return c.Context.Done()
}

// mapWriter is one of MappingDb's writes of a set's mapping file, and how to
// tell its change is in the file.
type mapWriter struct {
	name  string
	write func(ctx context.Context, db *MappingDb) error
	wrote func(m *models.Mapping) bool
}

func listed(entries []models.MockEntry, name string) bool {
	return slices.ContainsFunc(entries, func(e models.MockEntry) bool { return e.Name == name })
}

func lockedEntry(m *models.Mapping, id string) *models.MappedTestCase {
	for i := range m.TestCases {
		if m.TestCases[i].ID == id {
			return &m.TestCases[i]
		}
	}
	return nil
}

func hasTestMock(id, mock string) func(m *models.Mapping) bool {
	return func(m *models.Mapping) bool {
		tc := lockedEntry(m, id)
		return tc != nil && listed(tc.Mocks, mock)
	}
}

const lockedSetID = "test-set-0"

// recorderWrite is keploy record's write of a batch of tests' entries: the
// writer every other one meets in a recording that goes on.
var recorderWrite = mapWriter{
	name: "UpsertBatch (the recorder)",
	write: func(ctx context.Context, db *MappingDb) error {
		return db.UpsertBatch(ctx, lockedSetID, map[string][]models.MockEntry{"rec": {{Name: "m-rec", Kind: "Http"}}})
	},
	wrote: hasTestMock("rec", "m-rec"),
}

// mapWriters is every MappingDb method that writes a set's mapping file but
// Delete (TestADeleteAndAWriteOfOneSetAreOneAfterTheOther), each with a
// change no other write of the pair undoes.
var mapWriters = []mapWriter{
	{name: "UpsertBatch", write: func(ctx context.Context, db *MappingDb) error {
		return db.UpsertBatch(ctx, lockedSetID, map[string][]models.MockEntry{"x": {{Name: "m-x", Kind: "Http"}}})
	}, wrote: hasTestMock("x", "m-x")},
	{name: "Upsert", write: func(ctx context.Context, db *MappingDb) error {
		return db.Upsert(ctx, lockedSetID, "y", []models.MockEntry{{Name: "m-y", Kind: "Http"}})
	}, wrote: hasTestMock("y", "m-y")},
	{name: "Insert", write: func(ctx context.Context, db *MappingDb) error {
		return db.Insert(ctx, &models.Mapping{TestSetID: lockedSetID, TestCases: []models.MappedTestCase{{ID: "z", Mocks: []models.MockEntry{{Name: "m-z", Kind: "Http"}}}}})
	}, wrote: hasTestMock("z", "m-z")},
	{name: "UpsertWorker", write: func(ctx context.Context, db *MappingDb) error {
		return db.UpsertWorker(ctx, lockedSetID, []models.MockEntry{{Name: "w-1", Kind: "Http"}})
	}, wrote: func(m *models.Mapping) bool { return listed(m.Worker, "w-1") }},
	{name: "AddToWorker", write: func(ctx context.Context, db *MappingDb) error {
		_, err := db.AddToWorker(ctx, lockedSetID, []models.MockEntry{{Name: "w-2", Kind: "Http", Reason: models.WorkerRuleReason("jwks")}})
		return err
	}, wrote: func(m *models.Mapping) bool { return listed(m.Worker, "w-2") }},
	{name: "MoveToWorker", write: func(ctx context.Context, db *MappingDb) error {
		_, err := db.MoveToWorker(ctx, lockedSetID, []models.MockEntry{{Name: "m-a"}})
		return err
	}, wrote: func(m *models.Mapping) bool { return listed(m.Worker, "m-a") && !hasTestMock("a", "m-a")(m) }},
	{name: "MoveToWorkerFromStartup", write: func(ctx context.Context, db *MappingDb) error {
		_, err := db.MoveToWorkerFromStartup(ctx, lockedSetID, []models.MockEntry{{Name: "st-1"}})
		return err
	}, wrote: func(m *models.Mapping) bool { return listed(m.Worker, "st-1") && !listed(m.Startup, "st-1") }},
	{name: "MarkRecordedWith", write: func(ctx context.Context, db *MappingDb) error {
		return db.MarkRecordedWith(ctx, lockedSetID, "feature-x")
	}, wrote: func(m *models.Mapping) bool { return m.WasRecordedWith("feature-x") }},
	{name: "SetRules", write: func(ctx context.Context, db *MappingDb) error {
		return db.SetRules(ctx, lockedSetID, "digest-1")
	}, wrote: func(m *models.Mapping) bool { return m.Rules == "digest-1" }},
	{name: "SetSources", write: func(ctx context.Context, db *MappingDb) error {
		return db.SetSources(ctx, lockedSetID, map[string][]models.DeclaredSource{"m-a": {lockedSource}})
	}, wrote: func(m *models.Mapping) bool {
		tc := lockedEntry(m, "a")
		return tc != nil && len(tc.Mocks) == 1 && reflect.DeepEqual(tc.Mocks[0].Sources, []models.DeclaredSource{lockedSource})
	}},
	{name: "UpsertBoots", write: func(ctx context.Context, db *MappingDb) error {
		return db.UpsertBoots(ctx, lockedSetID, []models.BootSpec{{Key: "app#1", Program: "app", N: 1}})
	}, wrote: func(m *models.Mapping) bool { return len(m.Boots) == 1 && m.Boots[0].Key == "app#1" }},
	{name: "UpsertCases", write: func(ctx context.Context, db *MappingDb) error {
		return db.UpsertCases(ctx, lockedSetID, map[string]models.MappedTestCase{"a": {Cases: []string{"case-1"}}}, nil, nil)
	}, wrote: func(m *models.Mapping) bool {
		tc := lockedEntry(m, "a")
		return tc != nil && slices.Contains(tc.Cases, "case-1")
	}},
	{name: "UpsertConcurrency", write: func(ctx context.Context, db *MappingDb) error {
		return db.UpsertConcurrency(ctx, lockedSetID, []models.ConcurrencyGroup{{ID: "g9", Tests: []string{"a"}}}, nil)
	}, wrote: func(m *models.Mapping) bool { return len(m.Groups) == 1 && m.Groups[0].ID == "g9" }},
	{name: "ApplyShares", write: func(ctx context.Context, db *MappingDb) error {
		_, err := db.ApplyShares(ctx, lockedSetID, models.SetShares{State: models.SharesComplete, Pins: map[string]string{"s-1": "a"}})
		return err
	}, wrote: func(m *models.Mapping) bool { return hasTestMock("a", "s-1")(m) && len(m.Shared) == 0 }},
}

var lockedSource = models.DeclaredSource{Rule: "id@44aa55bb", Kind: models.DeclaredSourceMinted, Field: "body.docs.msgs.0._id", Minted: &models.MintedForm{MinLen: 36, MaxLen: 36}}

// seedSet writes the set every pair starts from: test a with its mock m-a,
// a start-up call st-1 no test lists, which MoveToWorkerFromStartup moves,
// and a call s-1 shared in group g1, which ApplyShares pins to a.
func seedSet(t *testing.T, dir string) {
	t.Helper()
	ctx := context.Background()
	db := New(zap.NewNop(), dir, "mappings")
	if err := db.UpsertBatch(ctx, lockedSetID, map[string][]models.MockEntry{"a": {{Name: "m-a", Kind: "Http"}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertCases(ctx, lockedSetID, nil, []models.MockEntry{{Name: "st-1", Kind: "Http"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertConcurrency(ctx, lockedSetID, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a"}}}, []models.MockEntry{{Name: "s-1", Kind: "Http", Group: "g1"}}); err != nil {
		t.Fatal(err)
	}
}

// runMet runs first, then second once first has asked its context, each
// through its own MappingDb (as keploy record, a replay round and the
// auto-replay hooks each build their own over one directory), and returns
// their errors.
func runMet(first, second func(ctx context.Context) error) (error, error) {
	c1, c2 := meetPair()
	var e1, e2 error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer close(c1.returned)
		e1 = first(c1)
	}()
	go func() {
		defer wg.Done()
		defer close(c2.returned)
		select {
		case <-c1.asked:
		case <-c1.returned:
		}
		e2 = second(c2)
	}()
	wg.Wait()
	return e1, e2
}

// Two writes of one set's mapping file that overlap each read the file
// before either writes it back: without one writer at a time the second to
// write puts back a file without the first one's change. Each pair here is
// the recorder's write of a test's entry and another write, in both orders,
// through two MappingDbs over one directory, and both changes must be in the
// file at the end.
func TestTwoWritesOfOneSetLoseNeitherChange(t *testing.T) {
	for _, w := range mapWriters {
		for _, order := range []string{"recorder first", "recorder second"} {
			t.Run(w.name+"/"+order, func(t *testing.T) {
				dir := t.TempDir()
				seedSet(t, dir)
				recorder, other := New(zap.NewNop(), dir, "mappings"), New(zap.NewNop(), dir, "mappings")
				rec := func(ctx context.Context) error { return recorderWrite.write(ctx, recorder) }
				wr := func(ctx context.Context) error { return w.write(ctx, other) }
				var errRec, errW error
				if order == "recorder first" {
					errRec, errW = runMet(rec, wr)
				} else {
					errW, errRec = runMet(wr, rec)
				}
				if errRec != nil || errW != nil {
					t.Fatalf("recorder: %v, %s: %v", errRec, w.name, errW)
				}
				m, err := New(zap.NewNop(), dir, "mappings").GetMapping(context.Background(), lockedSetID)
				if err != nil || m == nil {
					t.Fatalf("GetMapping = %v, %v", m, err)
				}
				if !recorderWrite.wrote(m) {
					t.Errorf("the recorder's entry for rec is lost: %s wrote over it", w.name)
				}
				if !w.wrote(m) {
					t.Errorf("%s's change is lost: the recorder wrote over it", w.name)
				}
			})
		}
	}
}

// One set's writes are one at a time whatever path each MappingDb was given
// for the directory: here one names it relative to the working directory.
func TestTwoWritesOfOneSetNamedTwoWaysLoseNeitherChange(t *testing.T) {
	dir := t.TempDir()
	seedSet(t, dir)
	t.Chdir(filepath.Dir(dir))
	abs := New(zap.NewNop(), dir, "mappings")
	rel := New(zap.NewNop(), filepath.Base(dir)+string(filepath.Separator), "mappings")
	w := mapWriters[slices.IndexFunc(mapWriters, func(w mapWriter) bool { return w.name == "AddToWorker" })]
	errRec, errW := runMet(
		func(ctx context.Context) error { return recorderWrite.write(ctx, abs) },
		func(ctx context.Context) error { return w.write(ctx, rel) })
	if errRec != nil || errW != nil {
		t.Fatalf("recorder: %v, %s: %v", errRec, w.name, errW)
	}
	m, err := abs.GetMapping(context.Background(), lockedSetID)
	if err != nil || m == nil {
		t.Fatalf("GetMapping = %v, %v", m, err)
	}
	if !recorderWrite.wrote(m) || !w.wrote(m) {
		t.Fatalf("a change is lost: the recorder's %v, %s's %v", recorderWrite.wrote(m), w.name, w.wrote(m))
	}
}

// A delete and a write of one set are one after the other: the file is
// either gone (the write, then the delete) or holds the write alone (the
// delete, then the write). Without one writer at a time the write puts back
// the file it read before the delete, every entry of the deleted set in it.
func TestADeleteAndAWriteOfOneSetAreOneAfterTheOther(t *testing.T) {
	dir := t.TempDir()
	seedSet(t, dir)
	recorder, deleter := New(zap.NewNop(), dir, "mappings"), New(zap.NewNop(), dir, "mappings")
	errRec, errDel := runMet(
		func(ctx context.Context) error { return recorderWrite.write(ctx, recorder) },
		func(ctx context.Context) error { return deleter.Delete(ctx, lockedSetID) })
	if errRec != nil || errDel != nil {
		t.Fatalf("recorder: %v, Delete: %v", errRec, errDel)
	}
	m, err := New(zap.NewNop(), dir, "mappings").GetMapping(context.Background(), lockedSetID)
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		var ids []string
		for _, tc := range m.TestCases {
			ids = append(ids, tc.ID)
		}
		if !reflect.DeepEqual(ids, []string{"rec"}) {
			t.Fatalf("after the delete the file holds tests %v; want none, or rec alone", ids)
		}
	}
}

// A write that waits for the set's lock while another holds it stops
// waiting when its context ends, and writes nothing.
func TestAWriteWaitingForTheSetsLockStopsWhenItsContextEnds(t *testing.T) {
	dir := t.TempDir()
	seedSet(t, dir)
	holder, waiter := New(zap.NewNop(), dir, "mappings"), New(zap.NewNop(), dir, "mappings")
	held := &pausedCtx{Context: context.Background(), asked: make(chan struct{}), release: make(chan struct{})}
	holderErr := make(chan error, 1)
	go func() { holderErr <- recorderWrite.write(held, holder) }()
	<-held.asked // the holder has read the file and waits for release
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := mapWriters[slices.IndexFunc(mapWriters, func(w mapWriter) bool { return w.name == "AddToWorker" })]
	waited := make(chan error, 1)
	go func() { waited <- w.write(ctx, waiter) }()
	select {
	case err := <-waited:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the waiting write returned %v, want context.Canceled", err)
		}
	case <-time.After(30 * time.Second):
		t.Error("a write whose context ended still waits for the set's lock")
	}
	close(held.release)
	if err := <-holderErr; err != nil {
		t.Fatal(err)
	}
	m, err := holder.GetMapping(context.Background(), lockedSetID)
	if err != nil || m == nil {
		t.Fatalf("GetMapping = %v, %v", m, err)
	}
	if !recorderWrite.wrote(m) || w.wrote(m) {
		t.Fatalf("the holder's change %v (want it), the cancelled write's %v (want none)", recorderWrite.wrote(m), w.wrote(m))
	}
}

// Every MappingDb method is a read or one of the writes above, so a write
// added to MappingDb fails here until it takes the set's lock and has its row.
func TestEveryWriteOfASetsMappingFileIsInTheLockRows(t *testing.T) {
	reads := map[string]bool{"Exists": true, "GetSource": true, "GetMapping": true, "GetCases": true, "GetStartup": true, "Get": true}
	rows := map[string]bool{"Delete": true}
	for _, w := range mapWriters {
		rows[w.name] = true
	}
	typ := reflect.TypeOf(&MappingDb{})
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		if !reads[name] && !rows[name] {
			t.Errorf("MappingDb.%s is neither a read nor in mapWriters: a write of the mapping file must take the set's lock (lockedSetID) and have its row", name)
		}
	}
}
