package starts

import (
	"path/filepath"
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
// argv[0]), its origin's clock kept and its origin replaced whole; the
// command line is logged once per process at debug, by its shape (its
// program, its flags' names and its numbers kept), its secrets redacted.
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
	var keys *models.LogKeySession // no session was set: the process's key
	if want := "sqs --queue '{} " + keys.LogKey("orders") + "' --password <redacted>"; line != want {
		t.Fatalf("the logged command line is %q; want %q, its password redacted", line, want)
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

// A test binary a go test built runs from the go tool's work dir for that one
// go test, /tmp/go-build<random>/b001/<pkg>.test: keploy names it by its go
// test and its packages, the same for two runs of the same command, never
// its build or test flags, so the record's reason lines do not change from
// one record to the next.
func TestAGoTestBinaryIsNamedByItsGoTest(t *testing.T) {
	record := func(dir string, goPid, testPid uint32, goArgv ...string) (*models.Origin, string) {
		c := newCommander()
		c.spawn(goPid, 1, at(0), "/usr/local/go/bin/go")
		c.spawn(testPid, goPid, at(5), dir+"/b001/store.test")
		c.comm[goPid], c.argv[goPid] = "go", goArgv
		c.comm[testPid], c.argv[testPid] = "store.test", []string{dir + "/b001/store.test", "-test.paniconexit0", "-test.timeout=10m0s", "-test.v=true", "-test.run=TestOrders"}
		r := New(c, 0)
		r.NoteOrigin("7", testPid)
		m := &models.Mock{Name: "mock-1", Spec: models.MockSpec{Metadata: map[string]string{"connID": "7"}}, Origin: &models.Origin{Clock: "proxy:run-1"}}
		r.StampOrigin(m)
		return m.Origin, models.NewWindowedMock(m).FileProcess()
	}
	first, firstLine := record("/tmp/go-build3136784009", 300, 301, "go", "test", "-v", "-ldflags=-X=main.key=s3cret", "-run", "TestOrders", "./...")
	second, secondLine := record("/var/tmp/go-build3633416167", 900, 912, "/usr/local/go/bin/go", "test", "-v", "-ldflags=-X=main.key=s3cret", "-run", "TestOrders", "./...")
	if want := "`store.test` (`go test ./...`)"; firstLine != want || secondLine != firstLine {
		t.Fatalf("the two records name the test binary %q and %q; want both %q", firstLine, secondLine, want)
	}
	if first == nil || second == nil || *first != *second || first.Exe != "go test ./..." {
		t.Fatalf("the two records' origins are %+v and %+v; want both go test ./...", first, second)
	}

	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"go", "test"}, "go test"},
		{[]string{"go", "test", "./..."}, "go test ./..."},
		{[]string{"go", "test", "./pkg/store", "./pkg/api", "-run", "TestOrders", "-count=1"}, "go test ./pkg/store ./pkg/api"},
		{[]string{"go", "test", "-race", "-tags", "dev", "-ldflags", "-X main.key=s3cret", "-count", "1", "-timeout", "5m", "./pkg/store"}, "go test ./pkg/store"},
		{[]string{"go", "test", "-test.run", "TestOrders", "--test.v", "-json", "-cover", "-coverprofile", "c.out", "."}, "go test ."},
		{[]string{"go", "test", "-exec", "sudo", "-vet=off", "-o", "bin/x.test", "-p", "4", "./x"}, "go test ./x"},
		{[]string{"go", "test", "store_test.go", "store.go", "-v"}, "go test store_test.go store.go"},
		{[]string{"go", "test", "./x", "-args", "-token", "s3cret"}, "go test ./x"},
		// Every word after -args is the test binary's: no package is named.
		{[]string{"go", "test", "-args", "./x", "s3cret"}, "go test"},
		// Its packages are redacted as argv[0] is.
		{[]string{"go", "test", "example.com/tool@v1.2.0"}, "go test " + containerkey.RedactedText},
		// A flag keploy does not know may take the next word as its
		// value: no word is named.
		{[]string{"go", "test", "-future", "s3cret", "./..."}, "go test"},
		{[]string{"go", "test", "-run"}, "go test"},
	} {
		got, ok := goTestOf(tc.argv)
		if !ok || got != tc.want {
			t.Errorf("goTestOf(%q) = %q, %v; want %q", tc.argv, got, ok, tc.want)
		}
		if strings.Contains(got, "s3cret") {
			t.Errorf("goTestOf(%q) names a flag's value: %q", tc.argv, got)
		}
	}
	for _, argv := range [][]string{nil, {"go"}, {"go", "run", "."}, {"go", "vet", "./..."}, {"gotest", "test", "."}} {
		if got, ok := goTestOf(argv); ok {
			t.Errorf("goTestOf(%q) = %q; want no go test", argv, got)
		}
	}
}

