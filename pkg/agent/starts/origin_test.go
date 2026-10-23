package starts

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// commander is the fake Proc with process names (Commander), counting reads.
type commander struct {
	*fake
	comm  map[uint32]string
	argv  map[uint32][]string
	reads int
}

func (c *commander) Command(pid uint32) (string, []string, bool) {
	c.reads++
	comm, argv := c.comm[pid], c.argv[pid]
	return comm, argv, comm != "" || len(argv) > 0
}

func newCommander() *commander {
	return &commander{fake: newFake(), comm: map[uint32]string{}, argv: map[uint32][]string{}}
}

// A recorded mock names the process its connection came from (its comm and
// argv[0]), its origin's clock kept and its origin replaced whole; the whole
// command line, its secrets redacted, is logged once per process at debug.
// A mock whose connection was noted with no process, or whose process cannot
// be read, is left as it is.
func TestStampOriginNamesTheConnectionsProcess(t *testing.T) {
	c := newCommander()
	c.born[42] = at(0)
	c.comm[42], c.argv[42] = "sqs", []string{"./sqs", "--queue", "orders", "--password", "hunter2"}
	r := New(c, 0)
	core, logs := observer.New(zapcore.DebugLevel)
	r.SetLogger(zap.New(core))
	r.NoteOrigin("7", 42)
	r.NoteOrigin("8", 0)

	shared := &models.Origin{Clock: "proxy:run-1"}
	m := &models.Mock{Name: "mock-6", Spec: models.MockSpec{Metadata: map[string]string{"connID": "7"}}, Origin: shared}
	r.StampOrigin(m)
	if m.Origin == nil || *m.Origin != (models.Origin{Clock: "proxy:run-1", Comm: "sqs", Exe: "./sqs"}) {
		t.Fatalf("origin %+v; want the clock kept and sqs, ./sqs", m.Origin)
	}
	if shared.Comm != "" {
		t.Fatal("the origin a copy shares was edited in place")
	}
	again := &models.Mock{Name: "mock-7", ConnectionID: "7"}
	r.StampOrigin(again)
	if again.Origin == nil || again.Origin.Comm != "sqs" || again.Origin.Clock != "" {
		t.Fatalf("a second mock of the connection has origin %+v", again.Origin)
	}
	if c.reads != 1 {
		t.Fatalf("the process was read %d times; want once per connection", c.reads)
	}
	if n := logs.FilterMessage("a recorded call came from this process").Len(); n != 1 {
		t.Fatalf("the command line was logged %d times; want once", n)
	}
	line := logs.FilterMessage("a recorded call came from this process").All()[0].ContextMap()["cmdline"].(string)
	if strings.Contains(line, "hunter2") || !strings.Contains(line, "./sqs --queue orders --password") {
		t.Fatalf("the logged command line is %q; want it whole, its password redacted", line)
	}
	for _, mk := range []*models.Mock{{Name: "no-pid", ConnectionID: "8"}, {Name: "unknown", ConnectionID: "9"}} {
		r.StampOrigin(mk)
		if mk.Origin != nil {
			t.Fatalf("%s got origin %+v", mk.Name, mk.Origin)
		}
	}
	// A mock that names its process already keeps it; its SourcePID names
	// one when its connection does not.
	named := &models.Mock{ConnectionID: "7", Origin: &models.Origin{Comm: "other"}}
	r.StampOrigin(named)
	if named.Origin.Comm != "other" {
		t.Fatalf("a named origin was renamed %+v", named.Origin)
	}
	c.comm[43], c.argv[43] = "worker", []string{"/usr/bin/worker"}
	bySource := &models.Mock{SourcePID: 43}
	r.StampOrigin(bySource)
	if bySource.Origin == nil || bySource.Origin.Exe != "/usr/bin/worker" {
		t.Fatalf("a mock with only a SourcePID got origin %+v", bySource.Origin)
	}
}

