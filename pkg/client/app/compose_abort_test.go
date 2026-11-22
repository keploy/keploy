package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/internal/testnet"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// abortDaemon is a daemon holding one compose project's containers, by name
// (a name is its ID here). It lists containers newest first, as the daemon
// does, and its events streams stay open and quiet. trail is what happened,
// in order: each list of the project ("list"), each removal ("remove NAME" or
// "remove NAME force").
type abortDaemon struct {
	docker.Client
	mu      sync.Mutex
	project string
	cs      map[string]*abortContainer
	made    int
	trail   []string
}

type abortContainer struct {
	name, service, status string
	exitCode              int
	// project is the container's compose project when it is not the
	// daemon's (another stack on the same daemon).
	project string
	// oneOff: a `docker compose run` container of service, not one of the
	// up's.
	oneOff bool
	made   int
}

func newAbortDaemon(project string, cs ...*abortContainer) *abortDaemon {
	d := &abortDaemon{project: project, cs: map[string]*abortContainer{}}
	d.add(cs...)
	return d
}

// add makes the containers cs, in that order.
func (d *abortDaemon) add(cs ...*abortContainer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range cs {
		d.made++
		c.made = d.made
		d.cs[c.name] = c
	}
}

func (d *abortDaemon) labels(c *abortContainer) map[string]string {
	project := d.project
	if c.project != "" {
		project = c.project
	}
	// compose labels every container of its own with oneoff, True or False.
	oneOff := "False"
	if c.oneOff {
		oneOff = "True"
	}
	return map[string]string{composeProjectLabelKey: project, composeServiceLabelKey: c.service, composeOneOffLabelKey: oneOff}
}

// Info says the daemon's time.
func (d *abortDaemon) Info(context.Context) (system.Info, error) {
	return system.Info{SystemTime: time.Now().Format(time.RFC3339Nano)}, nil
}

func (d *abortDaemon) Events(context.Context, events.ListOptions) (<-chan events.Message, <-chan error) {
	return make(chan events.Message), make(chan error)
}

func (d *abortDaemon) ContainerList(_ context.Context, o container.ListOptions) ([]container.Summary, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []container.Summary
	for _, c := range d.cs {
		if (!o.All && c.status != "running") || (o.Filters.Contains("label") && !o.Filters.MatchKVList("label", d.labels(c))) {
			continue
		}
		out = append(out, container.Summary{ID: c.name, Names: []string{"/" + c.name}, Labels: d.labels(c), Created: int64(c.made)})
	}
	slices.SortFunc(out, func(x, y container.Summary) int { return int(y.Created - x.Created) })
	if o.Filters.ExactMatch("label", composeProjectLabelKey+"="+d.project) {
		d.trail = append(d.trail, "list")
	}
	return out, nil
}

func (d *abortDaemon) ContainerInspect(_ context.Context, ref string) (container.InspectResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.cs[ref]
	if !ok {
		return container.InspectResponse{}, errdefs.NotFound(fmt.Errorf("No such container: %s", ref))
	}
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{ID: c.name, Name: "/" + c.name,
			State: &container.State{Status: c.status, Running: c.status == "running", ExitCode: c.exitCode}},
		Config: &container.Config{Labels: d.labels(c)},
	}, nil
}

func (d *abortDaemon) ContainerRemove(_ context.Context, ref string, o container.RemoveOptions) error {
	d.mu.Lock()
	c, ok := d.cs[ref]
	if !ok {
		d.mu.Unlock()
		return errdefs.NotFound(fmt.Errorf("No such container: %s", ref))
	}
	if c.status == "running" && !o.Force {
		d.mu.Unlock()
		return errdefs.Conflict(errors.New("cannot remove a running container"))
	}
	delete(d.cs, ref)
	entry := "remove " + ref
	if o.Force {
		entry += " force"
	}
	d.trail = append(d.trail, entry)
	d.mu.Unlock()
	return nil
}

func (d *abortDaemon) ContainerLogs(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
	return nil, errors.New("no logs here")
}

func (d *abortDaemon) trailNow() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.trail...)
}

// abortedUpRun is run() of the compose-dep-retry lane's app on a daemon,
// with the stand-in docker CLI (stubDockerCLI): the first up runs until the
// file ended exists (only a kill ends it before), then exits firstUpCode; the
// retried up exits 0 at once. ran gives run()'s result once it returns, and
// stop is the run's stop.
type abortedUpRun struct {
	argvLog, ended string
	ran            chan models.AppError
	stop           context.CancelFunc
}

