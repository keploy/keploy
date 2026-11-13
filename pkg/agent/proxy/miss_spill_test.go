package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// noSpillRoom leaves a list of misses no room on the agent's disk
// (maxSpilledMissBytes) for t: a miss past its room in memory, alike to none
// it holds, is dropped.
func noSpillRoom(t *testing.T) {
	t.Helper()
	spillBound(t, 0)
}

// spillBound bounds what a list keeps on the agent's disk to n bytes for t.
func spillBound(t *testing.T, n int64) {
	t.Helper()
	old := maxSpilledMissBytes
	maxSpilledMissBytes = n
	t.Cleanup(func() { maxSpilledMissBytes = old })
}

// drained is what a drains, every miss its spill kept read back.
func drained(t *testing.T, a *testErrorAccumulator) []error {
	t.Helper()
	errs, lost := a.drain()
	if lost != "" {
		t.Fatalf("the drain could not read back what its spill kept: %s", lost)
	}
	return errs
}

// fill sets every field v holds, through pointers, lists and structs, to a
// value no zero value equals, n counting on; an interface is left as it is.
func fill(v reflect.Value, n *int) {
	*n++
	switch v.Kind() {
	case reflect.String:
		v.SetString(fmt.Sprintf("v%d", *n))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(*n))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(*n))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(*n) + 0.5)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem(), n)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 2, 2))
		fill(v.Index(0), n)
		fill(v.Index(1), n)
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(time.Unix(int64(1_700_000_000+*n), int64(*n)).UTC()))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i), n)
			}
		}
	}
}

// filledMiss is a miss whose every field, its report's own unwritten ones
// (json:"-") among them, is set: a miss, never an answer.
func filledMiss() models.ParserError {
	var pe models.ParserError
	n := 0
	fill(reflect.ValueOf(&pe).Elem(), &n)
	pe.ParserErrorType = models.ErrMockNotFound
	pe.MismatchReport.Answer = ""
	return pe
}

// pastTheRoom is a when it holds one miss, its room (1) full, and err was kept
// past it: on its spill, as err is alike to the miss it holds in nothing.
func pastTheRoom(t *testing.T, err error) *testErrorAccumulator {
	t.Helper()
	a := &testErrorAccumulator{}
	if !a.addBounded(distinctMiss(-1), 1) {
		t.Fatal("the first miss was not held")
	}
	if !a.addBounded(err, 1) {
		t.Fatalf("a miss past the room was not kept: %s", a.lostWhy())
	}
	if a.spill == nil || a.spill.n != 1 {
		t.Fatalf("the miss past the room was not kept on the disk: %+v", a.spill)
	}
	return a
}

// A miss kept on the agent's disk is read back as it was filed: every field
// of its parser error and report, the report's unwritten ones (json:"-":
// where its parser marked it, the times folded into it) among them; its
// marks, in their order; and its Err as a read reads it, its text and
// whether it was a miss no mock matched, or none.
func TestASpilledMissIsReadBackAsItWasFiled(t *testing.T) {
	noMatch := filledMiss()
	noMatch.Err = fmt.Errorf("call 7: %w", models.ErrNoMockMatched)
	other := filledMiss()
	other.Err = errors.New("decode failed")
	noErr := filledMiss()
	noErr.Err = nil
	noReport := filledMiss()
	noReport.MismatchReport = nil
	zeroReport := filledMiss()
	zeroReport.MismatchReport = &models.MockMismatchReport{}
	rows := []struct {
		name string
		err  error
	}{
		{"a miss no mock matched", noMatch},
		{"another error", other},
		{"no error", noErr},
		{"no report", noReport},
		{"an empty report", zeroReport},
		{"made before its set", beforeSetMiss{noMatch}},
		{"made in an earlier set", earlierSetMiss{noMatch}},
		{"both marks, outermost first", beforeSetMiss{earlierSetMiss{noMatch}}},
		{"both marks, the other way", earlierSetMiss{beforeSetMiss{noMatch}}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			got := drained(t, pastTheRoom(t, row.err))
			if len(got) != 2 {
				t.Fatalf("read back %d misses; want the one held and the one spilled", len(got))
			}
			gotPE, gotMarks := unmarked(got[1])
			wantPE, wantMarks := unmarked(row.err)
			if !reflect.DeepEqual(gotMarks, wantMarks) {
				t.Fatalf("read back marked %v; want %v", gotMarks, wantMarks)
			}
			if (gotPE.Err == nil) != (wantPE.Err == nil) {
				t.Fatalf("read back Err %v; want %v", gotPE.Err, wantPE.Err)
			}
			if wantPE.Err != nil {
				if gotPE.Err.Error() != wantPE.Err.Error() || errors.Is(gotPE.Err, models.ErrNoMockMatched) != errors.Is(wantPE.Err, models.ErrNoMockMatched) {
					t.Fatalf("read back Err %q (no match %v); want %q (no match %v)", gotPE.Err, errors.Is(gotPE.Err, models.ErrNoMockMatched), wantPE.Err, errors.Is(wantPE.Err, models.ErrNoMockMatched))
				}
				if errors.Is(gotPE.Err, errors.New("another error")) {
					t.Fatal("read back Err is any error errors.Is asks for")
				}
			}
			gotPE.Err, wantPE.Err = nil, nil
			if !reflect.DeepEqual(gotPE, wantPE) {
				t.Fatalf("read back\n%+v\nwant\n%+v", gotPE, wantPE)
			}
		})
	}
}