// A pid reused by another process names that one (its birth differs); a
// connection closed is kept for a late mock, among the last ones closed; a
// connection ID noted again while open is closed only by its last forget.
func TestOriginsFollowTheProcessAndTheConnection(t *testing.T) {
	c := newCommander()
	c.born[42] = at(0)
	c.comm[42] = "first"
	r := New(c, 0)
	if comm, _, ok := r.ProcessOf(42); !ok || comm != "first" {
		t.Fatalf("ProcessOf(42) = %q, %v", comm, ok)
	}
	c.born[42], c.comm[42] = at(1000), "second"
	if comm, _, _ := r.ProcessOf(42); comm != "second" {
		t.Fatalf("a reused pid names %q; want the new process", comm)
	}

	r.NoteOrigin("1", 42)
	r.NoteOrigin("1", 42)
	r.ForgetOrigin("1")
	r.ForgetOrigin("1")
	late := &models.Mock{ConnectionID: "1"}
	r.StampOrigin(late)
	if late.Origin == nil || late.Origin.Comm != "second" {
		t.Fatalf("a late mock of a closed connection got %+v", late.Origin)
	}
	for i := 0; i < keptClosedOrigins; i++ {
		id := "c" + strings.Repeat("x", 1) + time.Duration(i).String()
		r.NoteOrigin(id, 42)
		r.ForgetOrigin(id)
	}
	gone := &models.Mock{ConnectionID: "1"}
	r.StampOrigin(gone)
	if gone.Origin != nil {
		t.Fatalf("a connection closed before the last %d kept its process: %+v", keptClosedOrigins, gone.Origin)
	}
	r.Reset()
	if _, ok := r.origin.conns["c"+"x"+time.Duration(1).String()]; ok {
		t.Fatal("Reset kept the connections' processes")
	}
}

// Stamp stamps the origin's process under the proven switch only.
func TestStampNamesTheProcessUnderTheSwitchOnly(t *testing.T) {
	c := newCommander()
	c.comm[42] = "sqs"
	r := New(c, 0)
	r.NoteOrigin("7", 42)
	t.Setenv(provenswitch.Env, "")
	off := &models.Mock{ConnectionID: "7"}
	r.Stamp(off)
	if off.Origin != nil {
		t.Fatalf("without the switch Stamp gave the mock origin %+v", off.Origin)
	}
	t.Setenv(provenswitch.Env, "1")
	on := &models.Mock{ConnectionID: "7"}
	r.Stamp(on)
	if on.Origin == nil || on.Origin.Comm != "sqs" {
		t.Fatalf("under the switch Stamp gave the mock origin %+v", on.Origin)
	}
}

// The registry keeps the names of at most keptProcessNames processes: one
// read past that empties the cache, and is read again when asked for again.
func TestTheProcessNameCacheIsBounded(t *testing.T) {
	c := newCommander()
	r := New(c, 0)
	for pid := uint32(1); pid <= keptProcessNames+1; pid++ {
		c.comm[pid] = "worker"
		c.born[pid] = at(int(pid))
		if _, _, ok := r.ProcessOf(pid); !ok {
			t.Fatalf("process %d was not named", pid)
		}
	}
	r.origin.mu.Lock()
	n := len(r.origin.named)
	r.origin.mu.Unlock()
	if n > keptProcessNames {
		t.Fatalf("the cache holds %d process names; want at most %d", n, keptProcessNames)
	}
	reads := c.reads
	if comm, _, ok := r.ProcessOf(1); !ok || comm != "worker" {
		t.Fatalf("an evicted process is named %q, %v", comm, ok)
	}
	if c.reads != reads+1 {
		t.Fatalf("an evicted process was read %d times again; want once", c.reads-reads)
	}
}

// A committed file holds argv[0] with each word that carries a secret
// redacted, as a command line's argument is: a process that sets its own title
// puts a whole command line there, and a path may have a space in it.
func TestAnOriginsExeIsArgv0Redacted(t *testing.T) {
	for _, tc := range []struct{ argv0, want string }{
		{"./sqs", "./sqs"},
		{"/usr/bin/python3", "/usr/bin/python3"},
		{"/Applications/My Tool.app/Contents/MacOS/tool", "/Applications/My Tool.app/Contents/MacOS/tool"},
		{"[celeryd: worker@host:MainProcess] -active- (worker --broker=amqp://user:s3cret@mq)", "[celeryd: worker@host:MainProcess] -active- (worker " + containerkey.RedactedText},
		{"amqp://user:s3cret@mq", containerkey.RedactedText},
	} {
		got := exeOf(tc.argv0)
		if got != tc.want {
			t.Errorf("exeOf(%q) = %q; want %q", tc.argv0, got, tc.want)
		}
		if strings.Contains(got, "s3cret") {
			t.Errorf("exeOf(%q) keeps the secret: %q", tc.argv0, got)
		}
	}
	c := newCommander()
	c.comm[7] = "celery"
	c.argv[7] = []string{"[celeryd: w] (worker --broker=amqp://user:s3cret@mq)", "worker"}
	r := New(c, 0)
	if _, exe, ok := r.ProcessOf(7); !ok || exe != "[celeryd: w] (worker "+containerkey.RedactedText {
		t.Fatalf("a titled process's exe is %q, %v; want its title, redacted", exe, ok)
	}
}