// Under `go test -exec <prog>` the test binary the go tool built is the exec
// program's child (sudo's, or the child sudo runs it under), not the go
// tool's: it is named by the go test all the same, the same in two records.
// No go command within goCommandDepth ancestors, one above another program
// the go tool built, or one above an ancestor keploy cannot read, names it:
// it is its own argv[0].
func TestAGoTestBinaryRunUnderAnExecProgramIsNamedByItsGoTest(t *testing.T) {
	record := func(dir string, base uint32, between ...string) string {
		c := newCommander()
		c.spawn(base, 1, at(0), "/usr/local/go/bin/go")
		c.comm[base], c.argv[base] = "go", []string{"go", "test", "-exec", "sudo -E", "-run", "TestProbe", "./pkg/ebpf"}
		parent := base
		for i, prog := range between {
			pid := base + uint32(i) + 1
			// "!" marks an ancestor whose command line keploy cannot read.
			hidden := strings.HasPrefix(prog, "!")
			prog = strings.TrimPrefix(prog, "!")
			c.spawn(pid, parent, at(1), prog)
			if !hidden {
				c.comm[pid], c.argv[pid] = filepath.Base(prog), []string{prog, "-E", dir + "/b001/ebpf.test"}
			}
			parent = pid
		}
		test := base + 50
		c.spawn(test, parent, at(5), dir+"/b001/ebpf.test")
		c.comm[test], c.argv[test] = "ebpf.test", []string{dir + "/b001/ebpf.test", "-test.paniconexit0", "-test.run=TestProbe"}
		r := New(c, 0)
		r.NoteOrigin("7", test)
		m := &models.Mock{Name: "mock-1", Spec: models.MockSpec{Metadata: map[string]string{"connID": "7"}}, Origin: &models.Origin{Clock: "proxy:run-1"}}
		r.StampOrigin(m)
		return models.NewWindowedMock(m).FileProcess()
	}
	const want = "`ebpf.test` (`go test ./pkg/ebpf`)"
	for _, between := range [][]string{
		{"/usr/bin/sudo"},
		// sudo running the command under a child of its own (use_pty).
		{"/usr/bin/sudo", "/usr/bin/sudo"},
	} {
		first := record("/tmp/go-build3136784009", 300, between...)
		second := record("/var/tmp/go-build3633416167", 900, between...)
		if first != want || second != first {
			t.Errorf("under %q the two records name the test binary %q and %q; want both %q", between, first, second, want)
		}
	}
	// No go command within goCommandDepth ancestors: its own argv[0].
	deep := []string{"/usr/bin/sudo", "/usr/bin/a", "/usr/bin/b", "/usr/bin/c"}
	if len(deep) < goCommandDepth {
		t.Fatalf("the deep chain %q is within goCommandDepth %d", deep, goCommandDepth)
	}
	if got := record("/tmp/go-build1", 300, deep...); got != "`ebpf.test` (`/tmp/go-build1/b001/ebpf.test`)" {
		t.Errorf("a test binary %d ancestors below its go test is named %q; want its own argv[0]", len(deep)+1, got)
	}
	if got := record("/tmp/go-build1", 300, deep[:goCommandDepth-1]...); got != want {
		t.Errorf("a test binary %d ancestors below its go test is named %q; want %q", goCommandDepth, got, want)
	}
	// Below an ancestor keploy cannot read (sudo under hidepid=2): the walk
	// stops there, and the test binary is its own argv[0], never named by a
	// go command keploy did not see run it.
	if got := record("/tmp/go-build1", 300, "!/usr/bin/sudo"); got != "`ebpf.test` (`/tmp/go-build1/b001/ebpf.test`)" {
		t.Errorf("a test binary below an ancestor keploy cannot read is named %q; want its own argv[0]", got)
	}
	// Above another program the go tool built (an exec program go run
	// built): that program's go command, not this one's.
	if got := record("/tmp/go-build1", 300, "/tmp/go-build7/b001/exe/wrap"); got != "`ebpf.test` (`/tmp/go-build1/b001/ebpf.test`)" {
		t.Errorf("a test binary run by another program the go tool built is named %q; want its own argv[0]", got)
	}
}

