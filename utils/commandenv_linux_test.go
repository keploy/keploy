//go:build linux

package utils

import (
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// A keploy started in secure mode gives the command it starts its environment
// as keploy was started with it, where glibc's loader or Go's runtime changed
// it before keploy's code ran, and as keploy has it otherwise: keploy's own
// changes since, sets and unsets alike, stay keploy's.
func TestWithInitialOfUndoesWhatChangedBeforeKeploysCodeRan(t *testing.T) {
	for _, tc := range []struct {
		name                string
		env, start, initial []string
		want                []string
	}{
		{
			name:    "not started in secure mode: keploy's environment as it is",
			env:     []string{"A=1", "GOTRACEBACK=none"},
			initial: []string{"A=2"},
			want:    []string{"A=1", "GOTRACEBACK=none"},
		},
		{
			name:    "the runtime's GOTRACEBACK, where keploy was started with none, is dropped",
			env:     []string{"A=1", "GOTRACEBACK=none", "B=2"},
			start:   []string{"A=1", "B=2", "GOTRACEBACK=none"},
			initial: []string{"A=1", "B=2"},
			want:    []string{"A=1", "B=2"},
		},
		{
			name:    "the GOTRACEBACK keploy was started with is the command's",
			env:     []string{"GOTRACEBACK=none", "A=1"},
			start:   []string{"GOTRACEBACK=none", "A=1"},
			initial: []string{"GOTRACEBACK=crash", "A=1"},
			want:    []string{"A=1", "GOTRACEBACK=crash"},
		},
		{
			name:    "what the loader removed comes back, in the order keploy was started with it",
			env:     []string{"A=1", "B=2"},
			start:   []string{"A=1", "B=2"},
			initial: []string{"TMPDIR=/t", "A=1", "LD_LIBRARY_PATH=/l", "B=2", "LD_PRELOAD=/p.so"},
			want:    []string{"A=1", "B=2", "TMPDIR=/t", "LD_LIBRARY_PATH=/l", "LD_PRELOAD=/p.so"},
		},
		{
			name:    "a variable the loader removed and keploy then set is keploy's",
			env:     []string{"MALLOC_ARENA_MAX=4"},
			start:   []string{},
			initial: []string{"MALLOC_ARENA_MAX=2"},
			want:    []string{"MALLOC_ARENA_MAX=4"},
		},
		{
			name:    "a GOTRACEBACK keploy set itself is keploy's",
			env:     []string{"GOTRACEBACK=crash"},
			start:   []string{"GOTRACEBACK=none"},
			initial: []string{},
			want:    []string{"GOTRACEBACK=crash"},
		},
		{
			name:    "keploy's own sets and unsets stand",
			env:     []string{"KEPLOY_LATER=1", "TESTSETID=test-set-0"},
			start:   []string{"KEPLOY_DROPPED=1", "TESTSETID=x"},
			initial: []string{"KEPLOY_DROPPED=1", "TESTSETID=x"},
			want:    []string{"KEPLOY_LATER=1", "TESTSETID=test-set-0"},
		},
		{
			name:    "an entry merely starting alike is its own",
			env:     []string{"GOTRACEBACKX=1", "GOTRACEBACK=none"},
			start:   []string{"GOTRACEBACKX=1", "GOTRACEBACK=none"},
			initial: []string{"GOTRACEBACKX=1"},
			want:    []string{"GOTRACEBACKX=1"},
		},
		{
			name:    "a variable the loader removed twice comes back once, with its first value, as os.Getenv reads it",
			env:     []string{"A=1"},
			start:   []string{"A=1"},
			initial: []string{"TMPDIR=/a", "A=1", "TMPDIR=/b"},
			want:    []string{"A=1", "TMPDIR=/a"},
		},
		{
			name:    "nothing changed before keploy's code ran: keploy's environment as it is",
			env:     []string{"B=2", "A=1"},
			start:   []string{"A=1", "B=2"},
			initial: []string{"A=1", "B=2"},
			want:    []string{"B=2", "A=1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := withInitialOf(tc.env, tc.start, tc.initial); !slices.Equal(got, tc.want) {
				t.Errorf("withInitialOf(%q, %q, %q) = %q, want %q", tc.env, tc.start, tc.initial, got, tc.want)
			}
		})
	}
}