// A program a go run built is named by that go run, `go run` and its
// target, never by the binary's path in the go tool's build dir, which
// differs on every record: two records of the same app write the same reason
// line and the same origin. Its build flags are never named (a -ldflags
// value can carry a key). The go tool itself, a program another process
// started, and one whose parent cannot be read keep their argv[0].
func TestAGoRunProgramIsNamedByItsGoRun(t *testing.T) {
	record := func(dir string, goPid, appPid uint32, goArgv ...string) (*models.Origin, string) {
		c := newCommander()
		c.spawn(goPid, 1, at(0), "/usr/local/go/bin/go")
		c.spawn(appPid, goPid, at(5), dir+"/b001/exe/orders")
		c.comm[goPid], c.argv[goPid] = "go", goArgv
		c.comm[appPid], c.argv[appPid] = "orders", []string{dir + "/b001/exe/orders", "--port", "8080"}
		r := New(c, 0)
		r.NoteOrigin("7", appPid)
		m := &models.Mock{Name: "mock-1", Spec: models.MockSpec{Metadata: map[string]string{"connID": "7"}}, Origin: &models.Origin{Clock: "proxy:run-1"}}
		r.StampOrigin(m)
		return m.Origin, models.NewWindowedMock(m).FileProcess()
	}
	first, firstLine := record("/tmp/go-build2463177086", 300, 301, "go", "run", "-ldflags=-X=main.key=s3cret", ".")
	second, secondLine := record("/tmp/go-build981100412", 900, 912, "/usr/local/go/bin/go", "run", "-ldflags=-X=main.key=s3cret", ".")
	if want := "`orders` (`go run .`)"; firstLine != want || secondLine != firstLine {
		t.Fatalf("the two records name the app %q and %q; want both %q", firstLine, secondLine, want)
	}
	if first == nil || second == nil || *first != *second || first.Exe != "go run ." {
		t.Fatalf("the two records' origins are %+v and %+v; want both go run .", first, second)
	}

	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"go", "run", "."}, "go run ."},
		{[]string{"go", "run", "./cmd/server", "--port", "8080"}, "go run ./cmd/server"},
		{[]string{"go", "run", "main.go", "handlers.go", "serve", "x.go"}, "go run main.go handlers.go"},
		{[]string{"go", "run", "-race", "-tags", "dev", "-ldflags", "-X main.key=s3cret", "./cmd/server"}, "go run ./cmd/server"},
		{[]string{"go", "run", "-C", "svc", "--gcflags=all=-N -l", "--exec", "qemu", "-mod=vendor", "."}, "go run ."},
		{[]string{"go", "run", "-v", "--", "./cmd/x", "-v"}, "go run ./cmd/x"},
		{[]string{"go", "run", "example.com/tool", "--token", "s3cret"}, "go run example.com/tool"},
		// Its target is redacted as argv[0] is: a word that reads as a
		// credential (user/pw@host) is never committed, a module's
		// version included.
		{[]string{"go", "run", "example.com/tool@v1.2.0"}, "go run " + containerkey.RedactedText},
		// A flag keploy does not know may take the next word as its
		// value: no word is named.
		{[]string{"go", "run", "-future", "s3cret", "."}, "go run"},
		{[]string{"go", "run", "-tags"}, "go run"},
	} {
		got, ok := goRunOf(tc.argv)
		if !ok || got != tc.want {
			t.Errorf("goRunOf(%q) = %q, %v; want %q", tc.argv, got, ok, tc.want)
		}
		if strings.Contains(got, "s3cret") {
			t.Errorf("goRunOf(%q) names a flag's value: %q", tc.argv, got)
		}
	}
	for _, argv := range [][]string{nil, {"go"}, {"go", "build", "."}, {"gofmt", "run", "."}, {"./run", "run", "."}} {
		if got, ok := goRunOf(argv); ok {
			t.Errorf("goRunOf(%q) = %q; want no go run", argv, got)
		}
	}

	c := newCommander()
	c.spawn(300, 1, at(0), "/usr/local/go/bin/go")
	c.comm[300], c.argv[300] = "go", []string{"go", "run", "."}
	c.spawn(301, 300, at(5), "/tmp/go-build1/b001/exe/orders")
	c.comm[301], c.argv[301] = "orders", []string{"/tmp/go-build1/b001/exe/orders"}
	// The app's own child, and a process whose parent cannot be read.
	c.spawn(302, 301, at(9), "/tmp/go-build1/b001/exe/orders")
	c.comm[302], c.argv[302] = "orders", []string{"/tmp/go-build1/b001/exe/orders", "worker"}
	c.comm[303], c.argv[303] = "orders", []string{"/tmp/go-build1/b001/exe/orders"}
	r := New(c, 0)
	for pid, want := range map[uint32]string{300: "go", 301: "go run .", 302: "/tmp/go-build1/b001/exe/orders", 303: "/tmp/go-build1/b001/exe/orders"} {
		if _, exe, ok := r.ProcessOf(pid); !ok || exe != want {
			t.Errorf("pid %d is named %q, %v; want %q", pid, exe, ok, want)
		}
	}
}