// startAbortedUpRun starts that run() on dc, and returns once its first up
// has started its child (see stubDockerCLI). The test's cleanup ends the
// stand-in up and the run.
func startAbortedUpRun(t *testing.T, dc docker.Client, firstUpCode string) abortedUpRun {
	t.Helper()
	ps := `{"Service":"dep","State":"exited","ExitCode":3,"ID":"tmpproj-dep-1"}
{"Service":"runner","State":"created","ExitCode":0,"ID":"runner"}
{"Service":"keploy-agent","State":"exited","ExitCode":0,"ID":"keploy-v3-test"}
`
	if firstUpCode == "0" {
		// The read of an up that reported 0 is the retry's too: a ps that
		// says nothing must cost it nothing.
		ps = ""
	}
	r := abortedUpRun{argvLog: stubDockerCLI(t, ps, 0), ended: filepath.Join(t.TempDir(), "up-ends"), ran: make(chan models.AppError, 1)}
	t.Setenv("KEPLOY_STUB_UP_WAIT_FILE", r.ended)
	t.Setenv("KEPLOY_STUB_UP_CODE", firstUpCode)
	a := &App{
		logger:          zap.NewNop(),
		docker:          dc,
		kind:            utils.DockerCompose,
		cmd:             utils.NewAppCommand("docker compose -f /tmp/keploy-compose.yaml up"),
		composeFile:     "/tmp/keploy-compose.yaml",
		keployContainer: "keploy-v3-test",
		container:       "runner",
		composeService:  "runner",
		composeServices: []string{"dep", "runner", keployAgentComposeService},
		opts:            models.SetupOptions{Mode: models.MODE_RECORD},
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.stop = cancel
	go func() { r.ran <- a.run(ctx) }()
	t.Cleanup(func() {
		// The stand-in up waits for this file: one keploy did not end (its
		// kill broken) ends here, not as an orphan of the test.
		_ = os.WriteFile(r.ended, nil, 0o600)
		cancel()
		select {
		case appErr := <-r.ran:
			r.ran <- appErr
		case <-time.After(time.Minute):
			// A run() stuck past its stop (a read that outlives the watch)
			// fails the test here rather than hanging the package.
			t.Errorf("run() did not return within a minute of its context's end")
		}
	})
	// Until the first up has set its traps and started its child, an end of
	// it would not be the end of an up that has them.
	eventually(t, "the up starts", func() bool { return strings.Contains(recordedLog(t, r.argvLog), "UP-CHILD-STARTED") })
	return r
}

// flakyDaemon is an abortDaemon that fails reads the way a starved daemon
// does. Once listNeeds is set, every list of the project answers only to a
// caller whose cap leaves it at least that long, as a daemon slower than
// keploy's first cap on every read does; others' it answers only once their
// cap has passed. Each list it so holds is handed to stalling, when that is
// set, before it waits, and is counted by how its context ended: in
// listCapped when its deadline passed, in listCanceled when its caller
// cancelled it. listErrs and agentErrs are scripts: the project's next lists
// and the next inspects of keploy's agent's container (agent) fail with them,
// one each, in turn (a nil one answers), and then answer. Once listFails is
// set, every list of the project fails, as a daemon's that fails for good;
// once agentFails is set, every inspect of keploy's agent's container fails,
// as a daemon's that refuses it for good (a socket proxy's rule). lists counts
// every list of the project. Every inspect of inspectFails fails.
type flakyDaemon struct {
	*abortDaemon
	agent        string
	inspectFails string

	fmu                      sync.Mutex
	listNeeds                time.Duration
	stalling                 func(context.Context)
	listCapped, listCanceled int
	listErrs, agentErrs      []error
	listFails, agentFails    bool
	lists                    int
}

// next takes the next error of *script, nil once it is done.
func (d *flakyDaemon) next(script *[]error) error {
	d.fmu.Lock()
	defer d.fmu.Unlock()
	if len(*script) == 0 {
		return nil
	}
	err := (*script)[0]
	*script = (*script)[1:]
	return err
}

func (d *flakyDaemon) ContainerList(ctx context.Context, o container.ListOptions) ([]container.Summary, error) {
	if o.Filters.ExactMatch("label", composeProjectLabelKey+"="+d.project) {
		d.fmu.Lock()
		d.lists++
		d.fmu.Unlock()
		if err := d.next(&d.listErrs); err != nil {
			return nil, err
		}
		d.fmu.Lock()
		fails := d.listFails && slices.Equal(o.Filters.Get("label"), []string{composeProjectLabelKey + "=" + d.project})
		d.fmu.Unlock()
		if fails {
			return nil, errors.New("Error response from daemon: i/o timeout")
		}
		d.fmu.Lock()
		needs, stalling := d.listNeeds, d.stalling
		d.fmu.Unlock()
		if dl, ok := ctx.Deadline(); needs > 0 && (!ok || time.Until(dl) < needs) {
			if stalling != nil {
				stalling(ctx)
			}
			<-ctx.Done()
			d.fmu.Lock()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				d.listCapped++
			} else {
				d.listCanceled++
			}
			d.fmu.Unlock()
			return nil, ctx.Err()
		}
	}
	return d.abortDaemon.ContainerList(ctx, o)
}

func (d *flakyDaemon) ContainerInspect(ctx context.Context, ref string) (container.InspectResponse, error) {
	if ref == d.inspectFails {
		return container.InspectResponse{}, errors.New("Error response from daemon: i/o timeout")
	}
	if ref == d.agent {
		if err := d.next(&d.agentErrs); err != nil {
			return container.InspectResponse{}, err
		}
		d.fmu.Lock()
		fails := d.agentFails
		d.fmu.Unlock()
		if fails {
			return container.InspectResponse{}, errdefs.Forbidden(errors.New("Error response from daemon: forbidden by the socket proxy"))
		}
	}
	return d.abortDaemon.ContainerInspect(ctx, ref)
}

// realConnectionFailure is the error a real Engine API client gives when
// nothing serves its DOCKER_HOST (here a socket that does not exist): the
// error the watch reads as keploy's client cannot connect.
func realConnectionFailure(t *testing.T) error {
	t.Helper()
	cli, err := client.NewClientWithOpts(client.WithHost("unix://" + filepath.Join(testnet.SocketDir(t), "docker.sock")))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cli.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err = cli.Info(ctx)
	if !client.IsErrConnectionFailed(err) {
		t.Fatalf("a client at a socket that does not exist gave %v, not a connection failure", err)
	}
	return err
}

// realClientInfo reads the daemon's time with a real Engine API client made
// with opts, and gives the read's error.
func realClientInfo(t *testing.T, opts ...client.Opt) error {
	t.Helper()
	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cli.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err = cli.Info(ctx)
	if err == nil {
		t.Fatal("the read of the daemon's time succeeded")
	}
	return err
}

// droppingDaemon serves a unix socket that accepts each connection, reads
// the request's head, writes answer (if any) and closes the connection: with
// no answer, a daemon (or a socket proxy in front of it) that keploy's
// client reaches and that drops each ask before any answer. accepted, if
// not nil, counts the connections it accepted.
func droppingDaemon(t *testing.T, accepted *atomic.Int32, answer ...byte) string {
	t.Helper()
	sock := filepath.Join(testnet.SocketDir(t), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	serveAndDrop(t, l, func(c net.Conn) {
		if accepted != nil {
			accepted.Add(1)
		}
		if len(answer) > 0 {
			_, _ = c.Write(answer)
		}
	})
	return sock
}

// serveAndDrop accepts each connection of l until the test ends, reads its
// request's head, runs drop on it and closes it.
func serveAndDrop(t *testing.T, l net.Listener, drop func(net.Conn)) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.SetDeadline(time.Now().Add(10 * time.Second))
			var head []byte
			buf := make([]byte, 4096)
			for !strings.Contains(string(head), "\r\n\r\n") {
				n, err := c.Read(buf)
				head = append(head, buf[:n]...)
				if err != nil {
					break
				}
			}
			drop(c)
			_ = c.Close()
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		<-done
	})
}

