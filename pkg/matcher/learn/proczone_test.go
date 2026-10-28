package learn

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// fakeProc writes a process's /proc entry under root: its environ, its stat
// (with a name of spaces and parentheses, which hold no field) and the files
// of its own root.
func fakeProc(t *testing.T, root string, pid, start string, env []string, files map[string][]byte, links map[string]string) {
	t.Helper()
	dir := filepath.Join(root, pid)
	write := func(p string, data []byte) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "environ"), []byte(strings.Join(env, "\x00")+"\x00"))
	stat := pid + " (my app) (x) S 1 1 1 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 4 0 " + start + " 1000 100 18446744073709551615"
	write(filepath.Join(dir, "stat"), []byte(stat))
	for name, data := range files {
		write(filepath.Join(dir, "root", name), data)
	}
	for name, target := range links {
		p := filepath.Join(dir, "root", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
}

// The agent reads the zone of the process that made a call from the
// process's own files: TZ from the environment it started with, else its
// /etc/localtime, both in its own root, never keploy's. A TZ naming a zone
// the process's root holds no file of, a process keploy cannot read and a
// call of no process are errors (unknown), never keploy's own zone. Each
// process is read once; a pid another process has since taken is read again.
func TestReadProcZoneReadsTheZoneOfTheProcessThatMadeTheCall(t *testing.T) {
	kolkata, err := os.ReadFile("/usr/share/zoneinfo/Asia/Kolkata")
	if err != nil {
		t.Skipf("no host zone files: %v", err)
	}
	berlin, err := os.ReadFile("/usr/share/zoneinfo/Europe/Berlin")
	if err != nil {
		t.Skipf("no host zone files: %v", err)
	}
	at := time.Date(2026, 10, 28, 7, 30, 0, 0, time.UTC)
	zi := "usr/share/zoneinfo/"
	proc := t.TempDir()
	fakeProc(t, proc, "101", "500", []string{"PATH=/bin", "TZ=Asia/Kolkata", "TZ=Europe/Berlin"}, map[string][]byte{zi + "Asia/Kolkata": kolkata, zi + "Europe/Berlin": berlin}, map[string]string{"etc/localtime": "/" + zi + "Europe/Berlin"})
	fakeProc(t, proc, "102", "500", []string{"PATH=/bin"}, map[string][]byte{zi + "Europe/Berlin": berlin}, map[string]string{"etc/localtime": "/" + zi + "Europe/Berlin"})
	fakeProc(t, proc, "103", "500", []string{"TZ=Asia/Kolkata"}, map[string][]byte{zi + "Europe/Berlin": berlin}, nil)
	fakeProc(t, proc, "104", "500", []string{"TZ="}, nil, map[string]string{"etc/localtime": "/" + zi + "Europe/Berlin"})
	fakeProc(t, proc, "105", "500", []string{"PATH=/bin"}, nil, map[string]string{"etc/localtime": "/" + zi + "Europe/Berlin"})
	// A link of a directory on the way, absolute, names the process's own
	// root, never keploy's: here a loop, which the process reads as UTC.
	fakeProc(t, proc, "106", "500", []string{"TZ=Asia/Kolkata"}, nil, map[string]string{"usr/share/zoneinfo": "/usr/share/zoneinfo"})
	fakeProc(t, proc, "107", "500", []string{"PATH=/bin"}, nil, map[string]string{"etc/localtime": "/usr/share/zoneinfo/Asia/Kolkata", "usr/share": "/usr/share"})
	// An absolute link of a directory that names a directory in the
	// process's root.
	fakeProc(t, proc, "108", "500", []string{"TZ=Asia/Kolkata"}, map[string][]byte{"opt/zi/Asia/Kolkata": kolkata}, map[string]string{"usr/share/zoneinfo": "/opt/zi"})
	// An environment that reads empty: of a process exiting (a zombie, or
	// one flagged PF_EXITING, which the kernel sets before the process lets
	// go of its memory) unknown, never its /etc/localtime, which a TZ it
	// set may have overridden; of a live process (env -i) no TZ.
	for _, pid := range []string{"113", "114", "115"} {
		fakeProc(t, proc, pid, "500", nil, map[string][]byte{zi + "Europe/Berlin": berlin}, map[string]string{"etc/localtime": "/" + zi + "Europe/Berlin"})
	}
	for pid, stat := range map[string]string{
		"113": "113 (my app) Z 1 1 1 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 4 0 500 1000 100 18446744073709551615",
		"115": "115 (my app) R 1 1 1 0 -1 4194564 100 0 0 0 1 2 0 0 20 0 4 0 500 1000 100 18446744073709551615",
	} {
		if err := os.WriteFile(filepath.Join(proc, pid, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// No /etc/localtime at all, no TZ: UTC.
	fakeProc(t, proc, "109", "500", []string{"PATH=/bin"}, map[string][]byte{"etc/hostname": []byte("app")}, nil)
	for _, row := range []struct {
		what string
		pid  uint32
		want string // the zone's name; "!" an error
		off  int
	}{
		{"TZ in its environment (the first, as getenv reads it), over its /etc/localtime", 101, "+05:30", 19800},
		{"no TZ: its /etc/localtime, in its own root", 102, "Europe/Berlin", 3600},
		{"a TZ naming a zone its root holds no file of: unknown, never keploy's own", 103, "!", 0},
		{"an empty TZ: UTC", 104, "UTC", 0},
		{"no TZ, a link to no file in its root: unknown", 105, "!", 0},
		{"TZ naming a zone through an absolute link of its zoneinfo directory to itself: unknown, never keploy's file", 106, "!", 0},
		{"/etc/localtime through an absolute link of a directory to itself: unknown, never keploy's file", 107, "!", 0},
		{"TZ naming a zone through an absolute link of its zoneinfo directory, read in its root", 108, "+05:30", 19800},
		{"no /etc/localtime at all and no TZ: UTC", 109, "UTC", 0},
		{"an empty environment of a zombie: unknown, never its /etc/localtime", 113, "!", 0},
		{"an empty environment of a live process (env -i): no TZ, its /etc/localtime", 114, "Europe/Berlin", 3600},
		{"an empty environment of a process flagged PF_EXITING: unknown, never its /etc/localtime", 115, "!", 0},
		{"a process that is gone", 999, "!", 0},
		{"no process", 0, "!", 0},
	} {
		z, err := ReadProcZone(proc, row.pid)
		if row.want == "!" {
			if err == nil || z.Loc != nil {
				t.Errorf("%s: %v %v; want an error and no zone", row.what, z.Loc, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", row.what, err)
			continue
		}
		if _, off := at.In(z.Loc).Zone(); z.Name != row.want || off != row.off {
			t.Errorf("%s: %q at %d; want %q at %d", row.what, z.Name, off, row.want, row.off)
		}
	}

	// No process is read where keploy runs in a PID namespace of its own
	// (no initial namespace's entry at self/ns/pid): a kernel pid there
	// names another process, or none.
	if z, err := NewProcZones(proc).Of(101); err == nil || z.Loc != nil || !strings.Contains(err.Error(), "PID namespace of its own") {
		t.Errorf("keploy in a PID namespace of its own: %v %v; want unknown, saying why", z.Loc, err)
	}
	if err := os.MkdirAll(filepath.Join(proc, "self", "ns"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("pid:[4026531836]", filepath.Join(proc, "self", "ns", "pid")); err != nil {
		t.Fatal(err)
	}
	if z, err := NewProcZones(proc).Of(101); err != nil || z.Name != "+05:30" {
		t.Errorf("keploy in the initial PID namespace: %q %v; want the process's zone", z.Name, err)
	}
	var none *ProcZones
	if z, err := none.Of(101); err == nil || z.Loc != nil {
		t.Errorf("a nil cache: %v %v; want unknown", z.Loc, err)
	}

	// Read once per process: a changed environment of the same process is
	// not read again, a pid another process took (a new start) is.
	zs := NewProcZones(proc)
	if z, err := zs.Of(102); err != nil || z.Name != "Europe/Berlin" {
		t.Fatalf("first read: %q %v", z.Name, err)
	}
	if err := os.WriteFile(filepath.Join(proc, "102", "environ"), []byte("TZ=UTC\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	if z, err := zs.Of(102); err != nil || z.Name != "Europe/Berlin" {
		t.Errorf("the same process again: %q %v; want the zone read at its first call", z.Name, err)
	}
	stat := filepath.Join(proc, "102", "stat")
	b, err := os.ReadFile(stat)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stat, []byte(strings.Replace(string(b), " 500 ", " 777 ", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if z, err := zs.Of(102); err != nil || z.Name != "UTC" {
		t.Errorf("a pid another process took: %q %v; want it read again", z.Name, err)
	}
	if z, err := zs.Of(103); err == nil || z.Loc != nil {
		t.Errorf("a process whose zone keploy cannot read, through the cache: %v %v; want unknown", z.Loc, err)
	}
	// Processes that share a zone file share one parsed zone, whatever
	// pid reads it; a file of the same name another image ships with other
	// bytes is its own zone.
	fakeProc(t, proc, "110", "500", []string{"PATH=/bin"}, map[string][]byte{zi + "Europe/Berlin": berlin}, map[string]string{"etc/localtime": "/" + zi + "Europe/Berlin"})
	fakeProc(t, proc, "112", "500", []string{"PATH=/bin"}, map[string][]byte{zi + "Europe/Berlin": berlin}, map[string]string{"etc/localtime": "/" + zi + "Europe/Berlin"})
	fakeProc(t, proc, "111", "500", []string{"PATH=/bin"}, map[string][]byte{zi + "Europe/Berlin": kolkata}, map[string]string{"etc/localtime": "/" + zi + "Europe/Berlin"})
	shared := NewProcZones(proc)
	z110, err110 := shared.Of(110)
	z112, err112 := shared.Of(112)
	z111, err111 := shared.Of(111)
	if err110 != nil || err112 != nil || err111 != nil {
		t.Fatalf("shared zone files: %v %v %v", err110, err112, err111)
	}
	if z110.Loc != z112.Loc {
		t.Errorf("one zone file read for two processes: two parsed zones; want one shared")
	}
	if z110.Loc == z111.Loc {
		t.Errorf("two zone files of one name and different bytes: one parsed zone; want each its own")
	}
	if _, off := at.In(z111.Loc).Zone(); off != 19800 {
		t.Errorf("a Berlin-named file holding Kolkata's bytes: offset %d; want its own bytes' 19800", off)
	}
	if got := procStart(proc, 101); got != "500" {
		t.Errorf("the start of a process whose name holds spaces and parentheses: %q; want 500", got)
	}
}

// A call is learned in the zone of the process that made it (WithProcZone,
// Tracker.DriftsOf), never the agent's: a learning that depends on it names
// the process's zone. Where keploy could not read the process's zone, the
// learning names none, whatever the agent's own zone is (a zone keploy
// could not read may be any), and a ctx of no process zone reads the
// tracker's own, as before.
func TestACallIsLearnedInTheZoneOfItsOwnProcess(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	const tp = "body.at.$date.$numberLong"
	shape := func(l Leaf) Shape { return Shape{Leaves: map[string]Leaf{tp: l}} }
	typed := func(at time.Time) Leaf { return InstantLeaf(tp, "body.at", at, time.Millisecond) }
	hourIn := func(loc *time.Location, at time.Time) time.Time {
		l := at.In(loc)
		return time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), 0, 0, 0, loc)
	}
	rec := time.Date(2026, 10, 20, 9, 47, 7, 600e6, time.UTC)
	live := rec.Add(3*time.Hour + 7*time.Minute)
	learn := func(dc *DriftCache) models.ClockOffset {
		t.Helper()
		dc.appClock = true
		ln := ClassifyIn(dc, []string{tp}, shape(typed(hourIn(ist, live))), shape(typed(hourIn(ist, rec))), rec.Add(time.Millisecond), nil, nil, nil, nil)[0]
		o, ok := ln.Clock()
		if !ok {
			t.Fatalf("learned %s (%s); want a read of the clock", ln.Kind, ln.Why)
		}
		return o
	}
	tr := NewTracker()
	tr.SetAppZone(time.UTC) // the agent's own zone
	app := WithProcZone(context.Background(), ProcZone{Loc: ist, Name: "+05:30"})
	if o := learn(tr.DriftsOf(app)); o.Res != time.Hour || o.AppZone != "+05:30" {
		t.Errorf("a call of a process in IST, the agent in UTC: %+v; want the IST hour, naming +05:30", o)
	}
	tr = NewTracker()
	tr.SetAppZone(ist)
	if o := learn(tr.DriftsOf(WithProcZone(context.Background(), ProcZone{}))); o.Res != time.Hour || o.AppZone != "" {
		t.Errorf("a call of a process whose zone keploy could not read, the agent in IST: %+v; want the hour learned, naming no zone", o)
	}
	tr = NewTracker()
	tr.SetAppZone(ist)
	if o := learn(tr.DriftsOf(context.Background())); o.Res != time.Hour || o.AppZone != "+05:30" {
		t.Errorf("a ctx of no process zone: %+v; want the tracker's zone named, as before", o)
	}
	// The views share the test's cache, never its zone.
	tr = NewTracker()
	tr.SetAppZone(time.UTC)
	if v := tr.DriftsOf(app); v.driftState != tr.Drifts().driftState || tr.Drifts().zone != time.UTC || v.zone != ist {
		t.Errorf("a call's view of the test's cache: shares %v, tracker zone %v, view zone %v", v.driftState == tr.Drifts().driftState, tr.Drifts().zone, v.zone)
	}
}

// A replay checks a learned clock read in the zone the application's
// process writes its clock in in this run, where keploy read it from the
// process (Span.Own: a wall clock of no zone is the application's own),
// over the zone the learn step named; the named zone where it could not;
// and where neither is known, a plain read cut to an hour or coarser is
// refused, saying so, while a finer one is read as before.
func TestTheReplayApplicationsOwnZoneWinsOverTheOneLearned(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	const wp = "body.seenAt"
	wall := func(loc *time.Location, at time.Time) Leaf {
		return Leaf{Path: wp, Noise: wp, Type: String, Value: at.In(loc).Format("2006-01-02 15:04:05")}
	}
	hourIn := func(loc *time.Location, at time.Time) time.Time {
		l := at.In(loc)
		return time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), 0, 0, 0, loc)
	}
	// Learned on a laptop in IST: a wall clock of no zone the application
	// writes in its zone, cut to its hour.
	o := models.ClockOffset{Res: time.Hour, Zone: models.ZoneApp, Layout: "iso-second-space", AppZone: "+05:30"}
	now := time.Date(2026, 10, 28, 4, 45, 0, 0, time.UTC)
	own := Span{From: now, To: now.Add(20 * time.Millisecond), Zone: time.UTC, Own: true}
	utcHour, istHour := wall(time.UTC, hourIn(time.UTC, now)), wall(ist, hourIn(ist, now))
	if ok, why := CheckClockRead(wp, o, utcHour, own, true); !ok {
		t.Errorf("learned in IST, the application now writing in UTC: its UTC hour refused (%s); want checked in UTC", why)
	}
	if ok, why := CheckClockRead(wp, o, istHour, own, true); ok || !strings.Contains(why, "The application writes its clock in UTC in this run, not in +05:30, the zone the test was recorded in, and keploy checked the value in UTC") {
		t.Errorf("learned in IST, the application now writing in UTC: its IST hour %v %q; want refused, naming both zones", ok, why)
	}
	// The application's zone unread at replay (the agent's own is UTC):
	// the zone learned is read.
	agent := own
	agent.Own = false
	if ok, why := CheckClockRead(wp, o, istHour, agent, true); !ok {
		t.Errorf("learned in IST, the application's zone unread: its IST hour refused (%s); want checked in the zone learned", why)
	}
	if ok, _ := CheckClockRead(wp, o, utcHour, agent, true); ok {
		t.Errorf("learned in IST, the application's zone unread: its UTC hour passed; want checked in IST")
	}
	// Neither known: an hour's cut refused, saying so; a second's read as
	// before, in the agent's zone.
	old := o
	old.AppZone = ""
	if ok, why := CheckClockRead(wp, old, istHour, Span{From: now, To: now.Add(20 * time.Millisecond), Zone: ist}, true); ok || !strings.Contains(why, "keploy could not read the zone of the process that made this call, and the learn step named none") {
		t.Errorf("an hour's cut, neither zone known: %v %q; want refused, saying so", ok, why)
	}
	sec := models.ClockOffset{Res: time.Second, Zone: models.ZoneApp, Layout: "iso-second-space"}
	if ok, why := CheckClockRead(wp, sec, wall(ist, now), Span{From: now, To: now.Add(20 * time.Millisecond), Zone: ist}, true); !ok {
		t.Errorf("a second's cut, neither zone known: refused (%s); want read in the agent's zone, as before", why)
	}

	// Learned with the application's zone read as two zones (one pairing
	// read IST, another could not read the process's: models.AppZoneApart):
	// checked in the replay application's own zone, as each learning would
	// be, and refused where keploy cannot read it, saying what to do, never
	// read in a zone the learn step did not read it in.
	apart := o.Meet(models.ClockOffset{Res: time.Hour, Zone: models.ZoneApp, Layout: "iso-second-space"})
	if apart.AppZone != models.AppZoneApart {
		t.Fatalf("a named and an unnamed learning met as %+v; want AppZoneApart", apart)
	}
	if ok, why := CheckClockRead(wp, apart, utcHour, own, true); !ok {
		t.Errorf("learned in two zones, the application now writing in UTC: its UTC hour refused (%s); want checked in UTC", why)
	}
	if ok, _ := CheckClockRead(wp, apart, istHour, own, true); ok {
		t.Errorf("learned in two zones, the application now writing in UTC: its IST hour passed; want checked in UTC")
	}
	if ok, why := CheckClockRead(wp, apart, istHour, agent, true); ok || !strings.Contains(why, "the learn step read the application's zone as no one zone") || !strings.Contains(why, "--pid=host") {
		t.Errorf("learned in two zones, the application's zone unread: %v %q; want refused, saying why and what to do", ok, why)
	}
	// A wall clock either zone held, learned in two zones: both held it in
	// UTC, where it is checked when the application's zone is unread.
	either := models.ClockOffset{Res: time.Second, Zone: models.ZoneEither, Layout: "iso-second-space", AppZone: "+05:30"}.Meet(models.ClockOffset{Res: time.Second, Zone: models.ZoneEither, Layout: "iso-second-space"})
	if either.AppZone != models.AppZoneApart || either.Empty() {
		t.Fatalf("two wall clocks either zone held, named and unnamed, met as %+v; want AppZoneApart", either)
	}
	inIST := Span{From: now, To: now.Add(20 * time.Millisecond), Zone: ist}
	if ok, why := CheckClockRead(wp, either, wall(time.UTC, now), inIST, true); !ok {
		t.Errorf("either zone, learned in two zones, the application's zone unread: its UTC second refused (%s); want checked in UTC", why)
	}
	if ok, _ := CheckClockRead(wp, either, wall(ist, now), inIST, true); ok {
		t.Errorf("either zone, learned in two zones, the application's zone unread: its IST second passed; want checked in UTC alone")
	}
	// One cut to an hour is refused where the application's zone is unread,
	// as one of no name is (2026-10-28 QH): the meet cannot tell two named
	// zones from a named and an unnamed one.
	eHour := models.ClockOffset{Res: time.Hour, Zone: models.ZoneEither, Layout: "iso-second-space", AppZone: "+05:30"}.Meet(models.ClockOffset{Res: time.Hour, Zone: models.ZoneEither, Layout: "iso-second-space"})
	if ok, why := CheckClockRead(wp, eHour, utcHour, inIST, true); ok || !strings.Contains(why, "the learn step read the application's zone as no one zone") {
		t.Errorf("either zone, an hour's cut learned in two zones, neither zone known: %v %q; want refused, saying why", ok, why)
	}
	if ok, why := CheckClockRead(wp, apart, istHour, agent, true); ok || !strings.Contains(why, "docker and compose modes") {
		t.Errorf("learned in two zones, the application's zone unread: %v %q; want the refusal to say what works in docker and compose modes", ok, why)
	}

	// ClockSpanFrom carries the process's zone where the agent read it.
	tr := NewTracker()
	tr.SetAppZone(time.UTC)
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	tr.ServedMockAt(&models.Mock{Name: "mock-1", Kind: models.Mongo}, now)
	ctx := WithTracker(context.Background(), tr)
	arrival := now.Add(time.Second)
	if s, ok := ClockSpanFrom(WithProcZone(ctx, ProcZone{Loc: ist, Name: "+05:30"}), arrival); !ok || !s.Own || s.Zone != ist {
		t.Errorf("a call of a process in IST: %+v %v; want its zone, as its own", s, ok)
	}
	if s, ok := ClockSpanFrom(WithProcZone(ctx, ProcZone{}), arrival); !ok || s.Own || s.Zone != time.UTC {
		t.Errorf("a call of a process whose zone keploy could not read: %+v %v; want the tracker's zone, not as the application's own", s, ok)
	}
}

// The views of one test's cache in its calls' zones share what it keeps,
// never what one zone reads as a clock read: a wall clock of no zone the
// lead moved is read in each view's zone whichever call ran first, so a
// process writing UTC never takes a clock read only IST explains.
func TestOneTestsCacheReadsEachCallsClockInItsOwnZoneWhicheverRanFirst(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	callAt := time.Date(2026, 10, 20, 9, 47, 7, 0, time.UTC)
	recRead := callAt.Add(-time.Second)
	rec := recRead.In(ist).Format("2006-01-02 15:04:05")
	live := recRead.Add(FullRoundLead).In(ist).Format("2006-01-02 15:04:05")
	zones := []ProcZone{{Loc: ist, Name: "+05:30"}, {Loc: time.UTC, Name: "UTC"}}
	view := func(tr *Tracker, z ProcZone) *DriftCache {
		return tr.DriftsOf(WithProcZone(context.Background(), z))
	}
	fresh := make([]int, len(zones))
	for i, z := range zones {
		tr := NewTracker()
		tr.SetAppZone(time.UTC)
		fresh[i] = len(view(tr, z).ofIn(rec, live, nil, callAt).clock.rec)
	}
	if fresh[0] == fresh[1] {
		t.Fatalf("the value reads alike in IST and UTC (%v); the row shows nothing", fresh)
	}
	for _, order := range [][2]int{{0, 1}, {1, 0}} {
		tr := NewTracker()
		tr.SetAppZone(time.UTC)
		_ = view(tr, zones[order[0]]).ofIn(rec, live, nil, callAt)
		if got := len(view(tr, zones[order[1]]).ofIn(rec, live, nil, callAt).clock.rec); got != fresh[order[1]] {
			t.Errorf("%s after %s on one test's cache: %d clock words; a fresh cache in %s reads %d", zones[order[1]].Name, zones[order[0]].Name, got, zones[order[1]].Name, fresh[order[1]])
		}
	}
}