// Only the program the go tool built for a go run or a go test is named by
// that go command: the go tool's other children during the build (git
// fetching a GOPRIVATE module, the compiler, vet) and a go test -exec
// program are named by their own argv[0]. A go run's program the go build
// cache holds once built is named by its go run, as one in a work dir is.
func TestOnlyTheProgramTheGoToolBuiltIsNamedByItsGoCommand(t *testing.T) {
	c := newCommander()
	c.spawn(300, 1, at(0), "/usr/local/go/bin/go")
	c.comm[300], c.argv[300] = "go", []string{"go", "run", "."}
	c.spawn(400, 1, at(0), "/usr/local/go/bin/go")
	c.comm[400], c.argv[400] = "go", []string{"go", "test", "-exec", "/usr/local/bin/wrap", "./pkg/store"}
	gocache := "/home/u/.cache/go-build/9f/9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08-d/orders"
	// A dir named as a cache entry is, whose name is no hash.
	notHash := "/home/u/.cache/go-build/9f/" + strings.Repeat("g", 64) + "-d/orders"
	// A cache entry's look-alikes: its two-character dir no hex, and its
	// hash with no -d.
	notHexDir := "/home/u/.cache/go-build/zz/9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08-d/orders"
	notDash := "/home/u/.cache/go-build/9f/9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08-x/orders"
	for pid, child := range map[uint32]struct {
		parent uint32
		argv0  string
	}{
		301: {300, "git"},
		302: {300, "/usr/bin/git"},
		303: {300, "/usr/local/go/pkg/tool/linux_amd64/compile"},
		304: {300, "/usr/local/go/pkg/tool/linux_amd64/vet"},
		305: {300, gocache},
		306: {300, "/tmp/go-build1/b001/exe/orders"},
		401: {400, "/usr/local/bin/wrap"},
		402: {400, "/tmp/go-build22/b054/store.test"},
		// A path that only looks like the go tool's dirs.
		403: {400, "/tmp/go-builds/b001/store.test"},
		404: {400, "/tmp/go-build22/bin/store.test"},
		405: {300, notHash},
		406: {300, notHexDir},
		407: {300, notDash},
	} {
		c.spawn(pid, child.parent, at(5), child.argv0)
		c.comm[pid], c.argv[pid] = filepath.Base(child.argv0), []string{child.argv0}
	}
	r := New(c, 0)
	for pid, want := range map[uint32]string{
		301: "git", 302: "/usr/bin/git",
		303: "/usr/local/go/pkg/tool/linux_amd64/compile", 304: "/usr/local/go/pkg/tool/linux_amd64/vet",
		305: "go run .", 306: "go run .",
		401: "/usr/local/bin/wrap", 402: "go test ./pkg/store",
		403: "/tmp/go-builds/b001/store.test", 404: "/tmp/go-build22/bin/store.test",
		405: notHash, 406: notHexDir, 407: notDash,
	} {
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
	if line := lines.All()[0].ContextMap()["cmdline"].(string); line != "first --token <redacted>" {
		t.Fatalf("the logged command line is %q; want its program and its flag, its token redacted", line)
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

// Each mock carries the keploy instance (a test worker's) its own
// connection was stamped with, the connection's process aside: a mock that
// names one already keeps it, a connection noted again with another
// instance is the later connection's, and Stamp gives it under the proven
// switch only, by the connection's metadata ID too.
func TestAMockCarriesItsConnectionsInstance(t *testing.T) {
	c := newCommander()
	c.comm[42], c.comm[43] = "w1", "w2"
	r := New(c, 0)
	for _, v := range []uint32{3, 4, 5} {
		r.InstanceBegan(v)
	}
	r.NoteOriginIn("1", 42, 3)
	r.NoteOriginIn("2", 43, 5)
	r.NoteOrigin("3", 42)
	for conn, want := range map[string]uint32{"1": 3, "2": 5, "3": 0, "unknown": 0} {
		m := &models.Mock{ConnectionID: conn}
		r.StampInstance(m)
		if m.Instance != want {
			t.Fatalf("connection %s's mock got instance %d; want %d", conn, m.Instance, want)
		}
	}
	kept := &models.Mock{ConnectionID: "2", Instance: 7}
	r.StampInstance(kept)
	if kept.Instance != 7 {
		t.Fatalf("a mock that names its instance got %d", kept.Instance)
	}
	r.NoteOriginIn("1", 42, 4)
	again := &models.Mock{ConnectionID: "1"}
	r.StampInstance(again)
	if again.Instance != 4 {
		t.Fatalf("a connection ID noted again with another instance stamps %d; want the later 4", again.Instance)
	}
	t.Setenv(provenswitch.Env, "")
	off := &models.Mock{ConnectionID: "2"}
	r.Stamp(off)
	if off.Instance != 0 {
		t.Fatalf("without the switch Stamp gave the mock instance %d", off.Instance)
	}
	t.Setenv(provenswitch.Env, "1")
	on := &models.Mock{Spec: models.MockSpec{Metadata: map[string]string{"connID": "2"}}}
	r.Stamp(on)
	if on.Instance != 5 {
		t.Fatalf("under the switch Stamp gave the mock instance %d; want 5", on.Instance)
	}
	if c := on.DeepCopy(); c.Instance != 5 {
		t.Fatalf("a copy of the mock carries instance %d; want 5", c.Instance)
	}
}

// A connection stamped with a test worker's keploy instance whose worker
// has ended by the time its mock is stamped is from no live worker: the
// mock gets no instance, so no window of a later claim of that id holds it,
// and carries the ended one, which record names as it charges the call. A
// stamp never made live is no worker's either. While the worker lives its
// mocks carry its instance, and a mock stamped before the end keeps it.
func TestAnEndedWorkersConnectionStampsNoInstance(t *testing.T) {
	c := newCommander()
	c.comm[42] = "app"
	r := New(c, 0)
	r.InstanceBegan(1025)
	r.NoteOriginIn("1", 42, 1025)
	r.NoteOriginIn("2", 42, 9)
	before := &models.Mock{ConnectionID: "1"}
	r.StampInstance(before)
	if before.Instance != 1025 || before.EndedInstance != 0 {
		t.Fatalf("a live worker's mock got instance %d, ended %d; want 1025, 0", before.Instance, before.EndedInstance)
	}
	r.InstanceEnded(1025)
	r.StampInstance(before)
	if before.Instance != 1025 || before.EndedInstance != 0 {
		t.Fatalf("a mock stamped while its worker lived got instance %d, ended %d at a later stamp; want 1025, 0", before.Instance, before.EndedInstance)
	}
	r.Reset()
	r.NoteOriginIn("1", 42, 1025)
	for conn, want := range map[string]uint32{"1": 1025, "2": 0} {
		if conn == "2" {
			r.NoteOriginIn("2", 42, 9)
			want = 9
		}
		after := &models.Mock{ConnectionID: conn}
		r.StampInstance(after)
		if after.Instance != 0 || after.EndedInstance != want {
			t.Fatalf("connection %s's mock got instance %d, ended %d; want 0, %d", conn, after.Instance, after.EndedInstance, want)
		}
		r.StampInstance(after)
		if after.Instance != 0 || after.EndedInstance != want {
			t.Fatalf("connection %s's mock stamped again got instance %d, ended %d; want 0, %d", conn, after.Instance, after.EndedInstance, want)
		}
		if cp := after.DeepCopy(); cp.EndedInstance != want {
			t.Fatalf("a copy of connection %s's mock carries ended %d; want %d", conn, cp.EndedInstance, want)
		}
	}
	r.InstanceBegan(2049)
	r.Reset()
	r.NoteOriginIn("3", 42, 2049)
	live := &models.Mock{ConnectionID: "3"}
	r.StampInstance(live)
	if live.Instance != 2049 || live.EndedInstance != 0 {
		t.Fatalf("a worker live across a Reset: its mock got instance %d, ended %d; want 2049, 0", live.Instance, live.EndedInstance)
	}
	if r.InstanceGone(0) {
		t.Fatal("a connection stamped with no instance was read as an ended worker's")
	}
	stamped := &models.Mock{ConnectionID: "1", EndedInstance: 1025}
	r.NoteOriginIn("1", 42, 2049)
	r.StampInstance(stamped)
	if stamped.Instance != 0 || stamped.EndedInstance != 1025 {
		t.Fatalf("a mock stamped as an ended worker's got instance %d, ended %d once its connection id was noted in a live worker; want 0, 1025 (the first stamp is final)", stamped.Instance, stamped.EndedInstance)
	}
}

// Whether a call is an ended worker's is decided by when keploy read its
// request, against its worker's end, never by when its parser hands it
// over: a request read while the worker lived is the worker's, though
// handed over after its end; one read at or after the end, or with no read
// time, is from no live worker. Another worker's connection stays its own,
// a second end does not move the first, and a connection noted after the
// end is from no live worker whatever its calls' times.
func TestACallIsAnEndedWorkersByWhenItsRequestWasRead(t *testing.T) {
	c := newCommander()
	c.comm[42], c.comm[43] = "a", "b"
	r := New(c, 0)
	r.InstanceBegan(1)
	r.InstanceBegan(2)
	r.NoteOriginIn("a", 42, 1)
	r.NoteOriginIn("b", 43, 2)
	live := time.Now()
	time.Sleep(time.Millisecond)
	r.InstanceEnded(1)
	end := r.origin.conns["a"].endedAt
	if end.IsZero() || !r.origin.conns["b"].endedAt.IsZero() {
		t.Fatalf("worker 1's end marked its connection at %v and worker 2's at %v; want only worker 1's", end, r.origin.conns["b"].endedAt)
	}
	time.Sleep(time.Millisecond)
	between := time.Now()
	time.Sleep(time.Millisecond)
	r.InstanceEnded(1)
	r.NoteOriginIn("a-late", 42, 1)
	for _, tc := range []struct {
		what, conn          string
		read                time.Time
		instance, wantEnded uint32
	}{
		{"read while its worker lived, handed over after its end", "a", live, 1, 0},
		{"read at the instant its worker ended", "a", end, 0, 1},
		{"read after its worker ended, before a second end", "a", between, 0, 1},
		{"read after its worker ended", "a", time.Now(), 0, 1},
		{"with no read time", "a", time.Time{}, 0, 1},
		{"of another, live, worker, read after the first's end", "b", time.Now(), 2, 0},
		{"on a connection noted after its worker ended", "a-late", live, 0, 1},
	} {
		m := &models.Mock{ConnectionID: tc.conn}
		m.Spec.ReqTimestampMock = tc.read
		r.StampInstance(m)
		if m.Instance != tc.instance || m.EndedInstance != tc.wantEnded {
			t.Fatalf("a call %s got instance %d, ended %d; want %d, %d", tc.what, m.Instance, m.EndedInstance, tc.instance, tc.wantEnded)
		}
	}
}

// loggedCommandLine is the command line the registry r logs, once, for
// process pid with argv, its first name read (ProcessOf).
func loggedCommandLine(t *testing.T, r *Registry, c *commander, pid uint32, argv []string) string {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	r.SetLogger(zap.New(core))
	c.born[pid] = at(0)
	c.comm[pid], c.argv[pid] = filepath.Base(argv[0]), argv
	if _, _, ok := r.ProcessOf(pid); !ok {
		t.Fatalf("process %d was not read", pid)
	}
	lines := logs.FilterMessage("a recorded call came from this process").All()
	if len(lines) != 1 {
		t.Fatalf("the command line was logged %d times; want once", len(lines))
	}
	return lines[0].ContextMap()["cmdline"].(string)
}

// The record log names the process a call came from by its command line,
// written by its shape (a process's words are the application's traffic,
// the hosts, paths and users it calls with, and keploy's log reaches log
// stores): each word but its program, its flags' names and its short
// numbers by its shape and, after it, its tag, never whole: a URL in an
// argument, a host:port, a flag's URL, a host beside a secret, which is
// redacted. With no session set, the tag is under the process's key. The
// line is written from the process's words, not a line joined from them,
// so a quote or a space in the program's path, an empty argv[0], a
// process's own title and an argument with a space in it each stay one
// word, and none moves a host into the program's kept name.
func TestTheLoggedCommandLineNamesEachHostByItsShapeAndTag(t *testing.T) {
	var keys *models.LogKeySession // the process's key
	tag := keys.LogKey
	const url = "http://dep.keploy.test:18999"
	for i, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"a URL in an argument", []string{"python3", "client.py", "http://dep.keploy.test:18999"},
			"python3 '{}.{} " + tag("client.py") + "' '{}://{}.{}.{}:{} " + tag("http://dep.keploy.test:18999") + "'"},
		{"a bare host:port", []string{"./client", "dep.keploy.test:18999"},
			"client '{}.{}.{}:{} " + tag("dep.keploy.test:18999") + "'"},
		{"a flag=URL", []string{"./client", "--target=http://dep.keploy.test:18999/v1/orders"},
			"client '--target={}://{}.{}.{}:{}/{}/{} " + tag("http://dep.keploy.test:18999/{}/{}") + "'"},
		{"a secret beside a host", []string{"./worker", "--db-host", "db.keploy.test", "--db-password", "hunter2"},
			"worker --db-host '{}.{}.{} " + tag("db.keploy.test") + "' --db-password <redacted>"},
		{"an apostrophe in the program's path, a host flag last", []string{"/Users/dev/Bob's Projects/api/.venv/bin/python", "app.py", "--host", "db.keploy.test"},
			`''\''/{}/{}/{} {}/{}/.{}/{}/{}'\'' ` + tag("'/Users/dev/Bobs Projects/api/.venv/bin/python'") + "' '{}.{} " + tag("app.py") + "' --host '{}.{}.{} " + tag("db.keploy.test") + "'"},
		{"an apostrophe in the program's path, a URL last", []string{"/home/dev/Bob's app/bin/python3", "client.py", url},
			`''\''/{}/{}/{} {}/{}/{}'\'' ` + tag("'/home/dev/Bobs app/bin/python3'") + "' '{}.{} " + tag("client.py") + "' '{}://{}.{}.{}:{} " + tag(url) + "'"},
		{"a double quote in the program's path", []string{`/opt/a"b/run`, "--host", "db.keploy.test"},
			`'/{}/{}"{}/{} ` + tag(`/opt/a"b/run`) + "' --host '{}.{}.{} " + tag("db.keploy.test") + "'"},
		{"an empty argv[0]", []string{"", "dep.keploy.test:18999"},
			"'' '{}.{}.{}:{} " + tag("dep.keploy.test:18999") + "'"},
		{"sshd's title, with spaces", []string{"sshd: app@db.keploy.test [priv]"},
			"'{}: {}@{}.{}.{} [{}] " + tag("sshd: app@db.keploy.test [priv]") + "'"},
		{"ssh's [mux] title, with spaces", []string{"ssh: /home/u/.ssh/cm-app@db.keploy.test:22 [mux]"},
			"<redacted>"},
		{"an argument with a space", []string{"python3", "client.py", "my file.txt"},
			"python3 '{}.{} " + tag("client.py") + "' '{} {}.{} " + tag("my file.txt") + "'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCommander()
			got := loggedCommandLine(t, New(c, 0), c, uint32(40+i), tc.argv)
			if got != tc.want {
				t.Errorf("the logged command line is %q; want %q", got, tc.want)
			}
			if strings.Contains(got, "keploy.test") || strings.Contains(got, "hunter2") {
				t.Errorf("the logged command line %q names a host or a secret whole", got)
			}
		})
	}
}