// TestOnlyAFailedDialCountsTowardsAnUnreachableDaemon: with a real Engine
// API client, the failures composeAbortCannotDial counts towards the watch's
// end (composeAbortUnreachableAsks of them in a row) are the client's failed
// dials: no socket, nobody listening, one keploy's user may not open, a
// host or an HTTP proxy that does not resolve, a Windows named pipe that
// does not exist (go-winio's dial error). The client calls a connection the
// daemon accepted and dropped or reset before any answer, or shut before it
// read the ask (on Linux the client's write of it fails), or answered with
// what is no HTTP, a connection failure too: none of those count, that
// daemon is there and the watch stays on. Nor does a dial the read's cap
// ended, which the client does not call a connection failure.
func TestOnlyAFailedDialCountsTowardsAnUnreachableDaemon(t *testing.T) {
	failedDial := func(err error) client.Opt {
		return client.WithDialContext(func(context.Context, string, string) (net.Conn, error) { return nil, err })
	}
	notFound := func(name string) error {
		return &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}}
	}
	for _, tc := range []struct {
		name string
		unix bool   // the case serves a unix socket
		op   string // the op of a *net.OpError the client's error carries, if it must carry one
		opts func(t *testing.T) []client.Opt
		want bool
	}{
		{name: "no socket", unix: true, want: true, opts: func(t *testing.T) []client.Opt {
			return []client.Opt{client.WithHost("unix://" + filepath.Join(testnet.SocketDir(t), "docker.sock"))}
		}},
		{name: "a socket nobody listens on", unix: true, want: true, opts: func(t *testing.T) []client.Opt {
			p := filepath.Join(testnet.SocketDir(t), "docker.sock")
			l, err := net.Listen("unix", p)
			if err != nil {
				t.Fatal(err)
			}
			l.(*net.UnixListener).SetUnlinkOnClose(false)
			_ = l.Close()
			return []client.Opt{client.WithHost("unix://" + p)}
		}},
		{name: "a socket keploy's user may not open", unix: true, want: true, opts: func(t *testing.T) []client.Opt {
			if os.Geteuid() == 0 {
				t.Skip("root opens any socket")
			}
			p := filepath.Join(testnet.SocketDir(t), "docker.sock")
			l, err := net.Listen("unix", p)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = l.Close() })
			if err := os.Chmod(p, 0); err != nil {
				t.Fatal(err)
			}
			return []client.Opt{client.WithHost("unix://" + p)}
		}},
		{name: "a host that does not resolve", want: true, opts: func(*testing.T) []client.Opt {
			return []client.Opt{client.WithHost("tcp://daemon.invalid:2375"), failedDial(notFound("daemon.invalid"))}
		}},
		{name: "an HTTP proxy that does not resolve", want: true, opts: func(*testing.T) []client.Opt {
			return []client.Opt{client.WithHost("tcp://daemon.invalid:2375"), client.WithHTTPClient(&http.Client{Transport: &http.Transport{
				Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: "proxy.invalid:3128"}),
				DialContext: func(context.Context, string, string) (net.Conn, error) {
					return nil, notFound("proxy.invalid")
				},
			}})}
		}},
		{name: "a Windows named pipe that does not exist", want: true, opts: func(*testing.T) []client.Opt {
			// go-winio's DialPipeContext, the client's dial of an npipe://
			// host, fails so (tryDialPipe).
			return []client.Opt{client.WithHost("tcp://127.0.0.1:2375"), failedDial(&os.PathError{Op: "open", Path: "//./pipe/docker_engine",
				Err: errors.New("The system cannot find the file specified.")})}
		}},
		{name: "a daemon that drops the connection before any answer", unix: true, opts: func(t *testing.T) []client.Opt {
			return []client.Opt{client.WithHost("unix://" + droppingDaemon(t, nil))}
		}},
		{name: "a daemon that will not read the ask", unix: true, op: "write", opts: func(t *testing.T) []client.Opt {
			if runtime.GOOS != "linux" {
				// Only Linux tells the peer of a SHUT_RD: on darwin and
				// BSD the write succeeds and the client waits for an
				// answer.
				t.Skip("only Linux fails a write to a peer that shut its read side")
			}
			// A daemon, or a socket proxy that turns a peer away, that
			// shuts its side of the connection before it reads the ask:
			// the client's write of the ask fails (broken pipe). The dial
			// waits for that shutdown, so the write always comes after
			// it, and the connection stays open until the test ends, so
			// the client's read of the answer ends no sooner.
			sock := filepath.Join(testnet.SocketDir(t), "docker.sock")
			l, err := net.Listen("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			shut := make(chan struct{}, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				var held []net.Conn
				defer func() {
					for _, c := range held {
						_ = c.Close()
					}
				}()
				for {
					c, err := l.Accept()
					if err != nil {
						return
					}
					held = append(held, c)
					_ = c.(*net.UnixConn).CloseRead()
					select {
					case shut <- struct{}{}:
					default:
					}
				}
			}()
			t.Cleanup(func() {
				_ = l.Close()
				<-done
			})
			return []client.Opt{client.WithHost("unix://" + sock), client.WithDialContext(func(ctx context.Context, _, _ string) (net.Conn, error) {
				c, err := (&net.Dialer{}).DialContext(ctx, "unix", sock)
				if err != nil {
					return nil, err
				}
				select {
				case <-shut:
					return c, nil
				case <-ctx.Done():
					_ = c.Close()
					return nil, ctx.Err()
				}
			})}
		}},
		{name: "a daemon that resets the connection before any answer", op: "read", opts: func(t *testing.T) []client.Opt {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			serveAndDrop(t, l, func(c net.Conn) { _ = c.(*net.TCPConn).SetLinger(0) })
			return []client.Opt{client.WithHost("tcp://" + l.Addr().String())}
		}},
		{name: "a daemon that answers what is no HTTP (TLS to a plain client)", unix: true, opts: func(t *testing.T) []client.Opt {
			return []client.Opt{client.WithHost("unix://" + droppingDaemon(t, nil, []byte("\x15\x03\x01\x00\x02\x02\x28\r\n")...))}
		}},
		{name: "a dial the read's cap ended", opts: func(*testing.T) []client.Opt {
			return []client.Opt{client.WithHost("tcp://127.0.0.1:2375"), client.WithDialContext(func(context.Context, string, string) (net.Conn, error) {
				// The dial net gives once the read's context passed its
				// deadline.
				ended, cancel := context.WithDeadline(context.Background(), time.Now())
				defer cancel()
				return (&net.Dialer{}).DialContext(ended, "tcp", "127.0.0.1:9")
			})}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.unix && runtime.GOOS == "windows" {
				t.Skip("a unix socket DOCKER_HOST")
			}
			err := realClientInfo(t, tc.opts(t)...)
			if tc.op != "" {
				var ops []string
				var op *net.OpError
				for e := err; errors.As(e, &op); e = op.Err {
					ops = append(ops, op.Op)
				}
				if !slices.Contains(ops, tc.op) {
					t.Fatalf("the client gave %v (ops %q), with no *net.OpError of op %q", err, ops, tc.op)
				}
			}
			if got := composeAbortCannotDial(err); got != tc.want {
				t.Fatalf("composeAbortCannotDial(%v) = %v, want %v (client.IsErrConnectionFailed: %v)", err, got, tc.want, client.IsErrConnectionFailed(err))
			}
		})
	}
}

