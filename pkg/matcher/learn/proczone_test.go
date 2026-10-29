package learn

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// fakeProc writes a process's /proc entry under root: its environ, its
// command line (a program of no options), its maps (no JVM's), its stat
// (with a name of spaces and parentheses, which hold no field), its mount
// table (one mount, of its root) and the files of its own root.
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
	write(filepath.Join(dir, "cmdline"), []byte("/app/server\x00"))
	write(filepath.Join(dir, "maps"), []byte("5600-5610 r-xp 00000000 08:01 7 /app/server\n"))
	write(filepath.Join(dir, "stat"), []byte(statLine(pid, "my app) (x", "S", "4194560", start, statMem)))
	write(filepath.Join(dir, "mountinfo"), []byte(rootMount))
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

// rootMount is a mount table's line of a container's root.
const rootMount = "600 500 0:52 / / rw,relatime - overlay overlay rw,lowerdir=/l,upperdir=/u,workdir=/w\n"

// statMem is the addresses of a process's arguments and environment in its
// memory (stat fields 48-51: arg_start, arg_end, env_start, env_end).
const statMem = "140737488351000 140737488351100 140737488351100 140737488352000"

// statLine is a /proc/<pid>/stat line of all 52 fields, as a kernel from
// 3.5 writes it: the process named name, in state, of flags (field 9),
// started at start (field 22), its arguments and environment at mem
// (fields 48-51).
func statLine(pid, name, state, flags, start, mem string) string {
	return pid + " (" + name + ") " + state + " 1 1 1 0 -1 " + flags + " 100 0 0 0 1 2 0 0 20 0 4 0 " + start +
		" 1000 100 18446744073709551615 4194304 4227072 140737488350000 0 0 0 0 0 0 0 0 0 17 1 0 0 0 0 0 4239360 4239600 4243456 " + mem + " 0"
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
		"113": statLine("113", "my app", "Z", "4194560", "500", statMem),
		"115": statLine("115", "my app", "R", "4194564", "500", statMem),
	} {
		if err := os.WriteFile(filepath.Join(proc, pid, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A TZ through a zone file as if it were a directory (ENOTDIR): the
	// process's own files, lasting.
	fakeProc(t, proc, "116", "500", []string{"TZ=Asia/Kolkata/x"}, map[string][]byte{zi + "Asia/Kolkata": kolkata}, nil)
	// No /etc/localtime at all, no TZ: UTC.
	fakeProc(t, proc, "109", "500", []string{"PATH=/bin"}, map[string][]byte{"etc/hostname": []byte("app")}, nil)
	for _, row := range []struct {
		what string
		pid  uint32
		want string // the zone's name; "!" an error read again, "!!" one lasting the process's life (lastingError)
		off  int
	}{
		{"TZ in its environment (the first, as getenv reads it), over its /etc/localtime", 101, "+05:30", 19800},
		{"no TZ: its /etc/localtime, in its own root", 102, "Europe/Berlin", 3600},
		{"a TZ naming a zone its root holds no file of: unknown, never keploy's own", 103, "!!", 0},
		{"an empty TZ: UTC", 104, "UTC", 0},
		{"no TZ, a link to no file in its root: unknown", 105, "!!", 0},
		{"TZ naming a zone through an absolute link of its zoneinfo directory to itself: unknown, never keploy's file", 106, "!!", 0},
		{"/etc/localtime through an absolute link of a directory to itself: unknown, never keploy's file", 107, "!!", 0},
		{"TZ naming a zone through an absolute link of its zoneinfo directory, read in its root", 108, "+05:30", 19800},
		{"no /etc/localtime at all and no TZ: UTC", 109, "UTC", 0},
		{"a TZ naming a zone file as a directory: unknown", 116, "!!", 0},
		{"an empty environment of a zombie: unknown, never its /etc/localtime", 113, "!", 0},
		{"an empty environment of a live process (env -i): no TZ, its /etc/localtime", 114, "Europe/Berlin", 3600},
		{"an empty environment of a process flagged PF_EXITING: unknown, never its /etc/localtime", 115, "!", 0},
		{"a process that is gone", 999, "!", 0},
		{"no process", 0, "!", 0},
	} {
		z, err := ReadProcZone(proc, row.pid)
		if strings.HasPrefix(row.want, "!") {
			if err == nil || z.Loc != nil {
				t.Errorf("%s: %v %v; want an error and no zone", row.what, z.Loc, err)
			} else if isLasting(err) != (row.want == "!!") {
				t.Errorf("%s: lasting %v (%v); want %v", row.what, isLasting(err), err, row.want == "!!")
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
	// A link loop on a TZ's path is in the process's own files, as the C
	// library reads them: its reason says the TZ names no zone file, never
	// that keploy could not read one.
	if _, err := ReadProcZone(proc, 106); err == nil || err.Error() != "TZ=Asia/Kolkata names no zone file under the process's root" {
		t.Errorf("a TZ through a link loop: %v; want a TZ naming no zone file", err)
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
	// bytes is its own zone (here Berlin's rules written as a version 1
	// file); one of that name whose bytes hold another zone's rules is
	// unknown (loadZone).
	fakeProc(t, proc, "110", "500", []string{"PATH=/bin"}, map[string][]byte{zi + "Europe/Berlin": berlin}, map[string]string{"etc/localtime": "/" + zi + "Europe/Berlin"})
	fakeProc(t, proc, "112", "500", []string{"PATH=/bin"}, map[string][]byte{zi + "Europe/Berlin": berlin}, map[string]string{"etc/localtime": "/" + zi + "Europe/Berlin"})
	berlinV1 := tzifRules(t, berlin)
	fakeProc(t, proc, "111", "500", []string{"PATH=/bin"}, map[string][]byte{zi + "Europe/Berlin": berlinV1}, map[string]string{"etc/localtime": "/" + zi + "Europe/Berlin"})
	fakeProc(t, proc, "118", "500", []string{"PATH=/bin"}, map[string][]byte{zi + "Europe/Berlin": kolkata}, map[string]string{"etc/localtime": "/" + zi + "Europe/Berlin"})
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
	if z111.Name != "Europe/Berlin" {
		t.Errorf("a Berlin-named file holding Berlin's rules in other bytes: named %q; want Europe/Berlin", z111.Name)
	}
	if z, err := shared.Of(118); err == nil || z.Loc != nil || !isLasting(err) {
		t.Errorf("a Berlin-named file holding Kolkata's rules: %v %v; want unknown, lasting", z.Loc, err)
	}
	if id, exiting, ok := procIdent(proc, 101); !ok || exiting || id.start != "500" || id.mem != statMem {
		t.Errorf("the start and memory of a process whose name holds spaces and parentheses: %+v %v %v; want 500 and %s", id, exiting, ok, statMem)
	}
}

// ProcZones reads a process's zone once for each program it runs, as a JVM
// and the C library read their zone once: the zone, or an error the
// process's own files give (lastingError: a TZ naming no zone file, a
// user.timezone Java does not know), is kept, so the process is not read
// again at each call (a JVM's zone directory walked at each); an error of
// keploy's own read (a file it cannot read) is read again, as is a process
// exiting as keploy read it, which lets go of its files. A pid that runs
// another program is read again: another process taking it (a new start
// time), or an exec, which keeps the pid and its start time but lays its
// arguments and environment out anew (stat fields 48-51) and may run
// another executable.
func TestAProcessZoneIsReadOnceForEachProgramItRuns(t *testing.T) {
	proc := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proc, "self", "ns"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("pid:[4026531836]", filepath.Join(proc, "self", "ns", "pid")); err != nil {
		t.Fatal(err)
	}
	write := func(pid, name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(proc, pid, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env := func(pid, tz string) { write(pid, "environ", []byte("PATH=/bin\x00TZ="+tz+"\x00")) }
	stat := func(pid, flags, start, mem string) {
		write(pid, "stat", []byte(statLine(pid, "app", "S", flags, start, mem)))
	}
	// The zone's name, "!!" an error kept for the process's life, "!" one
	// read again.
	of := func(zs *ProcZones, pid uint32) (string, string) {
		z, err := zs.Of(pid)
		switch {
		case err != nil && isLasting(err):
			return "!!", err.Error()
		case err != nil:
			return "!", err.Error()
		}
		return z.Name, ""
	}
	const noZone = "Mars/Olympus" // a TZ naming no zone file: lasting
	zs := NewProcZones(proc)

	// A lasting error is kept: a TZ the process could not have changed
	// is not read again, nor its zone directory.
	fakeProc(t, proc, "801", "500", []string{"TZ=" + noZone}, map[string][]byte{"etc/hostname": []byte("app")}, nil)
	got, first := of(zs, 801)
	if got != "!!" {
		t.Fatalf("a TZ naming no zone file: %q %s; want an error kept for the process's life", got, first)
	}
	env("801", "UTC")
	if got, msg := of(zs, 801); got != "!!" || msg != first {
		t.Errorf("the same program again, its files changed: %q %s; want the error read at its first call, %s", got, msg, first)
	}
	stat("801", "4194560", "777", statMem)
	if got, msg := of(zs, 801); got != "UTC" {
		t.Errorf("a pid another process took (a new start): %q %s; want it read again", got, msg)
	}

	// An error of keploy's own read is read again.
	fakeProc(t, proc, "802", "500", nil, map[string][]byte{"etc/hostname": []byte("app")}, nil)
	if err := os.Remove(filepath.Join(proc, "802", "environ")); err != nil {
		t.Fatal(err)
	}
	if got, msg := of(zs, 802); got != "!" {
		t.Fatalf("an environment keploy cannot read: %q %s; want an error read again", got, msg)
	}
	env("802", "UTC")
	if got, msg := of(zs, 802); got != "UTC" {
		t.Errorf("the environment readable at the next call: %q %s; want it read again", got, msg)
	}

	// A process exiting (PF_EXITING) as keploy read it: what it read is
	// not kept, lasting or not. A JVM's, whose maps name its libjvm.so (an
	// exiting process's that name none may have let go of them).
	fakeProc(t, proc, "803", "500", []string{"JAVA_TOOL_OPTIONS=-Duser.timezone=" + noZone}, map[string][]byte{"etc/hostname": []byte("app")}, nil)
	write("803", "maps", []byte("7f00-7f80 r-xp 00000000 08:01 42 /opt/java/openjdk/lib/server/libjvm.so\n"))
	stat("803", "4194564", "500", statMem)
	if got, msg := of(zs, 803); got != "!!" {
		t.Fatalf("an exiting JVM's user.timezone Java does not know: %q %s; want the error", got, msg)
	}
	env("803", "UTC")
	stat("803", "4194560", "500", statMem)
	if got, msg := of(zs, 803); got != "UTC" {
		t.Errorf("a process exiting as keploy read it, again: %q %s; want it read again, nothing kept", got, msg)
	}

	// An exec keeps the pid and its start time: its arguments and
	// environment laid out anew, or another executable, is another
	// program, read again; the same, the same program, not.
	fakeProc(t, proc, "804", "500", []string{"TZ=UTC"}, map[string][]byte{"etc/hostname": []byte("app")}, nil)
	if got, msg := of(zs, 804); got != "UTC" {
		t.Fatalf("a wrapper (berglas, chamber) before its exec: %q %s", got, msg)
	}
	env("804", noZone)
	if got, msg := of(zs, 804); got != "UTC" {
		t.Errorf("the same program again: %q %s; want the zone read at its first call", got, msg)
	}
	stat("804", "4194560", "500", "140737488355000 140737488355100 140737488355100 140737488356000")
	if got, msg := of(zs, 804); got != "!!" {
		t.Errorf("an exec, its arguments and environment at new addresses (ASLR): %q %s; want it read again", got, msg)
	}
	env("804", "UTC")
	if err := os.Symlink("/opt/java/openjdk/bin/java", filepath.Join(proc, "804", "exe")); err != nil {
		t.Fatal(err)
	}
	if got, msg := of(zs, 804); got != "UTC" {
		t.Errorf("an exec of another executable, its arguments and environment at the same addresses (ASLR off): %q %s; want it read again", got, msg)
	}
	env("804", noZone)
	if got, msg := of(zs, 804); got != "UTC" {
		t.Errorf("the exec'd program again: %q %s; want the zone read at its first call", got, msg)
	}

	// A process that runs another program as keploy reads it (an exec, or
	// it exits and another takes its number): what keploy read may be
	// either's, so it is an error, never kept; the next call reads the new
	// program.
	for _, row := range []struct {
		what, start, mem string
	}{
		{"an exec as keploy reads it", "500", "140737488357000 140737488357100 140737488357100 140737488358000"},
		{"another process taking its number as keploy reads it", "901", statMem},
	} {
		fakeProc(t, proc, "806", "500", []string{"TZ=UTC"}, map[string][]byte{"etc/hostname": []byte("app")}, nil)
		mid := NewProcZones(proc)
		mid.read = func(root string, pid uint32) (ProcZone, error) {
			z, err := ReadProcZone(root, pid)
			env("806", noZone)
			stat("806", "4194560", row.start, row.mem)
			return z, err
		}
		if got, msg := of(mid, 806); got != "!" || !strings.Contains(msg, "ran another program") {
			t.Errorf("%s: %q %s; want an error read again", row.what, got, msg)
		}
		mid.read = ReadProcZone
		if got, msg := of(mid, 806); got != "!!" {
			t.Errorf("%s, the next call: %q %s; want the new program's read, nothing kept", row.what, got, msg)
		}
	}

	// Errors of the process's own files that no other row reads: lasting.
	big := make([]byte, zoneFileCap+1)
	defer func(n int) { javaZoneinfoMatchCap = n }(javaZoneinfoMatchCap)
	javaZoneinfoMatchCap = 3
	for i, row := range []struct {
		what  string
		argv  string // a JVM's command line; "" no JVM
		files map[string][]byte
		mode  map[string]os.FileMode
	}{
		{"a JVM's /etc/timezone that not every user can read", "java\x00Main\x00", map[string][]byte{"etc/timezone": []byte("UTC\n")}, map[string]os.FileMode{"etc/timezone": 0o600}},
		{"an /etc/localtime of bytes that are no zone file's", "", map[string][]byte{"etc/localtime": []byte("no zone\n")}, nil},
		{"an /etc/localtime larger than any zone file", "", map[string][]byte{"etc/localtime": big}, nil},
		{"a JVM's argfile larger than keploy reads", "java\x00@/app/big.args\x00Main\x00", map[string][]byte{"app/big.args": big}, nil},
		{"a JVM's copied /etc/localtime in a zone directory of more files than keploy searches", "java\x00Main\x00", map[string][]byte{"etc/localtime": []byte("x"), "usr/share/zoneinfo/A": nil, "usr/share/zoneinfo/B": nil, "usr/share/zoneinfo/C": nil, "usr/share/zoneinfo/D": nil}, nil},
	} {
		pid := strconv.Itoa(820 + i)
		fakeProc(t, proc, pid, "500", []string{"PATH=/bin"}, row.files, nil)
		for name, mode := range row.mode {
			if err := os.Chmod(filepath.Join(proc, pid, "root", name), mode); err != nil {
				t.Fatal(err)
			}
		}
		if row.argv != "" {
			write(pid, "cmdline", []byte(row.argv))
			write(pid, "maps", []byte("7f00-7f80 r-xp 00000000 08:01 42 /opt/java/openjdk/lib/server/libjvm.so\n"))
		}
		if z, err := ReadProcZone(proc, uint32(820+i)); err == nil || z.Loc != nil || !isLasting(err) {
			t.Errorf("%s: %v %v (lasting %v); want an error kept for the process's life", row.what, z.Loc, err, isLasting(err))
		}
	}

	// A zone read as the process exits is this call's, never kept.
	fakeProc(t, proc, "831", "500", []string{"TZ=UTC"}, map[string][]byte{"etc/hostname": []byte("app")}, nil)
	write("831", "maps", []byte("7f00-7f80 r-xp 00000000 08:01 42 /opt/java/openjdk/lib/server/libjvm.so\n"))
	stat("831", "4194564", "500", statMem)
	if got, msg := of(zs, 831); got != "UTC" {
		t.Errorf("a JVM exiting as keploy read its zone: %q %s; want its zone", got, msg)
	}
	env("831", noZone)
	stat("831", "4194560", "500", statMem)
	if got, msg := of(zs, 831); got != "!!" {
		t.Errorf("a JVM exiting as keploy read its zone, again: %q %s; want it read again, nothing kept", got, msg)
	}

	// A file keploy cannot read (EACCES, which root never meets) is an
	// error of keploy's own read, read again: never the process's own.
	if os.Geteuid() != 0 {
		fakeProc(t, proc, "830", "500", []string{"TZ=Europe/Berlin"}, map[string][]byte{"usr/share/zoneinfo/Europe/Berlin": nil}, nil)
		dir := filepath.Join(proc, "830", "root", "usr", "share", "zoneinfo", "Europe")
		if err := os.Chmod(dir, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
		if got, msg := of(zs, 830); got != "!" || !strings.Contains(msg, "cannot read") {
			t.Errorf("a TZ whose zone file keploy cannot read: %q %s; want an error read again", got, msg)
		}
	}

	// A stat of no field 51 (a kernel before 3.5) tells no program apart:
	// nothing is kept, and the read is an error.
	fakeProc(t, proc, "805", "500", []string{"TZ=UTC"}, map[string][]byte{"etc/hostname": []byte("app")}, nil)
	write("805", "stat", []byte("805 (app) S 1 1 1 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 4 0 500 1000 100 18446744073709551615"))
	if got, msg := of(zs, 805); got != "!" {
		t.Errorf("a stat of no field 51: %q %s; want an error read again", got, msg)
	}
}

// A JVM writes its clock in its user.timezone, which Java reads over TZ, so
// the agent reads a JVM's zone from the option that sets it, in the order
// the JVM reads them, the last winning: JAVA_TOOL_OPTIONS, then
// JDK_JAVA_OPTIONS (the java launcher's alone), then the command line's own
// options (none after the main class, -jar or -m), then _JAVA_OPTIONS; an
// empty one is none (TZ then). Another program's options are no JVM's (it
// maps no libjvm.so); an id keploy cannot read, and a JVM it cannot tell is
// one, are unknown, never another zone.
func TestAJVMsZoneIsReadFromTheOptionThatSetsIt(t *testing.T) {
	at := time.Date(2026, 1, 28, 7, 30, 0, 0, time.UTC)
	proc := t.TempDir()
	const libjvm = "7f00-7f80 r-xp 00000000 08:01 42 /opt/java/openjdk/lib/server/libjvm.so\n"
	// java writes a process: argv its command line (none: empty, as an
	// exiting one's reads), maps after its executable's line ("-": no maps
	// file, "=": an empty one), files in its root, its working directory
	// /app; a zombie where it is exiting.
	files := map[string][]byte{
		"app/jvm.args":            []byte("# the JVM's options\n-Xmx1g\n\"-Duser.timezone=Asia/Kolkata\" # its zone\n"),
		"app/jib-classpath-file":  []byte("/app/libs/*:/app/classes\n"),
		"app/jib-main-class-file": []byte("com.example.Main\n"),
		"app/cp-and-opts.args":    []byte("/app/classes -Duser.timezone=Europe/Berlin\n"),
		"app/jvm-commented.args":  []byte("-Xmx1g\n# -Duser.timezone=Asia/Kolkata\n"),
	}
	java := func(pid string, env, argv []string, exe, maps string) {
		t.Helper()
		fakeProc(t, proc, pid, "500", env, files, nil)
		dir := filepath.Join(proc, pid)
		cmdline := ""
		if len(argv) > 0 {
			cmdline = strings.Join(argv, "\x00") + "\x00"
		} else if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(statLine(pid, "java", "Z", "4194560", "500", statMem)), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0o644); err != nil {
			t.Fatal(err)
		}
		switch maps {
		case "-":
			if err := os.Remove(filepath.Join(dir, "maps")); err != nil {
				t.Fatal(err)
			}
		case "=":
			if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(statLine(pid, "java", "Z", "4194560", "500", statMem)), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "maps"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		default:
			if err := os.WriteFile(filepath.Join(dir, "maps"), []byte("5600-5610 r-xp 00000000 08:01 7 "+exe+"\n"+maps), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for name, target := range map[string]string{"exe": exe, "cwd": "/app"} {
			if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	const launcher = "/opt/java/openjdk/bin/java"
	jar := []string{"java", "-Xmx1g", "-cp", "/app/lib/*", "-Duser.timezone=Asia/Kolkata", "-jar", "/app/app.jar"}
	for i, row := range []struct {
		what      string
		env, argv []string
		exe, maps string
		want      string // the zone's name; "!" an error read again, "!!" one lasting the process's life (lastingError)
		off       int
	}{
		{"the command line's option, over TZ", []string{"TZ=UTC"}, jar, launcher, libjvm, "+05:30", 19800},
		{"the command line's, over JAVA_TOOL_OPTIONS", []string{"JAVA_TOOL_OPTIONS=-Xss1m -Duser.timezone=America/New_York"}, jar, launcher, libjvm, "+05:30", 19800},
		{"JAVA_TOOL_OPTIONS, over TZ", []string{"TZ=UTC", "JAVA_TOOL_OPTIONS=-Xss1m -Duser.timezone=America/New_York"}, []string{"java", "Main"}, launcher, libjvm, "America/New_York", -18000},
		{"_JAVA_OPTIONS, over the command line", []string{"_JAVA_OPTIONS=-Duser.timezone=Europe/Berlin"}, jar, launcher, libjvm, "Europe/Berlin", 3600},
		{"JDK_JAVA_OPTIONS, over JAVA_TOOL_OPTIONS, in the java launcher", []string{"JDK_JAVA_OPTIONS=-Duser.timezone=Europe/Berlin", "JAVA_TOOL_OPTIONS=-Duser.timezone=America/New_York"}, []string{"java", "Main"}, launcher, libjvm, "Europe/Berlin", 3600},
		{"the command line's, over JDK_JAVA_OPTIONS", []string{"JDK_JAVA_OPTIONS=-Duser.timezone=Europe/Berlin"}, jar, launcher, libjvm, "+05:30", 19800},
		{"JDK_JAVA_OPTIONS in a JVM another program starts: not read", []string{"TZ=UTC", "JDK_JAVA_OPTIONS=-Duser.timezone=Europe/Berlin"}, []string{"/app/bin/server", "Main"}, "/app/bin/server", libjvm, "UTC", 0},
		{"a quoted option", []string{"TZ=UTC", `JAVA_TOOL_OPTIONS=-Dname="a b" '-Duser.timezone=Europe/Berlin'`}, []string{"java", "Main"}, launcher, libjvm, "Europe/Berlin", 3600},
		{"a quoted option holding another: not one of its own", []string{"TZ=UTC", `JAVA_TOOL_OPTIONS="-Dname=x -Duser.timezone=Europe/Berlin"`}, []string{"java", "Main"}, launcher, libjvm, "UTC", 0},
		{"the application's argument after -jar: not the JVM's", []string{"TZ=UTC"}, []string{"java", "-jar", "/app/app.jar", "-Duser.timezone=Asia/Kolkata"}, launcher, libjvm, "UTC", 0},
		{"the application's argument after --module=: not the JVM's", []string{"TZ=UTC"}, []string{"java", "--module=app/com.example.Main", "-Duser.timezone=Asia/Kolkata"}, launcher, libjvm, "UTC", 0},
		{"the application's argument after its main class: not the JVM's", []string{"TZ=UTC"}, []string{"java", "-cp", "-Duser.timezone=Asia/Kolkata", "Main", "-Duser.timezone=Asia/Kolkata"}, launcher, libjvm, "UTC", 0},
		{"the last set empty: none, so TZ", []string{"TZ=UTC", "JAVA_TOOL_OPTIONS=-Duser.timezone=Europe/Berlin"}, []string{"java", "-Duser.timezone=", "Main"}, launcher, libjvm, "UTC", 0},
		{"a custom id", []string{"TZ=UTC"}, []string{"java", "-Duser.timezone=GMT+0530", "Main"}, launcher, libjvm, "+05:30", 19800},
		{"a custom id west of UTC", []string{"TZ=UTC"}, []string{"java", "-Duser.timezone=GMT-3", "Main"}, launcher, libjvm, "-03:00", -10800},
		{"GMT", []string{"JAVA_TOOL_OPTIONS=-Duser.timezone=GMT"}, []string{"java", "Main"}, launcher, libjvm, "UTC", 0},
		{"an IANA link of no slash", []string{"TZ=UTC"}, []string{"java", "-Duser.timezone=CET", "Main"}, launcher, libjvm, "CET", 3600},
		{"an old three-letter id, as Java maps it", []string{"TZ=UTC"}, []string{"java", "-Duser.timezone=IST", "Main"}, launcher, libjvm, "+05:30", 19800},
		{"an old three-letter id of a fixed offset", []string{"TZ=UTC"}, []string{"java", "-Duser.timezone=EST", "Main"}, launcher, libjvm, "-05:00", -18000},
		{"an id keploy does not read: unknown, never TZ's", []string{"TZ=UTC"}, []string{"java", "-Duser.timezone=Mars/Olympus", "Main"}, launcher, libjvm, "!!", 0},
		{"no = sets it empty: none, so TZ", []string{"TZ=UTC", "JAVA_TOOL_OPTIONS=-Duser.timezone=Europe/Berlin"}, []string{"java", "-Duser.timezone", "Main"}, launcher, libjvm, "UTC", 0},
		{"an argfile's option, read in its place, its comments skipped", []string{"TZ=UTC"}, []string{"java", "@/app/jvm.args", "-jar", "/app/app.jar"}, launcher, libjvm, "+05:30", 19800},
		{"an argfile of the working directory", []string{"TZ=UTC"}, []string{"java", "@jvm.args", "Main"}, launcher, libjvm, "+05:30", 19800},
		{"the command line's after an argfile, over it", []string{"TZ=UTC"}, []string{"java", "@/app/jvm.args", "-Duser.timezone=Europe/Berlin", "Main"}, launcher, libjvm, "Europe/Berlin", 3600},
		{"an argfile's commented option: none", []string{"TZ=UTC"}, []string{"java", "@/app/jvm-commented.args", "Main"}, launcher, libjvm, "UTC", 0},
		{"a main class in an argfile (Jib) ends the options", []string{"TZ=UTC", "JAVA_TOOL_OPTIONS=-Duser.timezone=Europe/Berlin"}, []string{"java", "-cp", "@/app/jib-classpath-file", "@/app/jib-main-class-file", "-Duser.timezone=Asia/Kolkata"}, launcher, libjvm, "Europe/Berlin", 3600},
		{"an argfile in a value's place, read before the value is taken", []string{"TZ=UTC"}, []string{"java", "-cp", "@/app/cp-and-opts.args", "Main"}, launcher, libjvm, "Europe/Berlin", 3600},
		{"an argfile that is not there: unknown", []string{"TZ=UTC"}, []string{"java", "@/app/missing.args", "Main"}, launcher, libjvm, "!!", 0},
		{"another program's @ argument: no argfile", []string{"TZ=UTC"}, []string{"/app/server", "@/app/missing.args"}, "/app/server", "", "UTC", 0},
		{"a JVM exiting, its command line read empty: unknown", []string{"TZ=UTC"}, nil, launcher, libjvm, "!", 0},
		{"a JVM exiting, its maps read empty: unknown", []string{"TZ=UTC", "JAVA_TOOL_OPTIONS=-Duser.timezone=Europe/Berlin"}, []string{"java", "Main"}, launcher, "=", "!", 0},
		{"a libjvm.so deleted under it (an upgrade in place): still a JVM", []string{"TZ=UTC", "JAVA_TOOL_OPTIONS=-Duser.timezone=Europe/Berlin"}, []string{"java", "Main"}, launcher + " (deleted)", "7f00-7f80 r-xp 00000000 08:01 42 /opt/java/openjdk/lib/server/libjvm.so (deleted)\n", "Europe/Berlin", 3600},
		{"JDK_JAVA_OPTIONS in Java 8's launcher: not read", []string{"TZ=UTC", "JDK_JAVA_OPTIONS=-Duser.timezone=Europe/Berlin"}, []string{"java", "Main"}, "/usr/lib/jvm/java-8/jre/bin/java", "7f00-7f80 r-xp 00000000 08:01 42 /usr/lib/jvm/java-8/jre/lib/amd64/server/libjvm.so\n", "UTC", 0},
		{"OpenJ9's option variable setting it: unknown (its order is not read)", []string{"TZ=UTC", "OPENJ9_JAVA_OPTIONS=-Duser.timezone=Europe/Berlin"}, []string{"java", "Main"}, launcher, libjvm, "!!", 0},
		{"another program's OpenJ9 variable: not its zone", []string{"TZ=UTC", "OPENJ9_JAVA_OPTIONS=-Duser.timezone=Europe/Berlin"}, []string{"/app/server"}, "/app/server", "", "UTC", 0},
		{"the first of a variable set twice, as getenv reads it", []string{"TZ=UTC", "JAVA_TOOL_OPTIONS=-Duser.timezone=Europe/Berlin", "JAVA_TOOL_OPTIONS=-Duser.timezone=Asia/Kolkata"}, []string{"java", "Main"}, launcher, libjvm, "Europe/Berlin", 3600},
		{"a name of a host's zone directory a JVM does not know: unknown", []string{"TZ=UTC"}, []string{"java", "-Duser.timezone=posix/Asia/Kolkata", "Main"}, launcher, libjvm, "!!", 0},
		{"a custom id Java does not read: unknown", []string{"TZ=UTC"}, []string{"java", "-Duser.timezone=GMT+24:00", "Main"}, launcher, libjvm, "!!", 0},
		{"another program's JAVA_TOOL_OPTIONS: not its zone", []string{"TZ=UTC", "JAVA_TOOL_OPTIONS=-Duser.timezone=Europe/Berlin"}, []string{"/app/server"}, "/app/server", "", "UTC", 0},
		{"an option that sets it, where keploy cannot read whether the process is a JVM: unknown", []string{"TZ=UTC", "JAVA_TOOL_OPTIONS=-Duser.timezone=Europe/Berlin"}, []string{"java", "Main"}, launcher, "-", "!", 0},
	} {
		pid := 200 + i
		java(strconv.Itoa(pid), row.env, row.argv, row.exe, row.maps)
		z, err := ReadProcZone(proc, uint32(pid))
		if strings.HasPrefix(row.want, "!") {
			if err == nil || z.Loc != nil {
				t.Errorf("%s: %v %v; want an error and no zone", row.what, z.Loc, err)
			} else if isLasting(err) != (row.want == "!!") {
				t.Errorf("%s: lasting %v (%v); want %v", row.what, isLasting(err), err, row.want == "!!")
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
	// A name only a host's zone directory holds is refused as no zone a JVM
	// reads, whatever the host holds, never read from the host's files; so
	// is a name whose part starts with no letter, as no region of Java's
	// starts.
	for _, id := range []string{"localtime", "posixrules", "Factory", "posix/Asia/Kolkata", "right/UTC", "+05:30", "-08:00", "+0530", "Etc/_UTC"} {
		if loc, err := javaZone(id); err == nil || loc != nil || !strings.Contains(err.Error(), "is no zone a JVM reads") {
			t.Errorf("user.timezone %q: %v %v; want refused as no zone a JVM reads", id, loc, err)
		}
	}
}

// A JVM whose options set no user.timezone reads its zone as the JDK does,
// not as the C library: TZ where it is set and not empty, read in Java's
// zone data; else the zone /etc/localtime links to, by its link's name;
// else the zone file of the bytes it copies; else GMT. All in the process's
// own root. Where JDKs differ (older ones read /etc/timezone first, and
// read a link uncleaned) and the files make them disagree, and where Java
// does not know the id (it reads the C library's offset, which keploy does
// not), the zone is unknown. Another program with the same files reads them
// as the C library does, as before.
func TestAJVMWithNoZoneOptionReadsItsZoneAsTheJDKDoes(t *testing.T) {
	kolkata, err := os.ReadFile("/usr/share/zoneinfo/Asia/Kolkata")
	if err != nil {
		t.Skipf("no host zone files: %v", err)
	}
	berlin, err := os.ReadFile("/usr/share/zoneinfo/Europe/Berlin")
	if err != nil {
		t.Skipf("no host zone files: %v", err)
	}
	at := time.Date(2026, 1, 28, 7, 30, 0, 0, time.UTC)
	proc := t.TempDir()
	const libjvm = "5600-5610 r-xp 00000000 08:01 7 /opt/java/openjdk/bin/java\n7f00-7f80 r-xp 00000000 08:01 42 /opt/java/openjdk/lib/server/libjvm.so\n"
	zi := func(names ...string) map[string][]byte {
		m := map[string][]byte{}
		for _, n := range names {
			b := kolkata
			if strings.Contains(n, "Berlin") {
				b = berlin
			}
			m["usr/share/zoneinfo/"+n] = b
		}
		return m
	}
	with := func(m map[string][]byte, name string, b []byte) map[string][]byte {
		m[name] = b
		return m
	}
	toKolkata := map[string]string{"etc/localtime": "/usr/share/zoneinfo/Asia/Kolkata"}
	toBerlin := map[string]string{"etc/localtime": "/usr/share/zoneinfo/Europe/Berlin"}
	etc := func(m map[string][]byte) map[string][]byte {
		if m == nil {
			m = map[string][]byte{}
		}
		return with(m, "etc/hostname", []byte("app\n"))
	}
	for i, row := range []struct {
		what  string
		jvm   bool
		env   []string
		argv  []string
		files map[string][]byte
		links map[string]string
		want  string // the zone's name; "!" an error read again, "!!" one lasting the process's life (lastingError)
		off   int
	}{
		{"TZ, in Java's own zone data: an image with no zone files", true, []string{"TZ=Asia/Kolkata"}, nil, nil, nil, "+05:30", 19800},
		{"another program's TZ of no zone file: unknown, as before", false, []string{"TZ=Asia/Kolkata"}, nil, nil, nil, "!!", 0},
		{"TZ with a leading colon", true, []string{"TZ=:Europe/Berlin"}, nil, nil, nil, "Europe/Berlin", 3600},
		{"TZ under posix/, dropped", true, []string{"TZ=posix/Europe/Berlin"}, nil, nil, nil, "Europe/Berlin", 3600},
		{"TZ's colon dropped before posix/", true, []string{"TZ=:posix/Europe/Berlin"}, nil, nil, nil, "Europe/Berlin", 3600},
		{"TZ empty: unset to Java, so /etc/localtime", true, []string{"TZ="}, nil, nil, toBerlin, "Europe/Berlin", 3600},
		{"another program's empty TZ: UTC", false, []string{"TZ="}, nil, nil, toBerlin, "UTC", 0},
		{"an /etc/timezone that names the zone /etc/localtime gives", true, nil, nil, with(zi("Asia/Kolkata"), "etc/timezone", []byte("Asia/Kolkata\n")), toKolkata, "+05:30", 19800},
		{"an /etc/timezone line read to a NUL", true, nil, nil, with(zi("Asia/Kolkata"), "etc/timezone", []byte("Asia/Kolkata\x00Europe/Berlin\n")), toKolkata, "+05:30", 19800},
		{"an /etc/timezone that disagrees with /etc/localtime: unknown (older JDKs read one, newer the other)", true, nil, nil, with(zi("Asia/Kolkata"), "etc/timezone", []byte("Europe/Berlin\n")), toKolkata, "!!", 0},
		{"an /etc/timezone of no /etc/localtime: unknown (GMT to newer JDKs)", true, nil, nil, map[string][]byte{"etc/timezone": []byte("Europe/Berlin\n")}, nil, "!!", 0},
		{"another program reads /etc/localtime, not /etc/timezone", false, nil, nil, with(zi("Asia/Kolkata"), "etc/timezone", []byte("Europe/Berlin\n")), toKolkata, "+05:30", 19800},
		{"an empty /etc/timezone line: /etc/localtime", true, nil, nil, with(zi("Asia/Kolkata"), "etc/timezone", []byte("\nEurope/Berlin\n")), toKolkata, "+05:30", 19800},
		{"an /etc/timezone directory (docker's bind of a missing file): /etc/localtime", true, nil, nil, with(zi("Asia/Kolkata"), "etc/timezone/x", nil), toKolkata, "+05:30", 19800},
		{"the /etc/localtime link's name, to a file the image lacks", true, nil, nil, nil, toKolkata, "+05:30", 19800},
		{"another program's /etc/localtime link to nothing: unknown, as before", false, nil, nil, nil, toKolkata, "!!", 0},
		// A host's /etc/localtime bound over the image's link to Etc/UTC: a
		// JVM reads the link's name, the C library the file's rules.
		{"a link to Etc/UTC whose file holds Kolkata's rules: the zone the link names", true, nil, nil, map[string][]byte{"usr/share/zoneinfo/Etc/UTC": kolkata}, map[string]string{"etc/localtime": "/usr/share/zoneinfo/Etc/UTC"}, "UTC", 0},
		{"another program's link to Etc/UTC whose file holds Kolkata's rules: unknown", false, nil, nil, map[string][]byte{"usr/share/zoneinfo/Etc/UTC": kolkata}, map[string]string{"etc/localtime": "/usr/share/zoneinfo/Etc/UTC"}, "!!", 0},
		{"a relative /etc/localtime link", true, nil, nil, nil, map[string]string{"etc/localtime": "../usr/share/zoneinfo/Europe/Berlin"}, "Europe/Berlin", 3600},
		{"a link newer JDKs clean and older do not: unknown", true, nil, nil, nil, map[string]string{"etc/localtime": "../usr/share//zoneinfo/./Europe/Berlin"}, "!!", 0},
		{"a link's name after its first zoneinfo/", true, nil, nil, nil, map[string]string{"etc/localtime": "/opt/zoneinfo/usr/share/zoneinfo/Europe/Berlin"}, "!!", 0},
		{"a link to nothing that names no zoneinfo/: GMT", true, nil, nil, etc(nil), map[string]string{"etc/localtime": "/etc/missing"}, "UTC", 0},
		{"a copy in a zone directory that links a directory to itself: unknown (the JDK's listing order picks)", true, nil, nil, with(zi("Europe/Berlin"), "etc/localtime", berlin), map[string]string{"usr/share/zoneinfo/posix": "."}, "!!", 0},
		{"a copy matching a dot file the JDK skips", true, nil, nil, with(with(zi("Asia/Kolkata"), "usr/share/zoneinfo/.cache", kolkata), "etc/localtime", kolkata), nil, "+05:30", 19800},
		{"a copied /etc/localtime: the zone file of its bytes, the JDK's skipped names skipped", true, nil, nil, with(with(with(zi("Europe/Berlin", "Asia/Kolkata", "Asia/Calcutta", "posix/Asia/Kolkata"), "usr/share/zoneinfo/posixrules", kolkata), "usr/share/zoneinfo/localtime", kolkata), "etc/localtime", kolkata), nil, "+05:30", 19800},
		{"a link naming no zoneinfo/: the zone file of its bytes", true, nil, nil, with(zi("Europe/Berlin", "Asia/Kolkata"), "etc/static/localtime", berlin), map[string]string{"etc/localtime": "/etc/static/localtime"}, "Europe/Berlin", 3600},
		{"a copied /etc/localtime with no zone directory: GMT", true, nil, nil, map[string][]byte{"etc/localtime": kolkata}, nil, "UTC", 0},
		{"another program reads the copy's own rules", false, nil, nil, map[string][]byte{"etc/localtime": kolkata}, nil, "+05:30", 19800},
		{"a copy of no zone file there: GMT", true, nil, nil, with(zi("Europe/Berlin"), "etc/localtime", kolkata), nil, "UTC", 0},
		{"no /etc/localtime and no /etc/timezone: GMT", true, nil, nil, etc(nil), nil, "UTC", 0},
		{"a root keploy cannot read (an exited process's): unknown, never GMT", true, nil, nil, nil, nil, "!", 0},
		{"an empty user.timezone: the platform's zone, as the JDK reads it", true, []string{"TZ="}, []string{"java", "-Duser.timezone=", "Main"}, nil, toBerlin, "Europe/Berlin", 3600},
		{"an id spelt as no region of Java's", true, []string{"TZ=Europe//Berlin"}, nil, zi("Europe/Berlin"), nil, "!!", 0},
		{"a TZ of a POSIX rule: unknown", true, []string{"TZ=IST-5:30"}, nil, nil, nil, "!!", 0},
		{"a TZ of a path: unknown", true, []string{"TZ=/usr/share/zoneinfo/Asia/Kolkata"}, nil, zi("Asia/Kolkata"), nil, "!!", 0},
		{"an /etc/timezone Java does not know: unknown", true, nil, nil, map[string][]byte{"etc/timezone": []byte("Mars/Olympus\n")}, nil, "!!", 0},
		{"a copy of a zone file Java does not know too: unknown", true, nil, nil, with(with(zi("Asia/Kolkata"), "usr/share/zoneinfo/Mars/Olympus", kolkata), "etc/localtime", kolkata), nil, "!!", 0},
		{"a copy of UTC's file: UTC, found first", true, nil, nil, with(with(zi("Asia/Kolkata"), "usr/share/zoneinfo/UTC", kolkata), "etc/localtime", kolkata), nil, "UTC", 0},
	} {
		pid := strconv.Itoa(600 + i)
		fakeProc(t, proc, pid, "500", row.env, row.files, row.links)
		dir := filepath.Join(proc, pid)
		if row.jvm {
			argv := row.argv
			if argv == nil {
				argv = []string{"java", "Main"}
			}
			if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(argv, "\x00")+"\x00"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "maps"), []byte(libjvm), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/opt/java/openjdk/bin/java", filepath.Join(dir, "exe")); err != nil {
				t.Fatal(err)
			}
		}
		z, err := ReadProcZone(proc, uint32(600+i))
		if strings.HasPrefix(row.want, "!") {
			if err == nil || z.Loc != nil {
				t.Errorf("%s: %v %v; want an error and no zone", row.what, z.Loc, err)
			} else if isLasting(err) != (row.want == "!!") {
				t.Errorf("%s: lasting %v (%v); want %v", row.what, isLasting(err), err, row.want == "!!")
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
// refused, saying so, while a finer one is read as before, and so is a wall
// clock either zone held (models.ZoneEither) whatever its cut: checked at
// its cut in UTC and the agent's zone (2026-10-28), never refused for it.
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
	// the zone learned is read, and an hour's cut must hold in UTC too
	// (2026-10-29): one learned at UTC+05:30, more than an hour from UTC, is
	// refused whatever it is, saying so, the IST hour and the UTC hour alike
	// (TestADateOfNoZoneCheckedInTheStoredZoneHoldsOnlyWhereItHoldsInUTCToo).
	agent := own
	agent.Own = false
	if ok, why := CheckClockRead(wp, o, istHour, agent, true); ok || !strings.Contains(why, "read in a zone at UTC+05:30, the application's zone the learn step read it in") || !strings.Contains(why, "within an hour of UTC: a zone at UTC+05:30 does not, so keploy refuses the value whatever it is") || strings.Contains(why, "+05:30 stands") {
		t.Errorf("learned in IST, the application's zone unread: its IST hour %v %q; want checked in the zone learned and in UTC, and refused, saying so", ok, why)
	}
	if ok, why := CheckClockRead(wp, o, utcHour, agent, true); ok || !strings.Contains(why, "keploy refuses the value whatever it is") || strings.Contains(why, "run it in a zone at UTC+05:30") {
		t.Errorf("learned in IST, the application's zone unread: its UTC hour %v %q; want refused whatever it is, never told to run in IST (which a replay refuses too)", ok, why)
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
	// One cut to an hour is checked at its cut the same, in UTC alone, never
	// refused for its cut (a cut read is checked, 2026-10-28): the meet
	// cannot tell two named zones from a named and an unnamed one, so UTC is
	// the one zone every learning held it in.
	eHour := models.ClockOffset{Res: time.Hour, Zone: models.ZoneEither, Layout: "iso-second-space", AppZone: "+05:30"}.Meet(models.ClockOffset{Res: time.Hour, Zone: models.ZoneEither, Layout: "iso-second-space"})
	if eHour.AppZone != models.AppZoneApart || eHour.Empty() {
		t.Fatalf("two hours' cuts either zone held, named and unnamed, met as %+v; want AppZoneApart", eHour)
	}
	if ok, why := CheckClockRead(wp, eHour, utcHour, inIST, true); !ok {
		t.Errorf("FALSE RED: either zone, an hour's cut learned in two zones, neither zone known: its UTC hour refused (%s); want checked at its cut in UTC, and passed", why)
	}
	if ok, why := CheckClockRead(wp, eHour, istHour, inIST, true); ok || !strings.Contains(why, "it changed") {
		t.Errorf("either zone, an hour's cut learned in two zones, neither zone known: its IST hour %v %q; want refused as changed, checked in UTC alone", ok, why)
	}
	if ok, why := CheckClockRead(wp, eHour, wall(time.UTC, hourIn(time.UTC, now.Add(-3*time.Hour))), inIST, true); ok || !strings.Contains(why, "it changed") {
		t.Errorf("either zone, an hour's cut learned in two zones, neither zone known: a UTC hour three hours stale %v %q; want refused as changed", ok, why)
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

// A zone file a file of another zone is mounted over is unknown, read from
// the process's own mounts (models.MountAt), where its rules agree with
// keploy's own zone data of its name: where the process shares keploy's root
// (native mode, keploy's own zone) keploy's zone data is the very file, so
// the rules alone never differ there. A host's /etc/localtime bind-mounted
// over an image's link to Etc/UTC (-v /etc/localtime:/etc/localtime:ro,
// which lands on the file the link names) is named by the link while the C
// library, Go and Python read the host's zone. Each row's Etc/UTC file holds
// UTC's rules, as keploy's own zone data gives them, so only the mount
// tells. A mount of the same zone's file, of another of IANA's names of UTC,
// or of a directory above the file is the zone the path names; mounts the
// process cannot have kept (hidden by a later one) are not read; a mount
// table keploy cannot read is an error read again.
func TestAZoneFileMountedFromAnotherZonesIsUnknown(t *testing.T) {
	read := func(name string) []byte {
		b, err := os.ReadFile("/usr/share/zoneinfo/" + name)
		if err != nil {
			t.Skipf("no host zone files: %v", err)
		}
		return b
	}
	utc := read("Etc/UTC")
	zi := "usr/share/zoneinfo/"
	toUTC := map[string]string{"etc/localtime": "/" + zi + "Etc/UTC"}
	files := map[string][]byte{zi + "Etc/UTC": utc}
	const at = " /usr/share/zoneinfo/Etc/UTC ro,relatime - xfs /dev/sda1 rw\n"
	fromKolkata := rootMount + "700 600 252:0 /usr/share/zoneinfo/Asia/Kolkata" + at
	proc := t.TempDir()
	for _, row := range []struct {
		what   string
		pid    string
		env    []string
		files  map[string][]byte
		links  map[string]string
		mounts string // the mount table; "-" none
		want   string // the zone's name (its Location's), "!!" a lasting error, "!" one read again
		why    string // a text the error holds
	}{
		{"a link to Etc/UTC whose file is mounted from Kolkata's (the host's /etc/localtime)", "901", nil, files, toUTC, fromKolkata,
			"!!", `the zone file /usr/share/zoneinfo/Etc/UTC, which names "Etc/UTC" by its path, is a file mounted from /usr/share/zoneinfo/Asia/Kolkata, the zone file of "Asia/Kolkata". Node and a JVM read the zone by the name "Etc/UTC", the C library, Go and Python by the mounted file's rules, so keploy reads the application's zone as unknown`},
		{"the remedy names the mounted file's zone", "912", nil, files, toUTC, fromKolkata, "!!", "set TZ to the zone the application runs in (TZ=Asia/Kolkata for the mounted file's zone), or drop the mount"},
		{"mounted from a host's copy, whose path names no zone", "902", nil, files, toUTC, rootMount + "700 600 252:0 /etc/localtime" + at,
			"!!", `is a file mounted from /etc/localtime, a file whose path names no zone (a copy)`},
		{"mounted from the same zone's file in a host's subvolume", "903", nil, files, toUTC, rootMount + "700 600 0:40 /root/usr/share/zoneinfo/Etc/UTC" + at, "Etc/UTC", ""},
		{"mounted from another of IANA's names of UTC (a Fedora host's UTC)", "904", nil, files, toUTC, rootMount + "700 600 252:0 /usr/share/zoneinfo/UTC" + at, "Etc/UTC", ""},
		{"a host's zoneinfo directory mounted above the file", "905", nil, files, toUTC, rootMount + "700 600 252:0 /usr/share/zoneinfo /usr/share/zoneinfo ro - xfs /dev/sda1 rw\n", "Etc/UTC", ""},
		{"Kolkata's file mounted, then a directory mounted above it, which hides it", "906", nil, files, toUTC, fromKolkata + "701 600 252:0 /usr/share/zoneinfo /usr/share/zoneinfo ro - xfs /dev/sda1 rw\n", "Etc/UTC", ""},
		{"the same zone's file mounted, then Kolkata's over it", "907", nil, files, toUTC, rootMount + "699 600 252:0 /usr/share/zoneinfo/Etc/UTC" + at + "700 699 252:0 /usr/share/zoneinfo/Asia/Kolkata" + at,
			"!!", `mounted from /usr/share/zoneinfo/Asia/Kolkata`},
		{"TZ=Etc/UTC, whose file is mounted from Kolkata's", "908", []string{"TZ=Etc/UTC"}, files, nil, fromKolkata, "!!", `mounted from /usr/share/zoneinfo/Asia/Kolkata`},
		{"a copied /etc/localtime that /etc/timezone names Etc/UTC by, whose file is mounted from Kolkata's", "909", nil,
			map[string][]byte{"etc/localtime": utc, "etc/timezone": []byte("Etc/UTC\n"), zi + "Etc/UTC": utc}, nil, fromKolkata, "!!", `mounted from /usr/share/zoneinfo/Asia/Kolkata`},
		{"a copied /etc/localtime that /etc/timezone names Etc/UTC by, nothing mounted", "910", nil,
			map[string][]byte{"etc/localtime": utc, "etc/timezone": []byte("Etc/UTC\n"), zi + "Etc/UTC": utc}, nil, rootMount, "Etc/UTC", ""},
		{"mounted from the same zone's file, since replaced on the host", "913", nil, files, toUTC, rootMount + "700 600 252:0 /usr/share/zoneinfo/Etc/UTC//deleted" + at, "Etc/UTC", ""},
		{"mounted from Kolkata's file, since replaced on the host", "914", nil, files, toUTC, rootMount + "700 600 252:0 /usr/share/zoneinfo/Asia/Kolkata//deleted" + at,
			"!!", `is a file mounted from /usr/share/zoneinfo/Asia/Kolkata, the zone file of "Asia/Kolkata", since replaced on the host. Node and a JVM`},
		{"its remedy names the zone", "915", nil, files, toUTC, rootMount + "700 600 252:0 /usr/share/zoneinfo/Asia/Kolkata//deleted" + at, "!!", "(TZ=Asia/Kolkata for the mounted file's zone)"},
		{"a mount table keploy cannot read", "911", nil, files, toUTC, "-", "!", "keploy cannot read the mounts of the process's root"},
	} {
		fakeProc(t, proc, row.pid, "500", row.env, row.files, row.links)
		mi := filepath.Join(proc, row.pid, "mountinfo")
		if row.mounts == "-" {
			if err := os.Remove(mi); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(mi, []byte(row.mounts), 0o644); err != nil {
			t.Fatal(err)
		}
		pid, _ := strconv.ParseUint(row.pid, 10, 32)
		z, err := ReadProcZone(proc, uint32(pid))
		got := ""
		switch {
		case err != nil && isLasting(err):
			got = "!!"
		case err != nil:
			got = "!"
		default:
			got = z.Loc.String()
		}
		if got != row.want || row.why != "" && (err == nil || !strings.Contains(err.Error(), row.why)) {
			t.Errorf("%s: %q, %v; want %q holding %q", row.what, got, err, row.want, row.why)
		}
	}
	// keploy's own zone reads keploy's own mounts; a process's root its own.
	for _, c := range [][4]string{
		{"/", "/usr/share/zoneinfo/Etc/UTC", "/proc/self/mountinfo", "/usr/share/zoneinfo/Etc/UTC"},
		{"/proc/42/root", "/usr/share/zoneinfo/Etc/UTC", "/proc/42/mountinfo", "/usr/share/zoneinfo/Etc/UTC"},
		{"/var/tmp/image", "/etc/localtime", "/proc/self/mountinfo", "/var/tmp/image/etc/localtime"},
	} {
		if table, p := zoneMountTable(c[0], c[1]); table != c[2] || p != c[3] {
			t.Errorf("the mounts of %s at %s: %s at %s; want %s at %s", c[0], c[1], table, p, c[2], c[3])
		}
	}
}