// unmarked is the parser error err holds and the marks it is filed with,
// outermost first, read wrap by wrap as a read reads them.
func unmarked(err error) (models.ParserError, []string) {
	var marks []string
	for {
		switch e := err.(type) {
		case beforeSetMiss:
			marks, err = append(marks, "before"), e.error
			continue
		case earlierSetMiss:
			marks, err = append(marks, "earlier"), e.error
			continue
		}
		pe, _ := err.(models.ParserError)
		return pe, marks
	}
}

// An error past the room that no read lists (no parser's miss, or another
// parser error than a miss) is not kept on the disk: it is dropped as before,
// and no spill is made for it.
func TestAnErrorNoReadListsIsNotSpilled(t *testing.T) {
	for name, err := range map[string]error{
		"no parser error":      errors.New("noise"),
		"another parser error": models.ParserError{ParserErrorType: "other", MismatchReport: &models.MockMismatchReport{ClosestMock: "x"}},
	} {
		t.Run(name, func(t *testing.T) {
			a := &testErrorAccumulator{}
			a.addBounded(distinctMiss(0), 1)
			if a.addBounded(err, 1) {
				t.Fatal("it was kept past the room")
			}
			if a.spill != nil && a.spill.f != nil {
				t.Fatal("a spill file was made for it")
			}
			if got := drained(t, a); len(got) != 1 {
				t.Fatalf("read back %d; want the one miss held", len(got))
			}
		})
	}
}

// Misses past the room, alike to none held, are kept on the disk, not in
// memory, and read back after those held, in the order they were kept.
func TestMissesPastTheRoomAreReadBackInTheOrderKept(t *testing.T) {
	a := &testErrorAccumulator{}
	for i := 0; i < 5; i++ {
		if !a.addBounded(distinctMiss(i), 2) {
			t.Fatalf("miss %d was not kept", i)
		}
	}
	if len(a.errs) != 2 || a.spill.n != 3 {
		t.Fatalf("%d misses in memory and %d on the disk; want 2 and 3", len(a.errs), a.spill.n)
	}
	got := drained(t, a)
	if len(got) != 5 {
		t.Fatalf("read back %d misses; want 5", len(got))
	}
	for i, err := range got {
		if pe, _ := unmarked(err); pe.MismatchReport.ClosestMock != fmt.Sprintf("mock-%d", i) {
			t.Fatalf("miss %d read back is %s's", i, pe.MismatchReport.ClosestMock)
		}
	}
}

// A miss past the room alike to one kept on the disk folds into it, as into
// a held one: the disk keeps one report per kind of call too, and the read
// lists each folded call at its own time, right after the one it is alike
// to.
func TestAMissAlikeToASpilledOneFoldsIntoIt(t *testing.T) {
	a := &testErrorAccumulator{}
	for _, err := range []error{distinctMiss(0), pollerMiss(0), pollerMiss(1), distinctMiss(2), pollerMiss(2)} {
		if !a.addBounded(err, 1) {
			t.Fatalf("%v was not kept", err)
		}
	}
	if a.spill.n != 2 {
		t.Fatalf("the disk keeps %d misses; want the first poller miss and the other", a.spill.n)
	}
	got := drained(t, a)
	if len(got) != 3 {
		t.Fatalf("read back %d misses; want 3", len(got))
	}
	times := heldTimes(t, got[1])
	if len(times) != 2 || !times[0].Equal(pollerMiss(1).MismatchReport.At) || !times[1].Equal(pollerMiss(2).MismatchReport.At) || len(heldTimes(t, got[2])) != 0 {
		t.Fatalf("the spilled poller miss carries %v; want the next two poller misses' times", times)
	}
}