// The registry logs a process's command line in the session it is told of
// (SetLogKeySession): its words' tags are the session's, not the process's
// key's; a later session's differ.
func TestTheLoggedCommandLineTagsItsHostsUnderTheSession(t *testing.T) {
	c := newCommander()
	r := New(c, 0)
	keys := models.NewLogKeySession()
	r.SetLogKeySession(keys)
	if r.LogKeySession() != keys {
		t.Fatal("the registry does not hold the session it was told of")
	}
	const dep = "http://dep.keploy.test:18999"
	argv := []string{"python3", "client.py", dep}
	got := loggedCommandLine(t, r, c, 42, argv)
	if want := "python3 '{}.{} " + keys.LogKey("client.py") + "' '{}://{}.{}.{}:{} " + keys.LogKey(dep) + "'"; got != want {
		t.Fatalf("the logged command line is %q; want %q", got, want)
	}
	var process *models.LogKeySession
	if strings.Contains(got, process.LogKey(dep)) {
		t.Fatalf("the logged command line %q tags its words under the process's key", got)
	}
	r.Reset()
	next := models.NewLogKeySession()
	r.SetLogKeySession(next)
	if again := loggedCommandLine(t, r, c, 42, argv); !strings.Contains(again, next.LogKey(dep)) || again == got {
		t.Fatalf("the next session logged %q; want its own tag (the last one logged %q)", again, got)
	}
}