// removedSinceListDaemon lists removed, a container it no longer has, in its
// first list of the project only, as the daemon does for a container removed
// between that list and its inspect.
type removedSinceListDaemon struct {
	*abortDaemon
	removed       *abortContainer
	listedRemoved atomic.Bool
}

func (d *removedSinceListDaemon) ContainerList(ctx context.Context, o container.ListOptions) ([]container.Summary, error) {
	out, err := d.abortDaemon.ContainerList(ctx, o)
	if err != nil || !o.Filters.ExactMatch("label", composeProjectLabelKey+"="+d.project) || !d.listedRemoved.CompareAndSwap(false, true) {
		return out, err
	}
	return append(out, container.Summary{ID: d.removed.name, Names: []string{"/" + d.removed.name}, Labels: d.labels(d.removed)}), nil
}

// TestAnUpThatReportedZeroIsReadFromTheProject: an up that returned no error
// is an abort only as the daemon reads the project: a dependency crashed, the
// app's container never started, keploy's agent did not crash. Each other
// reading keeps the clean end, which the stopped containers and the run's end
// decide. Only a clean up of a compose run the stop has not ended is read at
// all.
func TestAnUpThatReportedZeroIsReadFromTheProject(t *testing.T) {
	dep := &abortContainer{name: "dep", service: "dep", status: "exited", exitCode: 3}
	agent := &abortContainer{name: "keploy-v3-test", service: keployAgentComposeService, status: "exited", exitCode: 143}
	appNever := &abortContainer{name: "runner", service: "runner", status: "created"}
	failed := utils.CmdError{Type: utils.Runtime, Err: errors.New("exit status 3")}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name    string
		cs      []*abortContainer
		kind    utils.CmdType
		service string
		agent   string
		noCli   bool
		ctx     context.Context
		up      utils.CmdError
		// wantReads is how many times the project was listed; wantAbort,
		// whether the up's end is the abort's error; unasked, that the
		// daemon was not asked at all.
		wantReads int
		wantAbort bool
		unasked   bool
	}{
		{name: "a dependency's crash before the app", cs: []*abortContainer{dep, agent, appNever}, wantReads: 1, wantAbort: true},
		{name: "an app that ran and exited 0", cs: []*abortContainer{dep, agent, {name: "runner", service: "runner", status: "exited"}}, wantReads: 1},
		{name: "an app that still runs", cs: []*abortContainer{dep, agent, {name: "runner", service: "runner", status: "running"}}, wantReads: 1},
		{name: "a project with no crashed dependency", cs: []*abortContainer{{name: "dep", service: "dep", status: "exited"}, agent, appNever}, wantReads: 1},
		{name: "keploy's agent crashed", cs: []*abortContainer{dep, {name: "keploy-v3-test", service: keployAgentComposeService, status: "exited", exitCode: 1}, appNever}, wantReads: 1},
		{name: "a compose run that crashed is no dependency's crash", cs: []*abortContainer{{name: "dep-run", service: "dep", status: "exited", exitCode: 3, oneOff: true}, agent, appNever}, wantReads: 1},
		{name: "another project's crash", cs: []*abortContainer{{name: "other-dep", service: "dep", status: "exited", exitCode: 3, project: "other"}, agent, appNever}, wantReads: 1},
		{name: "keploy's agent's container is gone", cs: []*abortContainer{dep, appNever}},
		{name: "an up that failed", cs: []*abortContainer{dep, agent, appNever}, up: failed, unasked: true},
		{name: "a stopped run", cs: []*abortContainer{dep, agent, appNever}, ctx: cancelled, unasked: true},
		{name: "not a compose run", cs: []*abortContainer{dep, agent, appNever}, kind: utils.DockerRun, unasked: true},
		{name: "an app's service keploy does not know", cs: []*abortContainer{dep, agent, appNever}, service: "-", unasked: true},
		{name: "an agent keploy does not know", cs: []*abortContainer{dep, agent, appNever}, agent: "-", unasked: true},
		{name: "no docker client", cs: []*abortContainer{dep, agent, appNever}, noCli: true, unasked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.kind == "" {
				tc.kind = utils.DockerCompose
			}
			switch tc.service {
			case "":
				tc.service = "runner"
			case "-":
				tc.service = ""
			}
			switch tc.agent {
			case "":
				tc.agent = "keploy-v3-test"
			case "-":
				tc.agent = ""
			}
			if tc.ctx == nil {
				tc.ctx = context.Background()
			}
			cs := make([]*abortContainer, 0, len(tc.cs))
			for _, c := range tc.cs {
				made := *c
				cs = append(cs, &made)
			}
			d := &askCountingDaemon{abortDaemon: newAbortDaemon("tmpproj", cs...)}
			core, logs := observer.New(zap.InfoLevel)
			a := &App{logger: zap.New(core), kind: tc.kind, composeService: tc.service, keployContainer: tc.agent}
			if !tc.noCli {
				a.docker = d
			}
			defer failOnPanic(t)
			got := a.composeAbortReportedZero(tc.ctx, tc.up)
			said := logs.FilterMessageSnippet("docker compose reported 0").Len()
			if want := map[bool]int{true: 1}[tc.wantAbort]; said != want {
				t.Fatalf("keploy said so %d time(s), want %d", said, want)
			}
			if reads := d.lists.Load(); int(reads) != tc.wantReads {
				t.Fatalf("the project was read %d time(s), want %d\n%v", reads, tc.wantReads, d.trailNow())
			}
			if asks := d.inspects.Load(); tc.unasked && asks != 0 {
				t.Fatalf("keploy asked the daemon %d time(s) about a run it does not read", asks)
			}
			if tc.wantAbort {
				if got.Type != utils.Runtime || !errors.Is(got.Err, errComposeAbortReportedZero) {
					t.Fatalf("got %+v, want the abort's Runtime error", got)
				}
				return
			}
			if got != tc.up {
				t.Fatalf("got %+v, want the up's own end %+v", got, tc.up)
			}
		})
	}
}