// The same through the read: a list full of misses each unlike the others,
// then a poller's three: the first is kept on the disk, the other two fold
// into it, and the read lists all, complete.
func TestGetMockErrors_FoldsAPollerIntoItsSpilledMiss(t *testing.T) {
	eitherSwitch(t, func(t *testing.T) {
		p := startedDrainProxy(t, maxPendingMockErrors+8)
		p.BeginTestErrorCapture()
		for i := 0; i < maxPendingMockErrors; i++ {
			p.SendError(distinctMiss(i))
		}
		for s := 0; s < 3; s++ {
			p.SendError(pollerMiss(s))
		}
		got, err := p.GetMockErrors(context.Background())
		if err != nil || len(got) != maxPendingMockErrors+3 {
			t.Fatalf("read %d misses, err %v; want all %d, complete", len(got), err, maxPendingMockErrors+3)
		}
		for s := 0; s < 3; s++ {
			c := got[maxPendingMockErrors+s]
			if c.Folded != (s > 0) || !c.At.Equal(pollerMiss(s).MismatchReport.At) || c.ClosestMock != "mock-orders" {
				t.Fatalf("poller call %d reads Folded %v at %v (closest %q); want it at its own time, folded after the first", s, c.Folded, c.At, c.ClosestMock)
			}
		}
	})
}

// The disk keeps up to its bound (maxSpilledMissBytes): a miss that does not
// fit is dropped, lostWhy saying the bound was reached, and once full the
// disk takes no more, though a later miss would fit, so a read is short from
// one place on. A miss alike to one it kept still folds into it.
func TestTheSpillIsBoundedAndSaysWhy(t *testing.T) {
	probe := &testErrorAccumulator{}
	for i := 0; i < 3; i++ {
		probe.addBounded(distinctMiss(i), 1)
	}
	two := probe.spill.size
	probe.addBounded(distinctMiss(3), 1)
	three := probe.spill.size
	drained(t, probe)
	spillBound(t, three)

	big := distinctMiss(5)
	big.MismatchReport.ActualSummary = strings.Repeat("x", 4096)
	a := &testErrorAccumulator{}
	for i := 0; i < 3; i++ {
		a.addBounded(distinctMiss(i), 1)
	}
	if a.spill.size != two {
		t.Fatalf("the disk keeps %d bytes; want the %d it kept the same two misses in", a.spill.size, two)
	}
	if a.addBounded(big, 1) {
		t.Fatal("a miss past the disk's bound was kept")
	}
	if why := a.lostWhy(); !strings.Contains(why, "they reached the 0 MiB the agent keeps of them on its disk") {
		t.Fatalf("lostWhy = %q; want the disk's bound named", why)
	}
	if a.addBounded(distinctMiss(3), 1) {
		t.Fatal("the disk, once full, kept a miss that would fit")
	}
	if !a.addBounded(pollerMissLike(distinctMiss(2)), 1) {
		t.Fatal("a miss alike to one on the full disk did not fold into it")
	}
	if got := drained(t, a); len(got) != 3 || len(heldTimes(t, got[1])) != 0 || len(heldTimes(t, got[2])) != 1 {
		t.Fatalf("read back %d misses; want the 3 kept, the last with one folded", len(got))
	}
	// A miss that fills the bound exactly is kept.
	b := &testErrorAccumulator{}
	for i := 0; i < 4; i++ {
		if !b.addBounded(distinctMiss(i), 1) {
			t.Fatalf("miss %d, within the bound, was not kept: %s", i, b.lostWhy())
		}
	}
	if got := drained(t, b); len(got) != 4 {
		t.Fatalf("read back %d misses; want 4", len(got))
	}
}