// A process whose birth cannot be read is not cached: a later process reusing
// its pid is named as itself. Its command line is logged all the same, once
// per pid, and again for a reused pid's other command line.
func TestANameWithNoBirthIsNotKept(t *testing.T) {
	c := newCommander()
	r := New(c, 0)
	core, logs := observer.New(zapcore.DebugLevel)
	r.SetLogger(zap.New(core))
	c.comm[11], c.argv[11] = "first", []string{"./first", "--token", "s3cret"}
	if comm, _, _ := r.ProcessOf(11); comm != "first" {
		t.Fatalf("pid 11 is %q", comm)
	}
	if comm, _, _ := r.ProcessOf(11); comm != "first" {
		t.Fatalf("pid 11 is %q the second time", comm)
	}
	lines := logs.FilterMessage("a recorded call came from this process")
	if lines.Len() != 1 {
		t.Fatalf("the command line of a process with no birth was logged %d times; want once", lines.Len())
	}
	if line := lines.All()[0].ContextMap()["cmdline"].(string); strings.Contains(line, "s3cret") || !strings.HasPrefix(line, "./first --token") {
		t.Fatalf("the logged command line is %q; want it whole, its token redacted", line)
	}
	c.comm[11], c.argv[11] = "second", []string{"./second"} // pid 11 reused; its birth still unknown
	if comm, _, _ := r.ProcessOf(11); comm != "second" {
		t.Fatalf("a reused pid with no birth is named %q; want second", comm)
	}
	if n := logs.FilterMessage("a recorded call came from this process").Len(); n != 2 {
		t.Fatalf("a reused pid's other command line was logged %d times in all; want 2", n)
	}
}

// A read of a process's name that fails is not kept: the next ask reads
// again, and a connection's name is set only once one is read.
func TestAFailedNameReadIsTriedAgain(t *testing.T) {
	c := newCommander()
	r := New(c, 0)
	if _, _, ok := r.ProcessOf(9); ok {
		t.Fatal("a process with no readable name was named")
	}
	c.comm[9] = "consumer"
	if comm, _, ok := r.ProcessOf(9); !ok || comm != "consumer" {
		t.Fatalf("after its name became readable the process is %q, %v", comm, ok)
	}
	r.NoteOrigin("conn-1", 10)
	if _, ok := r.connName("conn-1", 0); ok {
		t.Fatal("a connection whose process cannot be read was named")
	}
	c.comm[10] = "worker"
	if n, ok := r.connName("conn-1", 0); !ok || n.comm != "worker" {
		t.Fatalf("the connection is named %+v, %v once its process can be read", n, ok)
	}
}