// failOnPanic, deferred, fails the test on a panic of its goroutine: a read
// keploy should not have made is the row's failure, not the test binary's.
func failOnPanic(t *testing.T) {
	t.Helper()
	if p := recover(); p != nil {
		t.Fatalf("panic: %v", p)
	}
}

// askCountingDaemon counts every inspect and every list it is asked.
type askCountingDaemon struct {
	*abortDaemon
	inspects, lists atomic.Int32
}

func (d *askCountingDaemon) ContainerInspect(ctx context.Context, ref string) (container.InspectResponse, error) {
	d.inspects.Add(1)
	return d.abortDaemon.ContainerInspect(ctx, ref)
}

func (d *askCountingDaemon) ContainerList(ctx context.Context, o container.ListOptions) ([]container.Summary, error) {
	d.lists.Add(1)
	return d.abortDaemon.ContainerList(ctx, o)
}

// scriptedInspectDaemon answers the inspects of the container named name
// with answers, in turn, and as the abortDaemon once they are spent.
type scriptedInspectDaemon struct {
	askCountingDaemon
	name    string
	mu      sync.Mutex
	answers []container.InspectResponse
}

func (d *scriptedInspectDaemon) ContainerInspect(ctx context.Context, ref string) (container.InspectResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ref == d.name && len(d.answers) > 0 {
		next := d.answers[0]
		d.answers = d.answers[1:]
		return next, nil
	}
	return d.abortDaemon.ContainerInspect(ctx, ref)
}