// A list with no spill has no reason a miss was not kept on the disk, and
// nothing to read back.
func TestNoSpillHasNoWhyAndNothingToReadBack(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a list with no spill panicked: %v", r)
		}
	}()
	if why := (&testErrorAccumulator{}).lostWhy(); why != "" {
		t.Fatalf("lostWhy = %q; want none", why)
	}
	var s *missSpill
	if got, lost := s.readBack(); got != nil || lost != "" {
		t.Fatalf("readBack = %v, %q; want nothing", got, lost)
	}
}

// pollerMissLike is err's miss made again a second later: alike to it.
func pollerMissLike(pe models.ParserError) models.ParserError {
	r := *pe.MismatchReport
	r.At = r.At.Add(time.Second)
	pe.MismatchReport = &r
	return pe
}

// A disk that refuses the spill is said: the folder it is made in cannot be
// written, or its file refuses a write. The miss is dropped, and lostWhy says
// why.
func TestASpillTheDiskRefusesSaysWhy(t *testing.T) {
	t.Run("no folder", func(t *testing.T) {
		t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "gone"))
		a := &testErrorAccumulator{}
		a.addBounded(distinctMiss(0), 1)
		if a.addBounded(distinctMiss(1), 1) {
			t.Fatal("a miss was kept with no folder for its file")
		}
		if why := a.lostWhy(); !strings.HasPrefix(why, "the agent could not make a file for them: ") {
			t.Fatalf("lostWhy = %q; want the file it could not make named", why)
		}
		if got := drained(t, a); len(got) != 1 {
			t.Fatalf("read back %d; want the one miss held", len(got))
		}
	})
	t.Run("a write refused", func(t *testing.T) {
		a := pastTheRoom(t, distinctMiss(0))
		_ = a.spill.f.Close()
		if a.addBounded(distinctMiss(1), 1) {
			t.Fatal("a miss its file refused was kept")
		}
		if why := a.lostWhy(); !strings.HasPrefix(why, "the agent's disk refused them: ") {
			t.Fatalf("lostWhy = %q; want the refused write named", why)
		}
	})
}

// A spill's file is out of its folder from the start, so the agent leaves
// none behind, even killed, and it is closed at the drain; the next miss
// past the room starts a new one. Where a system kept it in the folder (it
// cannot remove an open file), the drain removes it.
func TestASpillIsRemovedAtTheDrain(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	a := pastTheRoom(t, distinctMiss(0))
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("the spill's file is in its folder: %v", left)
	}
	f := a.spill.f
	kept := filepath.Join(dir, "kept.gob")
	if err := os.WriteFile(kept, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	a.spill.path = kept
	drained(t, a)
	if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("the spill's file is open after the drain: %v", err)
	}
	if _, err := os.Stat(kept); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the spill's file kept in its folder is there after the drain: %v", err)
	}
	if a.spill != nil {
		t.Fatal("the drained list kept its spill")
	}
	a.addBounded(distinctMiss(1), 1)
	if !a.addBounded(distinctMiss(2), 1) || a.spill.n != 1 {
		t.Fatal("a miss past the room after a drain did not start a new spill")
	}
	drained(t, a)
}

// A spill that cannot be read back whole says so: the drain returns the
// misses it read and why the rest are lost; a fold into a miss it could not
// read back is lost with it.
func TestASpillNotReadBackWholeSaysWhy(t *testing.T) {
	t.Run("cut short", func(t *testing.T) {
		a := pastTheRoom(t, distinctMiss(0))
		a.addBounded(pollerMiss(0), 1)
		a.addBounded(pollerMiss(1), 1)
		if err := a.spill.f.Truncate(a.spill.size - 8); err != nil {
			t.Fatal(err)
		}
		var got []error
		var lost string
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("a fold into a miss not read back panicked the drain: %v", r)
				}
			}()
			got, lost = a.drain()
		}()
		if len(got) != 2 || !strings.HasPrefix(lost, "1 of the 2 misses kept on the agent's disk could not be read back: ") {
			t.Fatalf("read back %d misses, lost %q; want the held one and the first spilled, and the second said lost", len(got), lost)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		a := pastTheRoom(t, distinctMiss(0))
		_ = a.spill.f.Close()
		got, lost := a.drain()
		if len(got) != 1 || !strings.HasPrefix(lost, "the 1 misses kept on the agent's disk could not be read back: ") {
			t.Fatalf("read back %d misses, lost %q; want the held one, and the spilled one said lost", len(got), lost)
		}
	})
}