// A call's source carries its process's whole command line, its secrets
// redacted, to keploy mock's receipt (models.Origin.Cmdline), written from
// its words: a word with white space or a quote, or an empty one, is
// quoted, so each reads as the word it is.
func TestASourceCarriesItsCommandLineWordByWord(t *testing.T) {
	for i, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"./sqs", "--queue", "orders", "--password", "hunter2"}, "./sqs --queue orders --password <redacted>"},
		{[]string{"/home/dev/Bob's app/bin/python3", "client.py", "http://dep.keploy.test:18999"},
			`'/home/dev/Bob'\''s app/bin/python3' client.py http://dep.keploy.test:18999`},
		{[]string{"", "dep.keploy.test:18999"}, "'' dep.keploy.test:18999"},
		{[]string{"python3", "client.py", "my file.txt"}, "python3 client.py 'my file.txt'"},
	} {
		c := newCommander()
		r := New(c, 0)
		pid := uint32(60 + i)
		c.born[pid] = at(0)
		c.comm[pid], c.argv[pid] = "x", tc.argv
		if o := r.SourceOf(pid); o == nil || o.Cmdline != tc.want {
			t.Errorf("argv %q: the source is %+v; want its command line %q", tc.argv, o, tc.want)
		}
	}
}

// A process whose birth cannot be read is logged again for a pid only when
// its words differ: words that would join to one line ("a b" and "a", "b")
// are not one command line.
func TestAPidWithNoBirthIsLoggedAgainForOtherWords(t *testing.T) {
	c := newCommander()
	r := New(c, 0)
	core, logs := observer.New(zapcore.DebugLevel)
	r.SetLogger(zap.New(core))
	c.comm[12], c.argv[12] = "app", []string{"./app", "a b"}
	r.ProcessOf(12)
	r.ProcessOf(12)
	c.argv[12] = []string{"./app", "a", "b"} // pid 12 reused; its birth still unknown
	r.ProcessOf(12)
	if n := logs.FilterMessage("a recorded call came from this process").Len(); n != 2 {
		t.Fatalf("the command lines of pid 12 were logged %d times; want 2, one for each process's words", n)
	}
}

// A process's command line is written by its shape only for a logger that
// writes the line: with debug off, logging it does nothing, not even the
// shaping (which costs about 90 µs for a long command line, once per new
// process).
func TestAProcessLineIsShapedOnlyWhenLogged(t *testing.T) {
	r := New(newCommander(), 0)
	info, quiet := observer.New(zapcore.InfoLevel)
	r.SetLogger(zap.New(info))
	n := procName{comm: "python3", argv: []string{"python3", "client.py", "http://dep.keploy.test:18999"}, ok: true}
	if allocs := testing.AllocsPerRun(100, func() { r.origin.logProcess(7, n) }); allocs != 0 || quiet.Len() != 0 {
		t.Fatalf("with debug off, logging a process's line allocated %v times and wrote %d lines; want nothing done", allocs, quiet.Len())
	}
	debug, logs := observer.New(zapcore.DebugLevel)
	r.SetLogger(zap.New(debug))
	r.origin.logProcess(7, n)
	if logs.FilterMessage("a recorded call came from this process").Len() != 1 {
		t.Fatal("with debug on, the process's line was not written")
	}
}