// TestAnUpThatReportedZeroIsReadUntilTheDaemonAnswers: no clock decides what
// an up that reported 0 was. A read that runs past any cap is read again, with
// no end but the run's, so an abort on a slow daemon is still the abort. Only
// answers end the read with the up's clean end, when nothing is left to wait
// for: a refusal, a count of failures, a daemon keploy cannot connect to.
func TestAnUpThatReportedZeroIsReadUntilTheDaemonAnswers(t *testing.T) {
	busy := errdefs.System(errors.New("Error response from daemon: internal error"))
	refused := errdefs.Forbidden(errors.New("Error response from daemon: forbidden by the socket proxy"))
	unreachable := realConnectionFailure(t)
	project := func() *abortDaemon {
		return newAbortDaemon("tmpproj",
			&abortContainer{name: "dep", service: "dep", status: "exited", exitCode: 3},
			&abortContainer{name: "keploy-v3-test", service: keployAgentComposeService, status: "exited"},
			&abortContainer{name: "runner", service: "runner", status: "created"})
	}
	flaky := func(arm func(*flakyDaemon)) func() (docker.Client, *flakyDaemon) {
		return func() (docker.Client, *flakyDaemon) {
			fd := &flakyDaemon{abortDaemon: project(), agent: "keploy-v3-test"}
			arm(fd)
			return fd, fd
		}
	}
	for _, tc := range []struct {
		name      string
		dc        func() (docker.Client, *flakyDaemon)
		wantLists int
		wantAbort bool
	}{
		{name: "a list that fails is read again", wantAbort: true, wantLists: 3, dc: flaky(func(d *flakyDaemon) {
			d.listErrs = []error{busy, busy}
		})},
		{name: "an agent's read that fails is read again", wantAbort: true, wantLists: 1, dc: flaky(func(d *flakyDaemon) {
			d.agentErrs = []error{busy, busy}
		})},
		// Past as many caps as the failures that end a read, and still read:
		// a slow read is not a failure.
		{name: "a daemon slower than each of the first caps is still read", wantAbort: true, wantLists: composeEndReadFailures + 1, dc: flaky(func(d *flakyDaemon) {
			d.listNeeds = composeAbortFirstReadCap<<(composeEndReadFailures-1) + time.Second
		})},
		{name: "a daemon that keeps failing the read ends it", wantLists: composeEndReadFailures, dc: flaky(func(d *flakyDaemon) {
			d.listFails = true
		})},
		{name: "a list the daemon refuses ends the read", wantLists: 1, dc: flaky(func(d *flakyDaemon) {
			d.listErrs = []error{refused}
		})},
		{name: "an agent's read the daemon refuses ends the read", dc: flaky(func(d *flakyDaemon) {
			d.agentFails = true
		})},
		// A failed dial is composeAbortLost's to count, apart from failures:
		// the one failure before them leaves the read going.
		{name: "a daemon keploy cannot connect to ends the read", wantLists: 1 + composeAbortUnreachableAsks, dc: flaky(func(d *flakyDaemon) {
			d.listErrs = append([]error{busy}, slices.Repeat([]error{unreachable}, composeAbortUnreachableAsks)...)
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dc, fd := tc.dc()
			core, logs := observer.New(zap.InfoLevel)
			a := &App{logger: zap.New(core), docker: dc, kind: utils.DockerCompose, composeService: "runner", keployContainer: "keploy-v3-test"}
			got := a.composeAbortReportedZero(context.Background(), utils.CmdError{})
			if lists := fd.lists; lists != tc.wantLists {
				t.Fatalf("the project was listed %d time(s), want %d", lists, tc.wantLists)
			}
			if tc.wantAbort != errors.Is(got.Err, errComposeAbortReportedZero) {
				t.Fatalf("got %+v, abort %v (logs %v)", got, tc.wantAbort, logs.All())
			}
		})
	}

	// A read with no answer is left for the run's stop, not for a clock.
	t.Run("a daemon that never answers is read until the run ends", func(t *testing.T) {
		fd := &flakyDaemon{abortDaemon: project(), agent: "keploy-v3-test", listNeeds: time.Hour}
		a := &App{logger: zap.NewNop(), docker: fd, kind: utils.DockerCompose, composeService: "runner", keployContainer: "keploy-v3-test"}
		ctx, stop := context.WithCancel(context.Background())
		defer stop()
		read := make(chan utils.CmdError, 1)
		go func() { read <- a.composeAbortReportedZero(ctx, utils.CmdError{}) }()
		eventually(t, "keploy reads the project again", func() bool {
			fd.fmu.Lock()
			defer fd.fmu.Unlock()
			return fd.lists >= 2
		})
		select {
		case got := <-read:
			t.Fatalf("the read ended by itself with %+v, on a daemon that did not answer", got)
		default:
		}
		stop()
		select {
		case got := <-read:
			if got.Err != nil {
				t.Fatalf("got %+v, want the up's own clean end", got)
			}
		case <-time.After(time.Minute):
			t.Fatal("the read outlived the run")
		}
	})

	// Removed since the list: the rest of the project still answers.
	t.Run("a container removed since the list is passed over", func(t *testing.T) {
		d := &removedSinceListDaemon{abortDaemon: project(), removed: &abortContainer{name: "gone", service: "other", status: "exited"}}
		a := &App{logger: zap.NewNop(), docker: d, kind: utils.DockerCompose, composeService: "runner", keployContainer: "keploy-v3-test"}
		if got := a.composeAbortReportedZero(context.Background(), utils.CmdError{}); !errors.Is(got.Err, errComposeAbortReportedZero) {
			t.Fatalf("got %+v, want the abort's error", got)
		}
	})

	// An answer that lacks a state is no answer: it is read again.
	t.Run("a container with no state is read again", func(t *testing.T) {
		d := &scriptedInspectDaemon{askCountingDaemon: askCountingDaemon{abortDaemon: project()}, name: "dep",
			answers: []container.InspectResponse{{}, {ContainerJSONBase: &container.ContainerJSONBase{}}}}
		a := &App{logger: zap.NewNop(), docker: d, kind: utils.DockerCompose, composeService: "runner", keployContainer: "keploy-v3-test"}
		defer failOnPanic(t)
		if got := a.composeAbortReportedZero(context.Background(), utils.CmdError{}); !errors.Is(got.Err, errComposeAbortReportedZero) {
			t.Fatalf("got %+v, want the abort's error", got)
		}
		if lists := d.lists.Load(); lists != 3 {
			t.Fatalf("the project was listed %d time(s), want 3", lists)
		}
	})

	// An agent's container with no labels names no project to read.
	t.Run("an agent's container with no config names no project", func(t *testing.T) {
		d := &scriptedInspectDaemon{askCountingDaemon: askCountingDaemon{abortDaemon: project()}, name: "keploy-v3-test",
			answers: []container.InspectResponse{{ContainerJSONBase: &container.ContainerJSONBase{}}}}
		a := &App{logger: zap.NewNop(), docker: d, kind: utils.DockerCompose, composeService: "runner", keployContainer: "keploy-v3-test"}
		defer failOnPanic(t)
		if got := a.composeAbortReportedZero(context.Background(), utils.CmdError{}); got.Err != nil {
			t.Fatalf("got %+v, want the up's own clean end", got)
		}
		if lists := d.lists.Load(); lists != 0 {
			t.Fatalf("the project was listed %d time(s) with no name, want 0", lists)
		}
	})
}

// crashesOnEveryUpDaemon is an abortDaemon whose dependency, once crashes is
// set, an up has made by the next list, and which crashed again.
type crashesOnEveryUpDaemon struct {
	*abortDaemon
	crashes atomic.Bool
}

func (d *crashesOnEveryUpDaemon) ContainerList(ctx context.Context, o container.ListOptions) ([]container.Summary, error) {
	d.mu.Lock()
	_, there := d.cs["tmpproj-dep-1"]
	d.mu.Unlock()
	if !there && d.crashes.Load() {
		d.add(&abortContainer{name: "tmpproj-dep-1", service: "dep", status: "exited", exitCode: 3})
	}
	return d.abortDaemon.ContainerList(ctx, o)
}

// TestADependencyThatCrashesOnEveryUpThatReportsZeroFailsTheRun: compose
// reports 0 on every up, and the dependency crashes on every one. The retries
// are spent and the run ends on the abort's error, not as an app that stopped
// clean: `keploy mock` took that end for a test command that succeeded.
func TestADependencyThatCrashesOnEveryUpThatReportsZeroFailsTheRun(t *testing.T) {
	d := &crashesOnEveryUpDaemon{abortDaemon: newAbortDaemon("tmpproj")}
	r := startAbortedUpRun(t, d, "0")
	d.add(&abortContainer{name: "keploy-v3-test", service: keployAgentComposeService, status: "exited"},
		&abortContainer{name: "runner", service: "runner", status: "created"})
	d.crashes.Store(true)
	if err := os.WriteFile(r.ended, nil, 0o600); err != nil {
		t.Fatalf("could not end the stand-in up: %v", err)
	}
	var appErr models.AppError
	select {
	case appErr = <-r.ran:
		r.ran <- appErr
	case <-time.After(5 * time.Minute):
		t.Fatalf("run() did not return\n%s", recordedLog(t, r.argvLog))
	}
	if appErr.AppErrorType != models.ErrUnExpected || !errors.Is(appErr.Err, errComposeAbortReportedZero) {
		t.Fatalf("run() reported %+v, want the abort's error\n%v", appErr, d.trailNow())
	}
	if ups := strings.Count(recordedLog(t, r.argvLog), "ARG up\n"); ups != composeDepFailureRetries+1 {
		t.Fatalf("%d ups, want the first and its %d retries\n%s", ups, composeDepFailureRetries, recordedLog(t, r.argvLog))
	}
}

// TestTheRetryTakesTheReadThatFoundTheAbort: the read that found an up that
// reported 0 to be an abort is the retry's too. A daemon that refuses the next
// read, with a ps that says nothing, costs that retry nothing.
func TestTheRetryTakesTheReadThatFoundTheAbort(t *testing.T) {
	d := &flakyDaemon{abortDaemon: newAbortDaemon("tmpproj"), agent: "keploy-v3-test"}
	r := startAbortedUpRun(t, d, "0")
	d.add(&abortContainer{name: "tmpproj-dep-1", service: "dep", status: "exited", exitCode: 3},
		&abortContainer{name: "keploy-v3-test", service: keployAgentComposeService, status: "exited"},
		&abortContainer{name: "runner", service: "runner", status: "created"})
	d.fmu.Lock()
	d.listErrs = []error{nil, errdefs.Forbidden(errors.New("Error response from daemon: forbidden by the socket proxy"))}
	d.fmu.Unlock()
	if err := os.WriteFile(r.ended, nil, 0o600); err != nil {
		t.Fatalf("could not end the stand-in up: %v", err)
	}
	select {
	case appErr := <-r.ran:
		r.ran <- appErr
	case <-time.After(time.Minute):
		t.Fatalf("run() did not return within a minute\n%s", recordedLog(t, r.argvLog))
	}
	if log := recordedLog(t, r.argvLog); !strings.Contains(log, "UP-RETRIED") {
		t.Fatalf("the bring-up was not retried\n%s", log)
	}
}

// TestAFailedUpIsRetriedOnTheDaemonsAnswer: the retry of an up that failed
// rests on the daemon's answer, not on `docker compose ps`. ps loads the project
// again without what the user's command gave the up, so a file that needs such
// a variable fails every ps (rc=1, "required variable TAG is missing a value"
// on v5.1.3): read from ps, that up was never retried.
func TestAFailedUpIsRetriedOnTheDaemonsAnswer(t *testing.T) {
	t.Setenv("KEPLOY_STUB_PS_CODE", "1")
	d := newAbortDaemon("tmpproj")
	r := startAbortedUpRun(t, d, "3")
	d.add(&abortContainer{name: "tmpproj-dep-1", service: "dep", status: "exited", exitCode: 3},
		&abortContainer{name: "keploy-v3-test", service: keployAgentComposeService, status: "exited"},
		&abortContainer{name: "runner", service: "runner", status: "created"})
	if err := os.WriteFile(r.ended, nil, 0o600); err != nil {
		t.Fatalf("could not end the stand-in up: %v", err)
	}
	var appErr models.AppError
	select {
	case appErr = <-r.ran:
		r.ran <- appErr
	case <-time.After(time.Minute):
		t.Fatalf("run() did not return within a minute\n%s", recordedLog(t, r.argvLog))
	}
	if log := recordedLog(t, r.argvLog); !strings.Contains(log, "UP-RETRIED") {
		t.Fatalf("the bring-up was not retried (run() returned %+v)\n%s", appErr, log)
	}
	if appErr.AppErrorType != models.ErrAppStopped {
		t.Fatalf("the retried up exited 0, run() reported %+v", appErr)
	}
	if !slices.Contains(d.trailNow(), "remove tmpproj-dep-1 force") {
		t.Fatalf("the retry did not remove the crashed dependency: %v", d.trailNow())
	}
}

// TestAFailedUpIsRetriedOnASlowDaemonsAnswer: no clock decides the retry of a
// failed up. A daemon slower than each of the read's first caps is read until
// it answers, and that answer decides: the up is retried, and ps is not asked.
// A budget on the read would end it first and hand the decision to ps.
func TestAFailedUpIsRetriedOnASlowDaemonsAnswer(t *testing.T) {
	d := newAbortDaemon("tmpproj")
	fd := &flakyDaemon{abortDaemon: d, agent: "keploy-v3-test"}
	r := startAbortedUpRun(t, fd, "3")
	// Armed once the up has started: listNeeds holds a list with no label
	// filter too. Past as many caps as the failures that end a read, as a
	// daemon slower than each of them.
	fd.fmu.Lock()
	fd.listNeeds = composeAbortFirstReadCap<<(composeEndReadFailures-1) + time.Second
	fd.fmu.Unlock()
	d.add(&abortContainer{name: "tmpproj-dep-1", service: "dep", status: "exited", exitCode: 3},
		&abortContainer{name: "keploy-v3-test", service: keployAgentComposeService, status: "exited"},
		&abortContainer{name: "runner", service: "runner", status: "created"})
	if err := os.WriteFile(r.ended, nil, 0o600); err != nil {
		t.Fatalf("could not end the stand-in up: %v", err)
	}
	eventually(t, "the failed up is retried", func() bool { return strings.Contains(recordedLog(t, r.argvLog), "UP-RETRIED") })
	// The run asks ps of keploy's agent before its first up: what counts is
	// from that up's end to the retry.
	log := recordedLog(t, r.argvLog)
	done, retried := strings.Index(log, "UP-DONE"), strings.Index(log, "UP-RETRIED")
	if done < 0 || retried < done {
		t.Fatalf("the failed up's end and its retry are not in the log, in that order\n%s", log)
	}
	if strings.Contains(log[done:retried], "ARG ps\n") {
		t.Fatalf("keploy asked ps for the failed up's states: the daemon's read ended before the daemon answered\n%s", log)
	}
	// The retried up's own read is TestAnUpThatReportedZeroIsReadUntilTheDaemonAnswers's.
	fd.fmu.Lock()
	fd.listNeeds = 0
	capped := fd.listCapped
	fd.fmu.Unlock()
	if capped < composeEndReadFailures {
		t.Fatalf("%d of the project's lists ran past their cap, want at least %d: the row does not test a daemon slower than the first caps", capped, composeEndReadFailures)
	}
	var appErr models.AppError
	select {
	case appErr = <-r.ran:
		r.ran <- appErr
	case <-time.After(time.Minute):
		t.Fatalf("run() did not return within a minute\n%s", recordedLog(t, r.argvLog))
	}
	if appErr.AppErrorType != models.ErrAppStopped {
		t.Fatalf("the retried up exited 0, run() reported %+v", appErr)
	}
}

// TestAFailedUpIsReadFromTheDaemonThenFromPs: the daemon's answer decides the
// retry of a failed up, under the proven switch too, and ps is not asked then,
// so a slow ps cannot lose it. ps decides when the daemon gave none: it may be
// the only reader of a daemon keploy's client does not reach.
func TestAFailedUpIsReadFromTheDaemonThenFromPs(t *testing.T) {
	busy := errdefs.System(errors.New("Error response from daemon: internal error"))
	refused := errdefs.Forbidden(errors.New("Error response from daemon: forbidden by the socket proxy"))
	unreachable := realConnectionFailure(t)
	project := func() *abortDaemon {
		return newAbortDaemon("tmpproj",
			&abortContainer{name: "dep", service: "dep", status: "exited", exitCode: 3},
			&abortContainer{name: "keploy-v3-test", service: keployAgentComposeService, status: "exited"},
			&abortContainer{name: "runner", service: "runner", status: "created"})
	}
	flaky := func(arm func(*flakyDaemon)) func() docker.Client {
		return func() docker.Client {
			fd := &flakyDaemon{abortDaemon: project(), agent: "keploy-v3-test"}
			arm(fd)
			return fd
		}
	}
	for _, tc := range []struct {
		name    string
		dc      func() docker.Client
		service string
		proven  bool
		wantPs  bool
	}{
		{name: "the daemon answers", dc: func() docker.Client { return project() }},
		{name: "the daemon answers under the proven switch", dc: func() docker.Client { return project() }, proven: true},
		{name: "keploy's agent's container does not exist", wantPs: true, dc: func() docker.Client {
			return newAbortDaemon("tmpproj", &abortContainer{name: "dep", service: "dep", status: "exited", exitCode: 3})
		}},
		{name: "a project of one-offs alone", wantPs: true, dc: func() docker.Client {
			return newAbortDaemon("tmpproj", &abortContainer{name: "keploy-v3-test", service: keployAgentComposeService, status: "exited", oneOff: true})
		}},
		{name: "a daemon that refuses the read", wantPs: true, dc: flaky(func(d *flakyDaemon) { d.listErrs = []error{refused} })},
		{name: "a daemon that keeps failing the read", wantPs: true, dc: flaky(func(d *flakyDaemon) { d.listFails = true })},
		// The list gives the newest first, so dep's is the read that fails
		// after the others answered: what they said is no answer.
		{name: "a daemon that keeps failing one container's read", wantPs: true, dc: flaky(func(d *flakyDaemon) { d.inspectFails = "dep" })},
		{name: "a daemon keploy cannot connect to", wantPs: true, dc: flaky(func(d *flakyDaemon) {
			d.listErrs = slices.Repeat([]error{unreachable}, composeAbortUnreachableAsks)
		})},
		{name: "a failure before the answer is read again", dc: flaky(func(d *flakyDaemon) { d.listErrs = []error{busy} })},
		{name: "an app's service keploy does not know", wantPs: true, service: "-", dc: func() docker.Client { return project() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.proven {
				t.Setenv(provenswitch.Env, "1")
			}
			argvLog := stubDockerCLI(t, `{"Service":"from-ps","State":"exited","ExitCode":9,"ID":"ps-1"}`+"\n", 0)
			a := &App{logger: zap.NewNop(), docker: tc.dc(), kind: utils.DockerCompose, composeService: "runner", keployContainer: "keploy-v3-test",
				cmd: utils.NewAppCommand("docker compose -f /tmp/keploy-compose.yaml up"), composeFile: "/tmp/keploy-compose.yaml"}
			if tc.service == "-" {
				a.composeService = ""
			}
			defer failOnPanic(t)
			services := []string{}
			for _, s := range a.composeFailedUpStates(context.Background()) {
				services = append(services, s.Service)
			}
			asked := strings.Contains(recordedLog(t, argvLog), "ARG ps\n")
			if tc.wantPs {
				if !asked || !slices.Equal(services, []string{"from-ps"}) {
					t.Fatalf("ps asked: %v; got the services %v, want ps's", asked, services)
				}
				return
			}
			if asked {
				t.Fatalf("keploy asked ps with the daemon's answer in hand\n%s", recordedLog(t, argvLog))
			}
			slices.Sort(services)
			if want := []string{"dep", keployAgentComposeService, "runner"}; !slices.Equal(services, want) {
				t.Fatalf("got the services %v, want the daemon's %v", services, want)
			}
		})
	}
}

// TestARunStoppedDuringAFailedUpsReadEndsIt: on a daemon that never answers,
// only the run's stop ends the read of a failed up, so that stop must reach
// the read: the list the run stops in ends on the stop, not on a deadline of
// its own, run() returns as a stopped run, and the up is not retried.
func TestARunStoppedDuringAFailedUpsReadEndsIt(t *testing.T) {
	d := newAbortDaemon("tmpproj")
	fd := &flakyDaemon{abortDaemon: d, agent: "keploy-v3-test"}
	r := startAbortedUpRun(t, fd, "3")
	// The run stops inside the failed up's first list whose cap has not
	// passed, so no wait decides which ended that list. Lists capped before
	// the stop are no part of the check.
	var once sync.Once
	cappedBefore := -1
	fd.fmu.Lock()
	fd.listNeeds = time.Hour
	fd.stalling = func(ctx context.Context) {
		if ctx.Err() != nil {
			return
		}
		once.Do(func() {
			fd.fmu.Lock()
			cappedBefore = fd.listCapped
			fd.fmu.Unlock()
			r.stop()
		})
	}
	fd.fmu.Unlock()
	// A read the stop did not reach ends here, not with the test binary.
	t.Cleanup(func() {
		fd.fmu.Lock()
		defer fd.fmu.Unlock()
		fd.listNeeds = 0
	})
	d.add(&abortContainer{name: "tmpproj-dep-1", service: "dep", status: "exited", exitCode: 3},
		&abortContainer{name: "keploy-v3-test", service: keployAgentComposeService, status: "exited"},
		&abortContainer{name: "runner", service: "runner", status: "created"})
	if err := os.WriteFile(r.ended, nil, 0o600); err != nil {
		t.Fatalf("could not end the stand-in up: %v", err)
	}
	var appErr models.AppError
	select {
	case appErr = <-r.ran:
		r.ran <- appErr
	case <-time.After(time.Minute):
		t.Fatalf("run() did not return within a minute of its stop: the failed up's read outlives the run\n%s", recordedLog(t, r.argvLog))
	}
	fd.fmu.Lock()
	before, capped, canceled := cappedBefore, fd.listCapped, fd.listCanceled
	fd.fmu.Unlock()
	if before < 0 {
		t.Fatalf("the run was not stopped: no list of the failed up's read was held (run() returned %+v)", appErr)
	}
	if capped != before || canceled != 1 {
		t.Fatalf("after the run's stop %d of the failed up's lists ended on a deadline and %d on the stop, want 0 and 1: the stop did not end the read", capped-before, canceled)
	}
	if appErr.AppErrorType != models.ErrCtxCanceled {
		t.Fatalf("a run stopped during the read reported %+v", appErr)
	}
	if log := recordedLog(t, r.argvLog); strings.Contains(log, "UP-RETRIED") {
		t.Fatalf("a stopped run retried the bring-up\n%s", log)
	}
}

// TestAFailedReadCountsAcrossSlowOnes: a slow read is no failure, and no answer
// either, so it does not start the count again: a daemon that alternates the
// two would otherwise be read for ever.
func TestAFailedReadCountsAcrossSlowOnes(t *testing.T) {
	const wantReads = 2*composeEndReadFailures - 1
	a := &App{logger: zap.NewNop()}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	reads := 0
	err := a.composeEndRead(ctx, "the project", func(rctx context.Context) error {
		reads++
		if reads > wantReads {
			stop() // ends the row, whose check below then fails
			return rctx.Err()
		}
		if reads%2 == 0 {
			<-rctx.Done()
			return rctx.Err()
		}
		return errors.New("Error response from daemon: internal error")
	})
	if err != nil || reads != wantReads {
		t.Fatalf("got %v after %d reads, want no error after %d", err, reads, wantReads)
	}
}