// breakSpill cuts what a's spill holds to nothing, so it cannot be read back.
func breakSpill(t *testing.T, a *testErrorAccumulator) {
	t.Helper()
	if a == nil || a.spill == nil {
		t.Fatal("no spill to break")
	}
	if err := a.spill.f.Truncate(0); err != nil {
		t.Fatal(err)
	}
}

// Misses kept on the disk that cannot be read back make the read that would
// list them incomplete, naming why, said at Warn, wherever the list is
// drained: the test's own at its read, those made between two tests carried
// into the next test, those made before a set's first test (the set's, under
// the proven switch), a window read on the degenerate path, a test set's
// start, and EndTestErrorCapture.
func TestMissesNotReadBackMakeTheirReadIncomplete(t *testing.T) {
	const cause = "could not be read back"
	overfill := func(p *Proxy) {
		for i := 0; i <= maxPendingMockErrors; i++ {
			p.SendError(distinctMiss(i))
		}
		flushDrain(p)
	}
	rows := []struct {
		name string
		on   bool
		run  func(t *testing.T, p *Proxy)
	}{
		{"its own read", false, func(t *testing.T, p *Proxy) {
			p.BeginTestErrorCapture()
			overfill(p)
			breakSpill(t, p.activeTestErrors.Load())
		}},
		{"made between two tests", false, func(t *testing.T, p *Proxy) {
			overfill(p)
			breakSpill(t, &p.pendingMockErrors)
			p.ContinueTestErrorCapture()
		}},
		{"made before the set, the set's", true, func(t *testing.T, p *Proxy) {
			overfill(p)
			breakSpill(t, &p.pendingMockErrors)
			p.BeginTestErrorCapture()
		}},
		{"a window read before", false, func(t *testing.T, p *Proxy) {
			acc := &testErrorAccumulator{read: true}
			for i := 0; i <= maxPendingMockErrors; i++ {
				acc.addBounded(distinctMiss(i), maxPendingMockErrors)
			}
			breakSpill(t, acc)
			p.captureMu.Lock()
			p.activeTestErrors.Store(acc)
			p.captureMu.Unlock()
			p.ContinueTestErrorCapture()
		}},
		{"a set's start, its window's", true, func(t *testing.T, p *Proxy) {
			p.BeginTestErrorCapture()
			overfill(p)
			breakSpill(t, p.activeTestErrors.Load())
			p.cutMissesAtSetStart(context.Background())
		}},
		{"a set's start, those between tests", true, func(t *testing.T, p *Proxy) {
			overfill(p)
			breakSpill(t, &p.pendingMockErrors)
			p.cutMissesAtSetStart(context.Background())
		}},
		{"no window opened", false, func(t *testing.T, p *Proxy) {
			overfill(p)
			breakSpill(t, &p.pendingMockErrors)
		}},
		{"EndTestErrorCapture", false, func(t *testing.T, p *Proxy) {
			p.BeginTestErrorCapture()
			overfill(p)
			breakSpill(t, p.activeTestErrors.Load())
			p.EndTestErrorCapture()
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if row.on {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			core, logs := observer.New(zap.WarnLevel)
			p := startedDrainProxyLogging(t, 8, zap.New(core))
			row.run(t, p)
			_, err := p.GetMockErrors(context.Background())
			if !errors.Is(err, models.ErrUnmatchedCallsUnread) || !strings.Contains(err.Error(), cause) {
				t.Fatalf("GetMockErrors = %v; want it incomplete, naming the misses not read back", err)
			}
			if n := logs.FilterMessageSnippet("misses keploy kept on the agent's disk could not be read back").Len(); n != 1 {
				t.Fatalf("said %d times at Warn; want once", n)
			}
		})
	}
}

// Misses kept on the disk that are dropped anyway as no test's are said at
// Warn when they cannot be read back: those of a window never read, and,
// with the proven switch off, those made before a set's first test. Where
// all were read back, nothing is said.
func TestMissesNotReadBackThatAreNoTestsAreSaid(t *testing.T) {
	unreadWindow := func(t *testing.T, p *Proxy, broken bool) {
		acc := &testErrorAccumulator{}
		for i := 0; i <= maxPendingMockErrors; i++ {
			acc.addBounded(distinctMiss(i), maxPendingMockErrors)
		}
		if broken {
			breakSpill(t, acc)
		}
		p.captureMu.Lock()
		p.activeTestErrors.Store(acc)
		p.captureMu.Unlock()
		p.ContinueTestErrorCapture()
	}
	beforeSet := func(t *testing.T, p *Proxy, broken bool) {
		for i := 0; i <= maxPendingMockErrors; i++ {
			p.SendError(distinctMiss(i))
		}
		flushDrain(p)
		if broken {
			breakSpill(t, &p.pendingMockErrors)
		}
		p.BeginTestErrorCapture()
	}
	rows := []struct {
		name   string
		run    func(t *testing.T, p *Proxy, broken bool)
		broken bool
	}{
		{"a window never read", unreadWindow, true},
		{"a window never read, all read back", unreadWindow, false},
		{"made before the set, switch off", beforeSet, true},
		{"made before the set, switch off, all read back", beforeSet, false},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "")
			core, logs := observer.New(zap.WarnLevel)
			p := startedDrainProxyLogging(t, 8, zap.New(core))
			row.run(t, p, row.broken)
			if _, err := p.GetMockErrors(context.Background()); err != nil {
				t.Fatalf("GetMockErrors = %v; misses that are no test's leave the read complete", err)
			}
			want := map[bool]int{true: 1, false: 0}[row.broken]
			if n := logs.FilterMessageSnippet("could not be read back").Len(); n != want {
				t.Fatalf("said %d times at Warn; want %d", n, want)
			}
		})
	}
}

