package proxy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// setEndManager is the run's manager in a session whose client reads each
// set's end (SetReadsSetEnds).
func setEndManager(t *testing.T) *MockManager {
	m := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	t.Cleanup(m.Close)
	m.SetReadsSetEnds(true)
	return m
}

func mustRegister(t *testing.T, db integrations.MockMemDb, file func(context.Context)) func() {
	t.Helper()
	return mustRegisterKind(t, db, models.KAFKA, file)
}

func mustRegisterKind(t *testing.T, db integrations.MockMemDb, kind models.Kind, file func(context.Context)) func() {
	t.Helper()
	f, ok := db.(integrations.SetEndFiler)
	if !ok {
		t.Fatalf("%T is not a SetEndFiler", db)
	}
	cancel, ok := f.AtSetEnd(kind, file)
	if !ok {
		t.Fatalf("%T refused a set-end registration", db)
	}
	return cancel
}

// Each registration runs once per set-end read, in the order made, with the
// read's context, until it is cancelled. One cancelled before its turn in a
// read (by another registration's run) does not run in it; one made during a
// read runs from the next; a cancel called twice does nothing more; a
// registration may cancel itself while it runs.
func TestMockManager_AtSetEndRunsEachRegistrationOncePerSetEndUntilCancelled(t *testing.T) {
	m := setEndManager(t)
	var ran []string
	type key struct{}
	read := context.WithValue(context.Background(), key{}, "read")
	var cancelC, cancelD, cancelE func()
	cancelA := mustRegister(t, m, func(ctx context.Context) {
		if ctx.Value(key{}) != "read" {
			t.Error("a registration ran without the read's context")
		}
		ran = append(ran, "a")
		if cancelD == nil {
			cancelD = mustRegister(t, m, func(context.Context) { ran = append(ran, "d") })
		}
	})
	cancelB := mustRegister(t, m, func(context.Context) { ran = append(ran, "b"); cancelC() })
	cancelC = mustRegister(t, m, func(context.Context) { ran = append(ran, "c") })
	cancelE = mustRegister(t, m, func(context.Context) { ran = append(ran, "e"); cancelE() })
	for i := 0; i < 20; i++ {
		mustRegister(t, m, func(context.Context) {})() // registered and cancelled: never run
	}

	if err := m.FileAtSetEnd(read); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b", "e"}; !slices.Equal(ran, want) {
		t.Fatalf("first set end ran %v, want %v (c cancelled by b before its turn, d made during the read)", ran, want)
	}
	ran = nil
	cancelA()
	cancelA()
	if err := m.FileAtSetEnd(read); err != nil {
		t.Fatal(err)
	}
	if want := []string{"b", "d"}; !slices.Equal(ran, want) {
		t.Fatalf("second set end ran %v, want %v", ran, want)
	}
	ran = nil
	cancelB()
	cancelD()
	if err := m.FileAtSetEnd(read); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 0 {
		t.Fatalf("with every registration cancelled the set end ran %v", ran)
	}
}

// A read runs the registrations it found when it began, though one of them
// cancels another that already ran: the cancel moves no later one out of the
// read's turn.
func TestMockManager_FileAtSetEndRunsWhatItFoundThoughOneCancelsAnEarlierOne(t *testing.T) {
	m := setEndManager(t)
	var ran []string
	cancelA := mustRegister(t, m, func(context.Context) { ran = append(ran, "a") })
	mustRegister(t, m, func(context.Context) { ran = append(ran, "b"); cancelA() })
	mustRegister(t, m, func(context.Context) { ran = append(ran, "c") })
	if err := m.FileAtSetEnd(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b", "c"}; !slices.Equal(ran, want) {
		t.Fatalf("ran %v, want %v", ran, want)
	}
}

// A test worker's manager, which no set's end read asks, registers nothing
// and says so: a parser on it must not hold a call past its own window.
func TestMockManager_AWorkersManagerRefusesSetEndRegistrations(t *testing.T) {
	p := &Proxy{logger: zap.NewNop()}
	p.mockManager = setEndManager(t)
	if _, err := p.ServeInstance(7); err != nil {
		t.Fatal(err)
	}
	wm := p.instances.mgrs[7]
	t.Cleanup(wm.Close)
	// Refused on its own account, even were its session's word handed on.
	wm.SetReadsSetEnds(true)
	ran := false
	if _, ok := wm.AtSetEnd(models.KAFKA, func(context.Context) { ran = true }); ok {
		t.Fatal("a worker's manager took a set-end registration no read will run")
	}
	if err := wm.FileAtSetEnd(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("a worker's manager ran a registration it refused")
	}
	if _, ok := setEndManager(t).AtSetEnd(models.KAFKA, func(context.Context) {}); !ok {
		t.Fatal("the run's manager refused a registration")
	}
}