// Only a file capability makes keploy give its commands back what the loader
// removed: a process that is not root, with its user's own ids, holding
// capabilities. Not root, not a set-user-ID or set-group-ID keploy, not a
// process with nothing more than its user's own.
func TestFileCapable(t *testing.T) {
	const sysAdmin = 1 << 21
	for _, tc := range []struct {
		name                               string
		ruid, euid, suid, rgid, egid, sgid int
		permitted                          uint64
		ambient                            bool
		want                               bool
	}{
		{"a user's keploy with cap_sys_admin+p", 1000, 1000, 1000, 1000, 1000, 1000, sysAdmin, false, true},
		{"a user's keploy with no capability", 1000, 1000, 1000, 1000, 1000, 1000, 0, false, false},
		{"a user's keploy with an ambient capability, which its commands keep", 1000, 1000, 1000, 1000, 1000, 1000, sysAdmin, true, false},
		{"root", 0, 0, 0, 0, 0, 0, 0x1ffffffffff, false, false},
		{"set-user-ID root", 1000, 0, 0, 1000, 1000, 1000, 0x1ffffffffff, false, false},
		{"set-user-ID another user, with a capability", 1000, 1001, 1001, 1000, 1000, 1000, sysAdmin, false, false},
		{"set-group-ID", 1000, 1000, 1000, 1000, 50, 50, sysAdmin, false, false},
	} {
		if got := fileCapable(tc.ruid, tc.euid, tc.suid, tc.rgid, tc.egid, tc.sgid, tc.permitted, tc.ambient); got != tc.want {
			t.Errorf("%s: fileCapable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// environBounds reads env_start and env_end from a /proc/PID/stat, past a
// command name holding blanks and parentheses of its own, and refuses one
// that shows none (a reader without access to the process sees 0s).
func TestEnvironBounds(t *testing.T) {
	stat := func(comm string, start, end uint64) []byte {
		f := make([]string, 0, 52)
		f = append(f, "4242", "("+comm+")", "S")
		for i := 4; i <= 49; i++ {
			f = append(f, strconv.Itoa(i))
		}
		f = append(f, strconv.FormatUint(start, 10), strconv.FormatUint(end, 10), "0")
		return []byte(strings.Join(f, " ") + "\n")
	}
	start, end, err := environBounds(stat("a (b) c", 140737488347000, 140737488351000))
	if err != nil || start != 140737488347000 || end != 140737488351000 {
		t.Errorf("environBounds = %d, %d, %v; want 140737488347000, 140737488351000", start, end, err)
	}
	if _, _, err := environBounds(stat("keploy", 0, 0)); err == nil {
		t.Error("environBounds took a stat that shows no environment")
	}
	if _, _, err := environBounds([]byte("4242 (keploy) S 1 2 3")); err == nil {
		t.Error("environBounds took a stat cut short")
	}
}

// keploy reads the environment it was started with from its own memory, the
// very bytes /proc/self/environ holds (which this test process, not started
// in secure mode, may read).
func TestInitialEnvironIsWhatProcSelfEnvironHolds(t *testing.T) {
	b, err := os.ReadFile("/proc/self/environ")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
	got, err := initialEnviron()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("initialEnviron() = %q, want /proc/self/environ's %q", got, want)
	}
}

// A process with no more than its user's own (as go test runs this) reads
// nothing of its launch, and the command's environment is keploy's, entry
// for entry.
func TestCommandEnvironWithoutAFileCapabilityIsKeploysOwn(t *testing.T) {
	permitted, err := permittedCaps()
	if err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 && permitted != 0 {
		t.Skip("this test binary holds capabilities")
	}
	if launch.start != nil || launch.initial != nil || launch.err != nil {
		t.Errorf("keploy, holding no file capability, read its launch: %+v", launch)
	}
	t.Setenv("GOTRACEBACK", "none") // set after the start: still keploy's own
	if got := commandEnviron(zap.NewNop()); !slices.Equal(got, os.Environ()) {
		t.Errorf("commandEnviron() = %q, want os.Environ() = %q", got, os.Environ())
	}
}