// A list carried into the next test's window (made between two tests, or,
// under the proven switch, before a set's first test) that its window, and
// the disk past it, cannot keep makes that test's read incomplete, naming
// the full list and why the disk took no more, said at Warn with what to do.
func TestACarriedListPastTheDiskSaysWhy(t *testing.T) {
	for name, row := range map[string]struct {
		on    bool
		open  func(p *Proxy)
		cause string
	}{
		"between two tests":      {false, func(p *Proxy) { p.ContinueTestErrorCapture() }, missLostListFull},
		"before the set's first": {true, func(p *Proxy) { p.BeginTestErrorCapture() }, missLostBeforeSetFull},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, map[bool]string{true: "1", false: ""}[row.on])
			core, logs := observer.New(zap.WarnLevel)
			p := startedDrainProxyLogging(t, 8, zap.New(core))
			// A window read on the degenerate path, holding more than its
			// room: its misses are carried into the next window.
			acc := &testErrorAccumulator{read: true}
			for i := 0; i <= maxPendingMockErrors; i++ {
				acc.addBounded(distinctMiss(i), maxPendingMockErrors)
			}
			p.captureMu.Lock()
			p.activeTestErrors.Store(acc)
			p.captureMu.Unlock()
			noSpillRoom(t)
			row.open(p)
			_, err := p.GetMockErrors(context.Background())
			if !errors.Is(err, models.ErrUnmatchedCallsUnread) || !strings.Contains(err.Error(), row.cause+": they reached the 0 MiB") {
				t.Fatalf("GetMockErrors = %v; want it incomplete, naming the full list and the disk's bound", err)
			}
			if n := logs.FilterMessageSnippet("a miss was lost").Len(); n != 1 {
				t.Fatalf("the loss was said %d times at Warn; want once", n)
			}
		})
	}
}

// A miss made between two tests that the list waiting for the next test, and
// the disk past it, could not keep is said at Warn once, naming why and what
// to do.
func TestAPendingMissLostIsSaidOnce(t *testing.T) {
	noSpillRoom(t)
	core, logs := observer.New(zap.WarnLevel)
	p := startedDrainProxyLogging(t, 8, zap.New(core))
	for i := 0; i <= maxPendingMockErrors+2; i++ {
		p.SendError(distinctMiss(i))
	}
	flushDrain(p)
	said := logs.FilterMessageSnippet("a miss made while no test's window was open was lost").All()
	if len(said) != 1 || !strings.Contains(said[0].ContextMap()["cause"].(string), missLostBetweenFull+": they reached the 0 MiB") || said[0].ContextMap()["next_step"] != missLostNextStep {
		t.Fatalf("said %+v; want once at Warn, naming the full list, the disk's bound and what to do", said)
	}
}