// A scoped store hands on the SetEndFiler of the store it wraps: over the
// run's manager its registration runs at the run's set end; over a test
// worker's manager, or a store without the facet, it is refused.
func TestScopedMockDb_HandsOnTheWrappedSetEndFiler(t *testing.T) {
	run := setEndManager(t)
	n := 0
	cancel := mustRegister(t, &scopedMockDb{MockMemDb: run}, func(context.Context) { n++ })
	if err := run.FileAtSetEnd(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("a scoped registration ran %d times at the run's set end, want 1", n)
	}
	cancel()
	if err := run.FileAtSetEnd(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("a cancelled scoped registration ran")
	}

	// It hands on the kind it is handed: the run's read names that parser.
	named := setEndManager(t)
	kinds := []models.Kind{models.REDIS, models.MySQL}
	for _, kind := range kinds {
		mustRegisterKind(t, &scopedMockDb{MockMemDb: named}, kind, func(context.Context) { panic("parser bug") })
	}
	err := named.FileAtSetEnd(context.Background())
	for _, kind := range kinds {
		if err == nil || !strings.Contains(err.Error(), string(kind)+": parser bug") {
			t.Fatalf("a %s registration made through a scoped store panicked: FileAtSetEnd said %v, want an error naming %s", kind, err, kind)
		}
	}

	worker := setEndManager(t)
	worker.notAtSetEnd.Store(true)
	if _, ok := (&scopedMockDb{MockMemDb: worker}).AtSetEnd(models.KAFKA, func(context.Context) {}); ok {
		t.Fatal("a scoped store over a worker's manager took a set-end registration")
	}
	bare := struct{ integrations.MockMemDb }{run}
	if _, ok := (&scopedMockDb{MockMemDb: bare}).AtSetEnd(models.KAFKA, func(context.Context) {}); ok {
		t.Fatal("a scoped store over a store without the facet took a set-end registration")
	}
}

// The proxy asks the run's manager, with the read's context, and says what
// it says; with none it does nothing.
func TestProxy_FileAtSetEndAsksTheRunsManager(t *testing.T) {
	type key struct{}
	read := context.WithValue(context.Background(), key{}, "read")
	p := &Proxy{}
	if err := p.FileAtSetEnd(read); err != nil {
		t.Fatalf("with no manager: %v", err)
	}
	m := setEndManager(t)
	p.mockManager = m
	n := 0
	mustRegister(t, m, func(ctx context.Context) {
		if ctx.Value(key{}) != "read" {
			t.Error("the run's manager's registration ran without the read's context")
		}
		n++
	})
	if err := p.FileAtSetEnd(read); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the run's manager's registration ran %d times, want 1", n)
	}
	mustRegister(t, m, func(context.Context) { panic("parser bug") })
	if err := p.FileAtSetEnd(read); err == nil || !strings.Contains(err.Error(), "parser bug") {
		t.Fatalf("a registration panicked: the proxy said %v, want the manager's error naming it", err)
	}
}

