//go:build linux

package agent

import (
	"bufio"
	"context"
	"encoding/gob"
	"fmt"
	"io"
	"net"
	nethttp "net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	httpparser "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// These rows run keploy mock replay's staging against the agent's on-disk
// store and the proxy's real mock manager: what the agent decodes and holds,
// and what each test is answered with.

// diskServingProxy is a servingProxy that reads, as the agent's proxy does,
// the recordings its trees do not hold where the agent points it (the
// on-disk store), and takes the set's startup cutoff and recorded windows.
type diskServingProxy struct {
	*servingProxy
}

func (p *diskServingProxy) SetRecordingSource(src func(kind models.Kind, keys []string, visit func(name string, load func() (*models.Mock, error)) bool) error) error {
	p.mm.SetRecordingSource(src)
	return nil
}

func (p *diskServingProxy) SetRecordedBefore(src func(t time.Time, visit func(*models.Mock) bool) error) error {
	p.mm.SetRecordedBefore(src)
	return nil
}

func (p *diskServingProxy) SetRecordedOfKind(src func(kind models.Kind, visit func(name string, load func() (*models.Mock, error)) bool) error) error {
	p.mm.SetRecordedOfKind(src)
	return nil
}

// SetPoolNames installs the pool's names and the key of each test's
// recordings on the manager, as the agent's proxy does (Proxy.SetPoolNames):
// they bound the set's teardown (MockManager.teardownAfter).
func (p *diskServingProxy) SetPoolNames(names []string, owners map[string]string) error {
	p.mm.SetPoolOwners(owners)
	p.mm.SetPoolNames(names)
	return nil
}

func (p *diskServingProxy) SetRecordingTimes(src func(visit func(name string, req, res time.Time, protocolPhase bool) bool) error) error {
	p.mm.SetRecordingTimes(src)
	return nil
}

func (p *diskServingProxy) SeedStartupCutoff(start time.Time)          { p.mm.SeedStartupCutoff(start) }
func (p *diskServingProxy) SeedRecordedWindows(ws []models.TestWindow) { p.mm.SeedRecordedWindows(ws) }

// callSettled is call, returning once the HTTP parser that answered it has
// returned too: a parser that saw the app hang up retries its empty read
// for up to 150 ms (util.ReadBytes), holding the call's view until then.
func (p *diskServingProxy) callSettled(t *testing.T, path string) (int, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	acc := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); acc <- c }()
	app, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	prox := <-acc
	ctx := models.WithMockMismatchReporter(context.Background(), p.noteMiss)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = httpparser.New(zap.NewNop()).MockOutgoing(ctx, prox, &models.ConditionalDstCfg{Port: 18093}, p.mm, models.OutgoingOptions{})
	}()
	_, err = io.WriteString(app, "GET "+path+" HTTP/1.1\r\nHost: 127.0.0.1:18093\r\n\r\n")
	require.NoError(t, err)
	resp, err := nethttp.ReadResponse(bufio.NewReader(app), nil)
	require.NoError(t, err)
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	_ = app.Close()
	<-done
	_ = prox.Close()
	return resp.StatusCode, string(b)
}

// diskReplayAgent is keploy mock replay's agent on a real mock manager,
// whose per-test mocks the store keeps on disk (strict mock window, the
// default).
func diskReplayAgent(t *testing.T) (*Agent, *diskServingProxy) {
	t.Helper()
	a, _ := wireAgent(t, models.MODE_TEST)
	a.config.Agent.MockMode = true
	a.config.Test.StrictMockWindow = true
	p := &diskServingProxy{servingProxy: newServingProxy()}
	a.Proxy = p
	return a, p
}