// A registration is taken only where a read of the set's end will run it:
// in a session whose client reads each set's end (Proxy.Mock hands the run's
// manager the session's word, models.OutgoingOptions.ReadsSetEnds), on an
// open manager, for a file. A registration a session took stays when the
// next session's client makes no such read; that session takes none.
func TestMockManager_TakesASetEndRegistrationOnlyWhereAReadRunsIt(t *testing.T) {
	p := New(zap.NewNop(), nil, &config.Config{})
	p.nsswitchData = []byte("preseeded-by-test") // Mock leaves the host's DNS alone
	takes := func() bool {
		_, ok := p.getMockManager().AtSetEnd(models.KAFKA, func(context.Context) {})
		return ok
	}
	if err := p.Mock(context.Background(), models.OutgoingOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.getMockManager().Close)
	if takes() {
		t.Fatal("a session whose client makes no set's end read took a registration")
	}
	if err := p.Mock(context.Background(), models.OutgoingOptions{ReadsSetEnds: true}); err != nil {
		t.Fatal(err)
	}
	n := 0
	mustRegister(t, p.getMockManager(), func(context.Context) { n++ })
	if err := p.Mock(context.Background(), models.OutgoingOptions{}); err != nil {
		t.Fatal(err)
	}
	if takes() {
		t.Fatal("a later session whose client makes no set's end read took a registration")
	}
	if err := p.FileAtSetEnd(context.Background()); err != nil || n != 1 {
		t.Fatalf("the earlier session's registration ran %d times (%v), want once", n, err)
	}

	m := setEndManager(t)
	if _, ok := m.AtSetEnd(models.KAFKA, nil); ok {
		t.Fatal("a nil file was registered")
	}
	if _, ok := m.AtSetEnd("", func(context.Context) { t.Error("a registration with no kind ran") }); ok {
		t.Fatal("a registration with no kind, which the log could not name, was taken")
	}
	if err := m.FileAtSetEnd(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.Close()
	if _, ok := m.AtSetEnd(models.KAFKA, func(context.Context) {}); ok {
		t.Fatal("a closed manager took a registration")
	}
}

// A registration that panics is recovered: the others still run, and the
// read is told why it cannot take every miss, naming the parser that
// panicked, as the logged panic does. A read whose context is done is told
// so: a registration may have returned before it filed.
func TestMockManager_FileAtSetEndSaysWhenItCannotVouch(t *testing.T) {
	m, logs := observedSetEndManager(t)
	handTicked(t) // never ticked: no watch warns, so the panic is all the log holds
	var ran []string
	mustRegister(t, m, func(context.Context) { ran = append(ran, "a") })
	cancelB := mustRegisterKind(t, m, models.REDIS, func(context.Context) { panic("parser bug") })
	mustRegister(t, m, func(context.Context) { ran = append(ran, "c") })
	err := m.FileAtSetEnd(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Redis: parser bug") || strings.Contains(err.Error(), string(models.KAFKA)) {
		t.Fatalf("a Redis registration panicked: FileAtSetEnd said %v, want an error naming the panic and its parser alone", err)
	}
	if want := []string{"a", "c"}; !slices.Equal(ran, want) {
		t.Fatalf("ran %v, want %v: a panic stops no other registration", ran, want)
	}
	logged := logs.AllUntimed()
	if len(logged) != 1 || logged[0].Level != zapcore.ErrorLevel || logged[0].ContextMap()["kind"] != string(models.REDIS) || logged[0].ContextMap()["panic"] != "parser bug" {
		t.Fatalf("a Redis registration panicked: the log holds %v, want one error, naming the panic and its parser, Redis, alone", logged)
	}
	cancelB()
	done, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.FileAtSetEnd(done); !errors.Is(err, context.Canceled) {
		t.Fatalf("a read whose context is done: FileAtSetEnd said %v, want its context's error", err)
	}
	if err := m.FileAtSetEnd(context.Background()); err != nil {
		t.Fatal(err)
	}
	// With no registration run, a done context leaves nothing unfiled: the
	// read is as it was before any parser registered.
	empty := setEndManager(t)
	if err := empty.FileAtSetEnd(done); err != nil {
		t.Fatalf("no registration, a done context: FileAtSetEnd said %v, want nil", err)
	}
	cancelOnly := mustRegister(t, empty, func(context.Context) { t.Error("a cancelled registration ran") })
	cancelOnly()
	if err := empty.FileAtSetEnd(done); err != nil {
		t.Fatalf("only a cancelled registration, a done context: FileAtSetEnd said %v, want nil", err)
	}
}

// A miss a parser files for a call it held for a later test's recording
// (MockMismatchReport.HeldPastWindow) reaches the read as a held call
// (UnmatchedCall.Held), so keploy test tells it from a call made after the
// set's last test; every other miss is read as none.
func TestProxy_GetMockErrorsSaysWhichMissWasHeldPastItsWindow(t *testing.T) {
	p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 4)}
	p.BeginTestErrorCapture()
	for _, held := range []bool{true, false} {
		p.errChannel <- models.ParserError{ParserErrorType: models.ErrMockNotFound, Err: models.ErrNoMockMatched,
			MismatchReport: &models.MockMismatchReport{Protocol: "Kafka", ActualSummary: fmt.Sprintf("Fetch held=%v", held), HeldPastWindow: held}}
	}
	got, err := p.GetMockErrors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].Held || got[1].Held {
		t.Fatalf("read %+v; want the first miss held and the second not", got)
	}
}