// storeStream stores n mocks on a, made one at a time by mock as the CLI
// streams them (StoreMocksStream), so no side holds the whole recording, and
// returns the on-disk store they went to.
func storeStream(t *testing.T, a *Agent, n int, mock func(i int) *models.Mock) *proxyPkg.DiskMocks {
	t.Helper()
	pr, pw := io.Pipe()
	go func() {
		enc := gob.NewEncoder(pw)
		if err := enc.Encode(models.MockStreamHeader{FilteredCount: n}); err != nil {
			pw.CloseWithError(err)
			return
		}
		for i := 0; i < n; i++ {
			if err := enc.Encode(mock(i)); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		_ = pw.Close()
	}()
	dec := gob.NewDecoder(pr)
	var header models.MockStreamHeader
	require.NoError(t, dec.Decode(&header))
	require.NoError(t, a.StoreMocksStream(context.Background(), header, dec))
	_ = pr.Close()
	disk := loadStorage(t, a).diskMocks
	require.NotNil(t, disk, "the store kept no per-test mock on disk")
	require.Equal(t, n, disk.Len(), "the store kept some per-test mocks resident")
	return disk
}

// storeOnDisk is storeStream of mocks.
func storeOnDisk(t *testing.T, a *Agent, mocks ...*models.Mock) *proxyPkg.DiskMocks {
	t.Helper()
	return storeStream(t, a, len(mocks), func(i int) *models.Mock { return mocks[i] })
}

// ownMock is pingMock as a recorder that states each mock's lifetime records
// it: a per-test recording, which the store keeps on disk.
func ownMock(name, path, body string, at time.Time) *models.Mock {
	m := pingMock(name, path, body, at)
	m.Spec.Metadata = map[string]string{models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
	return m
}

// decodes notes the name of every mock disk decodes from now on; take
// returns them sorted, and starts the note afresh.
func decodes(disk *proxyPkg.DiskMocks) (take func() []string) {
	var mu sync.Mutex
	var names []string
	disk.SetOnLoad(func(m *models.Mock) {
		mu.Lock()
		names = append(names, m.Name)
		mu.Unlock()
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := names
		names = nil
		sort.Strings(out)
		return out
	}
}

// heapNow is the heap's live bytes now: what is left after a collection.
func heapNow() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// replaySet stores a set of two tests on disk: a call made as the app
// started (boot), test A's two, one between the tests (gap), test B's, and
// one after the last test (late). Every recording no test owns is the pool's,
// as keploy mock replay's table says.
func replaySet(t *testing.T, a *Agent) *proxyPkg.DiskMocks {
	t.Helper()
	disk := storeOnDisk(t, a,
		ownMock("boot", "/boot", "boot", t0.Add(-time.Second)),
		ownMock("mock-a", "/a", "a", t0.Add(time.Second)),
		ownMock("mock-a2", "/a2", "a2", t0.Add(1500*time.Millisecond)),
		ownMock("gap", "/gap", "gap", t0.Add(2500*time.Millisecond)),
		ownMock("mock-b", "/b", "b", t0.Add(3*time.Second)),
		ownMock("late", "/late", "late", t0.Add(5*time.Second)))
	require.NoError(t, a.SetScopeTable(context.Background(), map[string][]string{
		"o::TestA": {"mock-a", "mock-a2"},
		"o::TestB": {"mock-b"},
	}))
	return disk
}

// replayWindows are the recorded windows of replaySet's tests.
var replayWindows = []models.TestWindow{
	{TestCase: "o::TestA", Start: t0.Add(900 * time.Millisecond), End: t0.Add(2 * time.Second)},
	{TestCase: "o::TestB", Start: t0.Add(2900 * time.Millisecond), End: t0.Add(4 * time.Second)},
}

// keploy mock replay's staging decodes, of the on-disk store, what was
// recorded before the set's first test and the pool, never a test's own:
// each test decodes its own as it opens. It decoded the whole store and held
// it for the whole session (api-server's full round: 107-116 MB), beside the
// second copy each test's open decoded. Every call is answered as before: the
// pool between tests, each test its own, and a call that is another test's
// recording misses naming it, read from the on-disk store.
func TestMockReplayStagesOnlyWhatNoTestOwns(t *testing.T) {
	a, p := diskReplayAgent(t)
	disk := replaySet(t, a)
	take := decodes(disk)
	ctx := context.Background()
	require.NoError(t, a.SetRecordedWindows(ctx, replayWindows))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": {"boot", "gap", "late"}}))
	claimSelf(t, a)
	require.Equal(t, []string{"boot", "gap", "late"}, take(), "the staging decoded a test's own recordings")

	code, body := p.call(t, "/boot")
	require.Equal(t, 200, code)
	require.Equal(t, "boot", body)
	got, e := runTest(t, a, p.servingProxy, "TestA", "/a", "/a2", "/b")
	require.Equal(t, []string{"200 a", "200 a2"}, got[:2])
	require.True(t, strings.HasPrefix(got[2], "502"), "test B's recording answered test A: %s", got[2])
	require.Equal(t, models.ScopeActionFail, e.Action)
	ev := evidenceOf(t, a, "o::TestA")
	require.Len(t, ev.Misses, 1)
	require.Equal(t, "mock-b", ev.Misses[0].ClosestMock, "the miss on test B's recording does not name it")
	// Test A's open decoded its own; the miss's diagnosis decoded test B's,
	// which it reads by the call's keys and keeps nowhere.
	require.Equal(t, []string{"mock-a", "mock-a2", "mock-b"}, take())
	code, body = p.call(t, "/gap")
	require.Equal(t, 200, code)
	require.Equal(t, "gap", body)
	got, e = runTest(t, a, p.servingProxy, "TestB", "/b")
	require.Equal(t, []string{"200 b"}, got)
	require.Empty(t, e.Action, e.Reason)
	require.Equal(t, []string{"mock-b"}, take())
	code, body = p.call(t, "/late")
	require.Equal(t, 200, code)
	require.Equal(t, "late", body)
}

// keploy mock replay's staging asks for the strict window's staging load (the
// pool and what came before the set's first test), but it is no user's strict
// window: it prints nothing of the strict window or of its opt-out
// (KEPLOY_STRICT_MOCK_WINDOW=0), whose advice is keploy test's.
func TestMockReplayStagingPrintsNoStrictWindowNotice(t *testing.T) {
	require.True(t, pkg.IsStrictMockWindow(true), "precondition: the strict window is on in this process")
	a, _ := diskReplayAgent(t)
	core, logs := observer.New(zap.InfoLevel)
	a.logger = zap.New(core)
	take := decodes(replaySet(t, a))
	ctx := context.Background()
	require.NoError(t, a.SetRecordedWindows(ctx, replayWindows))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": {"boot", "gap", "late"}}))
	claimSelf(t, a)
	require.Equal(t, []string{"boot", "gap", "late"}, take(), "the staging decoded a test's own recordings")
	require.Zero(t, logs.FilterMessageSnippet("strict mock-window").Len(), "mock replay's staging printed the strict window's notice")
}

// The strict window's opt-outs (test.strictMockWindow: false, and
// KEPLOY_STRICT_MOCK_WINDOW=0, which wins over every flag) choose how keploy
// test matches. They do not choose how much keploy mock replay's agent holds:
// its store keeps the per-test mocks on disk, the staging decodes only what no
// test owns, and each test decodes its own as it opens. With either opt-out
// the agent kept no on-disk store and held the whole recording; with the env
// one, the staging would have decoded the whole store too. The env is read as
// the process starts, so that half runs in a process of its own.
func TestMockReplayHoldsAsMuchWithTheStrictWindowOff(t *testing.T) {
	const child = "KEPLOY_TEST_STRICT_OFF_CHILD"
	if os.Getenv(child) == "1" {
		require.False(t, pkg.IsStrictMockWindow(true), "precondition: KEPLOY_STRICT_MOCK_WINDOW=0 turns the strict window off in this process")
		replaysOneTestsRecordingsAtATime(t, true)
		return
	}
	t.Run("config", func(t *testing.T) {
		replaysOneTestsRecordingsAtATime(t, false)
	})
	// Only keploy mock replay's agent under the switch keeps the store with
	// the strict window off: keploy test's lax filter reads every per-test
	// mock for each test, and with the switch off the agent is main's.
	t.Run("only mock replay's agent", func(t *testing.T) {
		for _, c := range []struct {
			name           string
			proven, mockMd bool
			mode           models.Mode
			wantDisk       bool
		}{
			{"mock replay", true, true, models.MODE_TEST, true},
			{"keploy test", true, false, models.MODE_TEST, false},
			{"mock record", true, true, models.MODE_RECORD, false},
			{"switch off", false, true, models.MODE_TEST, false},
		} {
			t.Run(c.name, func(t *testing.T) {
				if c.proven {
					t.Setenv(provenswitch.Env, "1")
				} else {
					t.Setenv(provenswitch.Env, "0")
				}
				a := strictAgent(false)
				a.config.Agent.MockMode = c.mockMd
				a.config.Agent.Mode = c.mode
				header, dec := readHeaderAndDecoder(t, encodeStreamBody(t, []*models.Mock{diskEligibleMock("pt-1", t0)}, nil))
				require.NoError(t, a.StoreMocksStream(context.Background(), header, dec))
				disk := loadStorage(t, a).diskMocks
				if disk != nil {
					t.Cleanup(func() { _ = disk.Close() })
				}
				// An enabling KEPLOY_STRICT_MOCK_WINDOW keeps it for every agent.
				require.Equal(t, c.wantDisk || pkg.IsStrictMockWindow(false), disk != nil, "on-disk store kept")
			})
		}
	})
	t.Run("env", func(t *testing.T) {
		// The child is bounded: its own -test.timeout, the row's context, and
		// its own process group, killed whole when the row ends. The kernel
		// kills it too when its parent dies by a signal (go test's own
		// timeout kill), so a parent that dies early leaves nothing running.
		// The parent-death signal follows the thread that starts the child,
		// so this goroutine keeps that thread until the child is reaped.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestMockReplayHoldsAsMuchWithTheStrictWindowOff$", "-test.count=1", "-test.v", "-test.timeout=5m")
		cmd.Env = append(os.Environ(), child+"=1", "KEPLOY_STRICT_MOCK_WINDOW=0")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		t.Cleanup(func() {
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		})
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		// A child that ran no row would exit 0 as well.
		require.Contains(t, string(out), "--- PASS: TestMockReplayHoldsAsMuchWithTheStrictWindowOff", "%s", out)
	})
}

// replaysOneTestsRecordingsAtATime replays replaySet on keploy mock replay's
// agent with the strict window turned off by the user (byEnv: by the
// process's env, which is already so; else by config): the store keeps every
// per-test mock on disk (storeStream asserts it), the staging decodes what no
// test owns, each test its own, and every call is answered as recorded.
func replaysOneTestsRecordingsAtATime(t *testing.T, byEnv bool) {
	t.Helper()
	a, p := diskReplayAgent(t)
	if !byEnv {
		a.config.Test.StrictMockWindow = false
	}
	disk := replaySet(t, a)
	take := decodes(disk)
	ctx := context.Background()
	require.NoError(t, a.SetRecordedWindows(ctx, replayWindows))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": {"boot", "gap", "late"}}))
	claimSelf(t, a)
	require.Equal(t, []string{"boot", "gap", "late"}, take(), "the staging decoded a test's own recordings")
	code, body := p.call(t, "/boot")
	require.Equal(t, 200, code)
	require.Equal(t, "boot", body)
	got, e := runTest(t, a, p.servingProxy, "TestA", "/a", "/a2")
	require.Equal(t, []string{"200 a", "200 a2"}, got)
	require.Empty(t, e.Action, e.Reason)
	require.Equal(t, []string{"mock-a", "mock-a2"}, take(), "test A's open decoded more than its own")
	got, e = runTest(t, a, p.servingProxy, "TestB", "/b")
	require.Equal(t, []string{"200 b"}, got)
	require.Empty(t, e.Action, e.Reason)
	require.Equal(t, []string{"mock-b"}, take(), "test B's open decoded more than its own")
	code, body = p.call(t, "/late")
	require.Equal(t, 200, code)
	require.Equal(t, "late", body)
}

// A pool slice the CLI sends after the set is staged, naming a recording the
// staging did not read, stages the set again before it is served: a name not
// staged would answer nothing.
func TestAPoolSliceNamingAnUnstagedRecordingStagesIt(t *testing.T) {
	a, p := diskReplayAgent(t)
	disk := replaySet(t, a)
	take := decodes(disk)
	ctx := context.Background()
	require.NoError(t, a.SetRecordedWindows(ctx, replayWindows))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": {"boot"}}))
	require.Equal(t, []string{"boot"}, take())
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": {"boot", "gap"}}))
	claimSelf(t, a)
	code, body := p.call(t, "/gap")
	require.Equal(t, 200, code, body)
	require.Equal(t, "gap", body)
	// The same slice again stages nothing more.
	take()
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": {"boot", "gap"}}))
	require.Empty(t, take(), "a slice the staging already read staged the set again")
}

// A table with no recorded window (a CLI that sends none) gives the staging
// no startup cutoff: it reads the whole window, as before, and every test is
// still answered its own.
func TestAReplayWithNoRecordedWindowStagesTheWholeWindow(t *testing.T) {
	a, p := diskReplayAgent(t)
	disk := replaySet(t, a)
	take := decodes(disk)
	ctx := context.Background()
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": {"boot", "gap", "late"}}))
	claimSelf(t, a)
	require.Equal(t, []string{"boot", "gap", "late", "mock-a", "mock-a2", "mock-b"}, take())
	got, e := runTest(t, a, p.servingProxy, "TestA", "/a", "/a2")
	require.Equal(t, []string{"200 a", "200 a2"}, got)
	require.Empty(t, e.Action, e.Reason)
	code, body := p.call(t, "/gap")
	require.Equal(t, 200, code)
	require.Equal(t, "gap", body)
}

// keploy mock replay holds one test's recordings at a time, whatever the
// recording's size. Ten tests own 128 MiB of recordings each (1.25 GiB on
// disk; the bodies are requests, which the store decodes with each mock).
// After the staging the live heap holds the pool, under half a test's
// recordings over where it began; at each test's open, under 1.5 tests';
// after each test's close, under half a test's again. Each call is measured
// once its parser has returned (callSettled): a call in flight holds its
// test's view. The staging held all ten tests' (the whole store) for the
// whole session: run it on a box.
func TestMockReplayHoldsOneTestsRecordingsAtATime(t *testing.T) {
	const tests, pieces, piece = 10, 16, 8 << 20
	const testSize = pieces * piece
	a, p := diskReplayAgent(t)
	big := strings.Repeat("x", piece)
	start := func(k int) time.Time { return t0.Add(time.Duration(k) * 10 * time.Second) }
	// Per test k: its own call, its pieces, then a call between it and the
	// next test (the pool's). The app's start before them.
	per := 1 + pieces + 1
	mock := func(i int) *models.Mock {
		if i == 0 {
			return ownMock("boot", "/boot", "boot", t0.Add(-time.Second))
		}
		k, j := (i-1)/per, (i-1)%per
		at := start(k)
		switch {
		case j == 0:
			return ownMock(fmt.Sprintf("own-%d", k), fmt.Sprintf("/own/%d", k), fmt.Sprintf("own %d", k), at.Add(time.Second))
		case j <= pieces:
			m := ownMock(fmt.Sprintf("piece-%d-%d", k, j), fmt.Sprintf("/piece/%d/%d", k, j), "ok", at.Add(2*time.Second+time.Duration(j)*time.Millisecond))
			m.Spec.HTTPReq.Method = "POST"
			m.Spec.HTTPReq.Body = big
			return m
		default:
			return ownMock(fmt.Sprintf("gap-%d", k), fmt.Sprintf("/gap/%d", k), fmt.Sprintf("gap %d", k), at.Add(8*time.Second))
		}
	}
	table := map[string][]string{}
	var windows []models.TestWindow
	pool := []string{"boot"}
	for k := 0; k < tests; k++ {
		id := fmt.Sprintf("o::Test%d", k)
		names := []string{fmt.Sprintf("own-%d", k)}
		for j := 1; j <= pieces; j++ {
			names = append(names, fmt.Sprintf("piece-%d-%d", k, j))
		}
		table[id] = names
		windows = append(windows, models.TestWindow{TestCase: id, Start: start(k).Add(500 * time.Millisecond), End: start(k).Add(3 * time.Second)})
		pool = append(pool, fmt.Sprintf("gap-%d", k))
	}
	storeStream(t, a, 1+tests*per, mock)
	ctx := context.Background()
	require.NoError(t, a.SetScopeTable(ctx, table))
	require.NoError(t, a.SetRecordedWindows(ctx, windows))
	base := heapNow()
	held := func() float64 { return (float64(heapNow()) - float64(base)) / testSize }
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": pool}))
	claimSelf(t, a)
	if h := held(); h > 0.5 {
		t.Fatalf("staged: the live heap holds %.2f tests' recordings (%d MiB each) over the store's: the staging decoded the tests' own", h, testSize>>20)
	}
	for k := 0; k < tests; k++ {
		name := fmt.Sprintf("Test%d", k)
		_, serr := a.ProvenBegin(ctx, begin(name))
		require.Nil(t, serr)
		if h := held(); h > 1.5 {
			t.Fatalf("test %d open: the live heap holds %.2f tests' recordings over the store's", k, h)
		}
		code, body := p.callSettled(t, fmt.Sprintf("/own/%d", k))
		require.Equal(t, 200, code, body)
		require.Equal(t, fmt.Sprintf("own %d", k), body)
		_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: name, CanSkip: true, Outcome: models.ScopeOutcomePassed})
		require.Nil(t, serr)
		if h := held(); h > 0.5 {
			t.Fatalf("test %d closed: the live heap holds %.2f tests' recordings over the store's: a closed test's are kept", k, h)
		}
		code, body = p.callSettled(t, fmt.Sprintf("/gap/%d", k))
		require.Equal(t, 200, code, body)
		require.Equal(t, fmt.Sprintf("gap %d", k), body)
	}
}

// perTestNames is the names a per-test read of kind shows now: what a parser
// that reads only the per-test tier for a call (Mongo's index, after the
// first test fired) can be answered with.
func perTestNames(t *testing.T, p *diskServingProxy, kind models.Kind) []string {
	t.Helper()
	mocks, err := p.mm.GetFilteredMocksByKind(kind)
	require.NoError(t, err)
	names := make([]string, 0, len(mocks))
	for _, mk := range mocks {
		names = append(names, mk.Name)
	}
	sort.Strings(names)
	return names
}

// After a set's last test, a per-test read shows the pool's recording made
// after every test's own (a Go mongo-driver's endSessions at
// client.Disconnect), and it answers once. What tells "after every test" is
// the whole recording on disk, not what the staging read: the staging reads
// no test's own, and with the bound read from the trees no recording was
// after every test (api-server's full round: the endSessions after its last
// test missed, and CI would be red).
func TestMockReplayAnswersTheCallAfterTheLastTest(t *testing.T) {
	a, p := diskReplayAgent(t)
	replaySet(t, a)
	ctx := context.Background()
	require.NoError(t, a.SetRecordedWindows(ctx, replayWindows))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": {"boot", "gap", "late"}}))
	claimSelf(t, a)
	got, e := runTest(t, a, p.servingProxy, "TestA", "/a", "/a2")
	require.Equal(t, []string{"200 a", "200 a2"}, got)
	require.Empty(t, e.Action, e.Reason)
	got, e = runTest(t, a, p.servingProxy, "TestB", "/b")
	require.Equal(t, []string{"200 b"}, got)
	require.Empty(t, e.Action, e.Reason)
	require.Equal(t, []string{"late"}, perTestNames(t, p, models.HTTP),
		"after the last test a per-test read does not show the recording made after every test's own")
	late := ownMock("late", "/late", "late", t0.Add(5*time.Second))
	require.True(t, p.mm.DeleteFilteredMock(*late), "the recording made after the last test did not answer the call after it")
	require.Empty(t, perTestNames(t, p, models.HTTP), "the call after the last test, made once more, is shown the recording that answered it")
	require.False(t, p.mm.DeleteFilteredMock(*late), "FALSE GREEN: the call after the last test, made once more, was answered again")
}

// A recording made between two tests is never the set's teardown, though the
// staging read a test's own recording made before the set's first test (an
// early call of the test's, in the startup band): the bound is the last
// recording of every test, read from the whole recording on disk. Shown
// after the last test, the gap's recording would answer a late call made
// before another test's own, and the learn step would learn as noise a value
// that test's answer gave the application.
func TestARecordingBetweenTestsIsNeverAfterTheLastTest(t *testing.T) {
	a, p := diskReplayAgent(t)
	storeOnDisk(t, a,
		ownMock("boot", "/boot", "boot", t0.Add(-time.Second)),
		ownMock("mock-a0", "/a0", "a0", t0.Add(-500*time.Millisecond)),
		ownMock("mock-a", "/a", "a", t0.Add(time.Second)),
		ownMock("gap", "/gap", "gap", t0.Add(2500*time.Millisecond)),
		ownMock("mock-b", "/b", "b", t0.Add(3*time.Second)),
		ownMock("late", "/late", "late", t0.Add(5*time.Second)))
	ctx := context.Background()
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{
		"o::TestA": {"mock-a0", "mock-a"},
		"o::TestB": {"mock-b"},
	}))
	require.NoError(t, a.SetRecordedWindows(ctx, replayWindows))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": {"boot", "gap", "late"}}))
	claimSelf(t, a)
	got, e := runTest(t, a, p.servingProxy, "TestA", "/a0", "/a")
	require.Equal(t, []string{"200 a0", "200 a"}, got)
	require.Empty(t, e.Action, e.Reason)
	got, e = runTest(t, a, p.servingProxy, "TestB", "/b")
	require.Equal(t, []string{"200 b"}, got)
	require.Empty(t, e.Action, e.Reason)
	require.Equal(t, []string{"late"}, perTestNames(t, p, models.HTTP),
		"FALSE GREEN? after the last test a per-test read shows a recording made before a test's own")
}
