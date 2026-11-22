package http

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/errdefs"
	"github.com/docker/go-connections/nat"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	"go.uber.org/zap"
)

const restoreNet = "demo_default"
const restoreNetID = "netid-0001"
const restoreNet2 = "demo_backend"
const restoreNet2ID = "netid-0002"

// declaredBindings is empty under -P: the daemon assigns a host port per
// exposed port, so there is nothing declared to compare against.
func declaredBindings(publishAll bool) nat.PortMap {
	if publishAll {
		return nat.PortMap{}
	}
	return nat.PortMap{"9410/tcp": []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: "9410"}}}
}

// notFound is what the daemon returns for a container that is not there, which
// the code under test has to tell apart from every other failure.
func notFound(name string) error {
	return errdefs.NotFound(errors.New("No such container: " + name))
}

// fakeContainer is the daemon state that decides whether a container is broken:
// whether it is up, whether it holds a live network endpoint, and whether its
// declared ports are actually published. They are tracked separately because
// that is how the real failure presents - up, listed on its network, and
// publishing nothing.
type fakeContainer struct {
	running        bool
	hasEndpoint    bool
	publishedPorts bool
	staysDown      bool
}

/*
restoreFake is a small Docker daemon keyed by container name, so rename, create
and remove move real state around. Modelling it this way is the point: a fake
that answers the same inspect however it is driven cannot tell a container that
was repaired from one that was never broken.
*/
type restoreFake struct {
	kdocker.Client

	mu         sync.Mutex
	containers map[string]*fakeContainer

	hostMode        bool
	publishAllPorts bool
	// inspectErrOnce fails only the next inspect, so the retry has something to
	// succeed at; inspectErr fails every one.
	inspectErrOnce    bool
	rebuildStaysShort bool
	// autoRemove models --rm: AutoRemove fires on any exit, including the stop
	// that begins a session, so the container is gone rather than stopped.
	autoRemove bool
	// createHangs makes the create spend the whole budget before failing, which
	// is the state a rollback has to be able to run in.
	createHangs bool
	// idOverride makes the container answering to a name report a different id,
	// as a container that took the name later would.
	idOverride string
	// vanishOnStart models --rm on a container whose process exits at once: the
	// start succeeds and the container is gone immediately afterwards.
	vanishOnStart bool

	calls       []string
	renamedTo   []string
	connected   []string
	removed     []string
	removeOpts  []container.RemoveOptions
	staleCtxOps []string

	starts, stops, removes, creates, renames int

	createdName     string
	createdConfig   *container.Config
	createdHost     *container.HostConfig
	createdNetwork  *network.NetworkingConfig
	createdPlatform *ocispec.Platform

	createErr error
	// createErrForImage fails the create only when it asks for this image
	// reference, so a tag that no longer resolves can be told apart from one
	// that does.
	createErrForImage string
	// renameErr fires only from the renameErrAfter'th rename onward: setting
	// the original aside has to succeed for the rename BACK to be the thing
	// under test.
	renameErr      error
	renameErrAfter int
	removeErr      error
	stopErr        error
	inspectErr     error
	// startErr fires only from the startErrAfter'th start onward. The
	// replacement carries the original's name, so which start fails cannot be
	// expressed by name - and a fake that fails the original's start too never
	// reaches the rebuild whose start is the thing under test.
	startErr      error
	startErrAfter int
	// startErrUntil, when set, stops the failures after that many starts, so a
	// retry has something to succeed at.
	startErrUntil int
}

// resolve maps a name or an id onto the key the fake stores, because the code
// under test addresses containers by whichever the daemon last handed it.
func (f *restoreFake) resolve(nameOrID string) string {
	if _, ok := f.containers[nameOrID]; ok {
		return nameOrID
	}
	return strings.TrimPrefix(nameOrID, "id-")
}

func newRestoreFake(state fakeContainer) *restoreFake {
	return &restoreFake{containers: map[string]*fakeContainer{"demo-app": &state}}
}

// record notes the call and whether it arrived on a context that had already
// expired, which is how a rollback ends up unable to roll anything back.
func (f *restoreFake) record(ctx context.Context, op string) {
	f.calls = append(f.calls, op)
	if ctx.Err() != nil {
		f.staleCtxOps = append(f.staleCtxOps, op)
	}
}

func (f *restoreFake) ContainerInspect(ctx context.Context, name string) (container.InspectResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(ctx, "inspect:"+name)
	if f.inspectErrOnce {
		f.inspectErrOnce = false
		return container.InspectResponse{}, errors.New("error during connect: EOF")
	}
	if f.inspectErr != nil {
		return container.InspectResponse{}, f.inspectErr
	}

	name = f.resolve(name)
	state, ok := f.containers[name]
	if !ok {
		return container.InspectResponse{}, notFound(name)
	}
	id := "id-" + name
	if f.idOverride != "" {
		id = f.idOverride
	}

	// Both networks the spec declares, because a container short of one of them
	// is a container to rebuild - so a healthy fake has to report them all.
	endpoint := &network.EndpointSettings{NetworkID: restoreNetID}
	endpoint2 := &network.EndpointSettings{NetworkID: restoreNet2ID}
	if state.hasEndpoint {
		endpoint = &network.EndpointSettings{
			NetworkID:  restoreNetID,
			EndpointID: "live",
			IPAddress:  "172.18.0.9",
			MacAddress: "02:42:ac:12:00:09",
			Aliases:    []string{"demo-app"},
		}
		endpoint2 = &network.EndpointSettings{NetworkID: restoreNet2ID, EndpointID: "live2", Aliases: []string{"demo-app"}}
	}
	ports := nat.PortMap{"9410/tcp": nil}
	if state.publishedPorts {
		ports = nat.PortMap{"9410/tcp": []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: "9410"}}}
	}
	mode := container.NetworkMode(restoreNet)
	if f.hostMode {
		mode = "host"
	}
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			Name:  "/" + name,
			ID:    id,
			Image: "sha256:deadbeef",
			State: &container.State{Running: state.running, ExitCode: 3},
			HostConfig: &container.HostConfig{
				NetworkMode:     mode,
				PublishAllPorts: f.publishAllPorts,
				PortBindings:    declaredBindings(f.publishAllPorts),
			},
		},
		Config: &container.Config{
			Image:        "demo:local",
			MacAddress:   "02:42:ac:12:00:09",
			ExposedPorts: nat.PortSet{"9410/tcp": struct{}{}},
		},
		Mounts: []container.MountPoint{{Type: mount.TypeVolume, Name: "anon-vol", Destination: "/app/data", RW: true}},
		NetworkSettings: &container.NetworkSettings{
			NetworkSettingsBase: container.NetworkSettingsBase{Ports: ports},
			Networks:            map[string]*network.EndpointSettings{restoreNet: endpoint, restoreNet2: endpoint2},
		},
	}, nil
}

func (f *restoreFake) ContainerStart(ctx context.Context, name string, _ container.StartOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(ctx, "start:"+name)
	f.starts++
	name = f.resolve(name)
	if f.startErr != nil && f.starts > f.startErrAfter && (f.startErrUntil == 0 || f.starts <= f.startErrUntil) {
		return f.startErr
	}
	state, ok := f.containers[name]
	if !ok {
		return notFound(name)
	}
	if f.vanishOnStart {
		delete(f.containers, name)
		return nil
	}
	state.running = !state.staysDown
	return nil
}

// Stopping is what breaks the container: docker drops its network endpoint
// while the agent holds the same network under the app's own alias, and the
// published ports go with the endpoint.
func (f *restoreFake) ContainerStop(ctx context.Context, name string, _ container.StopOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(ctx, "stop:"+name)
	f.stops++
	if f.stopErr != nil {
		return f.stopErr
	}
	name = f.resolve(name)
	state, ok := f.containers[name]
	if !ok {
		return notFound(name)
	}
	if f.autoRemove {
		delete(f.containers, name)
		return nil
	}
	state.running = false
	state.hasEndpoint = false
	state.publishedPorts = false
	return nil
}

func (f *restoreFake) ContainerRename(ctx context.Context, name, newName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(ctx, "rename:"+name+"->"+newName)
	f.renames++
	if f.renameErr != nil && f.renames > f.renameErrAfter {
		return f.renameErr
	}
	name = f.resolve(name)
	state, ok := f.containers[name]
	if !ok {
		return notFound(name)
	}
	if _, taken := f.containers[newName]; taken {
		return errors.New("Conflict. The container name \"" + newName + "\" is already in use")
	}
	delete(f.containers, name)
	f.containers[newName] = state
	f.renamedTo = append(f.renamedTo, newName)
	return nil
}

func (f *restoreFake) ContainerRemove(ctx context.Context, name string, opts container.RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(ctx, "remove:"+name)
	f.removes++
	f.removeOpts = append(f.removeOpts, opts)
	if f.removeErr != nil {
		return f.removeErr
	}
	name = f.resolve(name)
	f.removed = append(f.removed, name)
	delete(f.containers, name)
	return nil
}

func (f *restoreFake) ContainerCreate(ctx context.Context, cfg *container.Config, host *container.HostConfig,
	net *network.NetworkingConfig, platform *ocispec.Platform, name string) (container.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(ctx, "create:"+name)
	f.creates++
	if f.createHangs {
		// Unlocked while waiting: the rollback that follows has to be able to
		// reach the fake.
		f.mu.Unlock()
		<-ctx.Done()
		f.mu.Lock()
		return container.CreateResponse{}, ctx.Err()
	}
	if f.createErr != nil {
		return container.CreateResponse{}, f.createErr
	}
	if f.createErrForImage != "" && cfg.Image == f.createErrForImage {
		return container.CreateResponse{}, errdefs.NotFound(errors.New("No such image: " + cfg.Image))
	}
	if _, taken := f.containers[name]; taken {
		return container.CreateResponse{}, errors.New("Conflict. The container name \"" + name + "\" is already in use")
	}
	f.createdName, f.createdConfig, f.createdHost = name, cfg, host
	f.createdNetwork, f.createdPlatform = net, platform
	// A freshly created container gets a real endpoint and its ports, as the
	// daemon would - unless the test is modelling a rebuild that did not help.
	f.containers[name] = &fakeContainer{hasEndpoint: !f.rebuildStaysShort, publishedPorts: !f.rebuildStaysShort}
	return container.CreateResponse{ID: "id-" + name}, nil
}

func (f *restoreFake) NetworkConnect(ctx context.Context, netName, _ string, _ *network.EndpointSettings) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(ctx, "connect:"+netName)
	f.connected = append(f.connected, netName)
	return nil
}

// state reports a container by name, or nil when the daemon no longer has it.
func (f *restoreFake) state(name string) *fakeContainer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.containers[name]
}

func (f *restoreFake) callIndex(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, call := range f.calls {
		if strings.HasPrefix(call, prefix) {
			return i
		}
	}
	return -1
}

func (f *restoreFake) backupName() string {
	for _, name := range f.renamedTo {
		if strings.Contains(name, backupSuffix) {
			return name
		}
	}
	return ""
}

func restoreAgentWith(fake kdocker.Client, spec *sourceContainerSpec) *AgentClient {
	return &AgentClient{logger: zap.NewNop(), dockerClient: fake, fromContainerSpec: spec}
}

func demoSpec() *sourceContainerSpec {
	return &sourceContainerSpec{
		name:    "demo-app",
		imageID: "sha256:deadbeef",
		config: &container.Config{
			Image:      "demo:local",
			MacAddress: "02:42:ac:12:00:04",
			Labels:     map[string]string{"com.docker.compose.project": "demo"},
			Volumes:    map[string]struct{}{"/app/data": {}},
		},
		hostConfig: &container.HostConfig{
			NetworkMode:  container.NetworkMode(restoreNet),
			PortBindings: nat.PortMap{"9410/tcp": []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: "9410"}}},
			Binds:        []string{"/host/data:/app/data"},
		},
		mounts:   []mount.Mount{{Type: mount.TypeVolume, Source: "anon-vol", Target: "/app/data"}},
		platform: &ocispec.Platform{OS: "linux", Architecture: "arm64"},
		networks: map[string]*network.EndpointSettings{
			restoreNet:  {Aliases: []string{"demo-app"}, NetworkID: restoreNetID, EndpointID: "stale", IPAddress: "172.18.0.4", MacAddress: "02:42:ac:12:00:04"},
			restoreNet2: {Aliases: []string{"demo-app"}, NetworkID: restoreNet2ID, EndpointID: "stale2"},
		},
	}
}

// shortRecreateBudget shrinks the rebuild's budget so a test can watch it run
// out, which is the only way to reach what happens once it has.
func shortRecreateBudget(t *testing.T) func() {
	t.Helper()
	was := recreateBudget
	recreateBudget = 150 * time.Millisecond
	return func() { recreateBudget = was }
}

// runRestore runs the teardown restore with a guard, since every failure mode
// here is either a loop or a stall.
func runRestore(t *testing.T, agent *AgentClient, name string) {
	t.Helper()
	done := make(chan struct{})
	go func() { agent.restoreSourceContainer(name); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("restore did not finish; it is looping or stalling teardown")
	}
}

/*
The most dangerous possible regression: rebuilding a container that was fine.
Rebuilding force-removes the user's container, so it must happen only when the
container is demonstrably broken.
*/
func TestRestoreLeavesAHealthyContainerAlone(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: true, publishedPorts: true})

	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	if fake.creates != 0 || fake.removes != 0 || fake.renames != 0 || fake.stops != 0 {
		t.Fatalf("touched a healthy container: creates=%d removes=%d renames=%d stops=%d",
			fake.creates, fake.removes, fake.renames, fake.stops)
	}
	if fake.starts != 1 {
		t.Fatalf("started %d times; one start is all a healthy container needs", fake.starts)
	}
}

// A container that starts and then exits looks EXACTLY like the bug by inspect
// alone: no endpoint id, no published ports. Rebuilding it would destroy a
// container that was never broken - very much including one that crashes
// because the dependency it talks to moved during the session.
func TestRestoreLeavesAnExitedContainerAlone(t *testing.T) {
	fake := newRestoreFake(fakeContainer{staysDown: true})

	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	if fake.creates != 0 || fake.removes != 0 {
		t.Fatalf("rebuilt a container that had merely exited: creates=%d removes=%d", fake.creates, fake.removes)
	}
}

// host and none ignore port bindings by design, so a container declaring both
// would report a shortfall it can never clear - and be rebuilt for nothing, on
// every pass, until the budget runs out.
func TestRestoreDoesNotRebuildForUnpublishablePortsOnHostNetwork(t *testing.T) {
	// Attached, so the network is not the complaint; publishing nothing, which
	// on host networking is correct rather than broken.
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: true, publishedPorts: false})
	fake.hostMode = true

	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	if fake.creates != 0 {
		t.Fatalf("rebuilt a host-network container over ports it can never publish: creates=%d", fake.creates)
	}
}

// The same state on a bridge network IS the bug: attached, and publishing
// nothing it was asked to publish.
func TestRestoreRebuildsWhenDeclaredPortsAreNotPublished(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: true, publishedPorts: false})

	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	if fake.creates != 1 {
		t.Fatalf("rebuilt %d times; an unpublished port cannot be bound by starting again", fake.creates)
	}
}

func TestRestoreRebuildsWhenTheNetworkIsGone(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})

	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	if fake.creates != 1 {
		t.Fatalf("rebuilt %d times; a container with no endpoint cannot be repaired in place", fake.creates)
	}
	if fake.createdName != "demo-app" {
		t.Fatalf("rebuilt as %q; the name is how everything else refers to it", fake.createdName)
	}
	if state := fake.state("demo-app"); state == nil || !state.running {
		t.Fatalf("left no running container under the user's name: %+v", state)
	}
}

// The original is RUNNING when the rebuild starts - the restore loop started it
// and the shortfall check confirmed it. Creating the replacement without
// stopping it first puts two containers on the same volumes and the same host
// ports.
func TestRebuildStopsTheOriginalBeforeCreatingItsReplacement(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})

	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	stopAt, createAt := fake.callIndex("stop:"), fake.callIndex("create:")
	if stopAt == -1 {
		t.Fatalf("never stopped the original; its replacement ran alongside it. calls=%v", fake.calls)
	}
	if createAt == -1 || stopAt > createAt {
		t.Fatalf("stopped the original after building its replacement. calls=%v", fake.calls)
	}
}

// Force-removing a running container is SIGKILL, bypassing its stop signal and
// grace period. The user's app gets an unclean shutdown - a recovery pass for a
// database, a torn write for anything mid-flush.
func TestRebuildDoesNotKillTheOriginalToRemoveIt(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})

	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	backup := fake.backupName()
	if backup == "" {
		t.Fatal("never set the original aside")
	}
	stopAt, removeAt := fake.callIndex("stop:"+backup), fake.callIndex("remove:"+backup)
	if stopAt == -1 || removeAt == -1 || stopAt > removeAt {
		t.Fatalf("removed the old copy without stopping it first. calls=%v", fake.calls)
	}
}

// The original must be renamed aside, not removed, so a failure can put it back.
func TestRebuildSetsTheOriginalAsideRatherThanRemovingIt(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})

	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	if len(fake.renamedTo) == 0 || !strings.HasPrefix(fake.renamedTo[0], "demo-app"+backupSuffix) {
		t.Fatalf("did not set the original aside first: %v", fake.renamedTo)
	}
	if len(fake.removed) != 1 || fake.removed[0] != fake.backupName() {
		t.Fatalf("removed %v; only the backup should go, and only once the replacement is up", fake.removed)
	}
}

// The replacement mounts the same volumes by name, so the copy being removed is
// only the shell they used to hang off. RemoveVolumes here deletes the user's
// data - the one-word change this whole path exists to avoid.
func TestRebuildNeverRemovesTheVolumesWithTheOldCopy(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})

	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	if len(fake.removeOpts) == 0 {
		t.Fatal("removed nothing, so the options went unchecked")
	}
	for _, opts := range fake.removeOpts {
		if opts.RemoveVolumes {
			t.Fatal("removed the old copy WITH its volumes; the replacement needs that data")
		}
	}
}

// A rebuild that does not clear the shortfall must not be tried again: it is
// destructive, and a loop would spend the whole teardown budget rebuilding the
// user's container over and over.
func TestRestoreRebuildsAtMostOnce(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})
	fake.rebuildStaysShort = true

	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	if fake.creates != 1 {
		t.Fatalf("rebuilt %d times; once is all a destructive repair gets", fake.creates)
	}
}

// If the rebuild cannot be created, the user's container must come back - not
// be left deleted, or parked under a name they never chose.
func TestRebuildPutsTheOriginalBackWhenCreateFails(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})
	fake.createErr = errors.New("no such image")

	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	if state := fake.state("demo-app"); state == nil {
		t.Fatalf("the user's container is not under its own name any more: %v", fake.calls)
	}
	if fake.removes != 0 {
		t.Fatalf("removed something after a failed create: removes=%d - the user's container must survive", fake.removes)
	}
}

// The replacement exists but will not run, so it has to go before the original
// can have its name back.
func TestRebuildPutsTheOriginalBackWhenTheReplacementWillNotStart(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})
	// The original comes up; only the replacement refuses to bind, and keeps
	// refusing, so the retry below runs out rather than succeeding.
	fake.startErr = errors.New("driver failed programming external connectivity")
	fake.startErrAfter = 1
	defer shortRecreateBudget(t)()

	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	if state := fake.state("demo-app"); state == nil {
		t.Fatalf("left nothing under the user's container name: calls=%v", fake.calls)
	}
	if fake.callIndex("remove:id-demo-app") == -1 && fake.callIndex("remove:demo-app") == -1 {
		t.Fatalf("did not remove the container it could not start, so the name stayed taken: %v", fake.calls)
	}
	// The replacement mounts the user's volumes by name, so removing it with
	// its volumes destroys the very data the rollback is preserving.
	for _, opts := range fake.removeOpts {
		if opts.RemoveVolumes {
			t.Fatal("removed the unstartable replacement WITH its volumes; they are the user's")
		}
	}
}

// Rebuilding is destructive, so it gets one attempt. A container that is gone
// again after a rebuild must end the restore rather than start another one, or
// teardown spends its whole budget re-creating and re-removing containers.
func TestRestoreRebuildsAtMostOnceWhenTheContainerKeepsDisappearing(t *testing.T) {
	fake := &restoreFake{containers: map[string]*fakeContainer{}}
	fake.vanishOnStart = true

	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	if fake.creates != 1 {
		t.Fatalf("rebuilt %d times; once is all a destructive repair gets", fake.creates)
	}
}

// A rollback exists to run after something has already overrun. Reusing the
// budget that just expired means the daemon refuses every call, and the user is
// left with their container under a temporary name.
func TestRollbackDoesNotRunOnAnExpiredContext(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})
	fake.createErr = errors.New("no such image")
	agent := restoreAgentWith(fake, demoSpec())

	runRestore(t, agent, "demo-app")

	for _, op := range fake.staleCtxOps {
		if strings.HasPrefix(op, "rename:") || strings.HasPrefix(op, "remove:") {
			t.Fatalf("rollback ran on a context that had already expired: %v", fake.staleCtxOps)
		}
	}
}

// --rm fires AutoRemove on any exit, including the stop that began the session,
// so there is nothing left to set aside or to start. Driven through the real
// entry point, because a container that no longer exists can never produce the
// successful start that the rebuild is otherwise reached through - it would sit
// out the whole teardown budget failing to start instead.
func TestRestoreRebuildsAContainerThatAutoRemovedItself(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: true, publishedPorts: true})
	fake.autoRemove = true
	agent := restoreAgentWith(fake, nil)

	// Stopping it destroys it, exactly as the session's own stop would.
	if _, err := agent.releaseSourceContainer(context.Background(), "demo-app"); err != nil {
		t.Fatal(err)
	}
	if fake.state("demo-app") != nil {
		t.Fatal("the fake did not model --rm; there is nothing to rebuild")
	}

	runRestore(t, agent, "demo-app")

	if fake.creates != 1 {
		t.Fatalf("rebuilt %d times; the container is gone and only a rebuild returns it", fake.creates)
	}
	if state := fake.state("demo-app"); state == nil || !state.running {
		t.Fatalf("did not bring the container back: %+v", state)
	}
	if fake.renames != 0 {
		t.Fatalf("tried to set aside a container that was already gone: renames=%d", fake.renames)
	}
}

// Pressing on after a failed stop is how the original ends up running beside
// its replacement. The broken container publishes nothing, so nothing refuses
// the replacement's ports either - two containers then share the volumes and
// the force-remove afterwards SIGKILLs one of them.
func TestRebuildAbortsWhenTheOriginalWillNotStop(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})
	fake.stopErr = errors.New("cannot stop container: permission denied")

	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	if fake.creates != 0 {
		t.Fatalf("built a replacement beside a container it could not stop: creates=%d", fake.creates)
	}
	if fake.removes != 0 {
		t.Fatalf("removed a container it could not stop: removes=%d", fake.removes)
	}
	if state := fake.state("demo-app"); state == nil {
		t.Fatalf("did not put the original back under its own name: calls=%v", fake.calls)
	}
}

// The name is all the rebuild has to go on otherwise, and names get reused - by
// a compose run in another terminal, or by a fresh container taking a name that
// --rm freed. Force-removing whatever answers to the name replaces a container
// keploy never captured with a stale spec.
func TestRebuildRefusesAContainerItNeverCaptured(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})
	fake.idOverride = "id-someone-elses-container"
	spec := demoSpec()
	spec.id = "id-demo-app"

	runRestore(t, restoreAgentWith(fake, spec), "demo-app")

	if fake.creates != 0 || fake.removes != 0 || fake.renames != 0 {
		t.Fatalf("rebuilt over a container it never captured: creates=%d removes=%d renames=%d",
			fake.creates, fake.removes, fake.renames)
	}
}

// Reading any inspect failure as "the container is gone" goes on to create over
// one that is still there, and turns a daemon blip into an abandoned repair.
func TestRebuildDoesNotTreatADaemonFailureAsAMissingContainer(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})
	fake.inspectErr = errors.New("error during connect: EOF")

	if err := restoreAgentWith(fake, demoSpec()).recreateSourceContainer(); err == nil {
		t.Fatal("reported success after it could not even look at the container")
	}
	if fake.creates != 0 {
		t.Fatalf("created over a container whose state it could not read: creates=%d", fake.creates)
	}
}

// A rollback exists to run after something has already overrun, so the budget
// that just expired is the one context it cannot use. Without its own, the
// daemon refuses every call and the user is left with their container parked
// under a name they never chose.
func TestRollbackRunsAfterTheRebuildBudgetIsSpent(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})
	// The create spends the entire budget and then fails on it, so the rollback
	// runs with the rebuild's own context already expired.
	fake.createHangs = true
	defer shortRecreateBudget(t)()

	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	for _, op := range fake.staleCtxOps {
		if strings.HasPrefix(op, "rename:") || strings.HasPrefix(op, "remove:") {
			t.Fatalf("rollback ran on the context that had just expired: %v", fake.staleCtxOps)
		}
	}
	if state := fake.state("demo-app"); state == nil || !state.running {
		t.Fatalf("did not put the original back and restart it: %+v (calls=%v)", state, fake.calls)
	}
}

// If the name cannot be given back, the container is neither where the user
// left it nor running - so it has to at least be started, and the message has
// to carry both steps.
func TestRollbackStartsTheContainerItCouldNotRename(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})
	fake.createErr = errors.New("no such image")

	// Setting the original aside works; only giving the name back fails.
	fake.renameErr = errors.New("name already in use")
	fake.renameErrAfter = 1

	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	backup := fake.backupName()
	if backup == "" {
		t.Fatal("never set the original aside")
	}
	if fake.callIndex("start:"+backup) == -1 {
		t.Fatalf("left the container stopped under a temporary name: %v", fake.calls)
	}
}

func TestRebuildCarriesOverWhatTheUserDeclared(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})

	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	if got := fake.createdHost.PortBindings["9410/tcp"]; len(got) != 1 || got[0].HostPort != "9410" {
		t.Fatalf("port bindings lost: %v", got)
	}
	if fake.createdConfig.Labels["com.docker.compose.project"] != "demo" {
		t.Fatalf("compose labels lost: %v", fake.createdConfig.Labels)
	}
	// Anonymous volumes only appear in Mounts. Replaying Config.Volumes or
	// Binds alongside them hands the app new empty volumes and orphans its data.
	if len(fake.createdHost.Mounts) != 1 || fake.createdHost.Mounts[0].Source != "anon-vol" {
		t.Fatalf("mounts lost: %v", fake.createdHost.Mounts)
	}
	if fake.createdConfig.Volumes != nil || fake.createdHost.Binds != nil {
		t.Fatalf("left Volumes/Binds alongside Mounts: volumes=%v binds=%v", fake.createdConfig.Volumes, fake.createdHost.Binds)
	}
	endpoint := fake.createdNetwork.EndpointsConfig[restoreNet]
	if endpoint == nil || len(endpoint.Aliases) != 1 || endpoint.Aliases[0] != "demo-app" {
		t.Fatalf("aliases lost: %+v", endpoint)
	}
	if endpoint.EndpointID != "" || endpoint.IPAddress != "" {
		t.Fatalf("replayed the discarded endpoint: %+v", endpoint)
	}
	// Read back from a running container, MacAddress is whatever the daemon
	// generated - on a bridge, derived from the address it assigned. Replayed as
	// a request it can pin a MAC another container now holds. Both spellings
	// carry it, and the deprecated top-level one rides along in the shallow
	// copy of Config.
	if endpoint.MacAddress != "" {
		t.Fatalf("replayed a generated MAC address as a requested one: %q", endpoint.MacAddress)
	}
	if fake.createdConfig.MacAddress != "" {
		t.Fatalf("replayed the generated MAC through Config: %q", fake.createdConfig.MacAddress)
	}
}

// The capture is the actual fix: without it there is nothing to rebuild from.
func TestSpecFromInspectCapturesWhatARebuildNeeds(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: true, publishedPorts: true})
	inspect, err := fake.ContainerInspect(context.Background(), "demo-app")
	if err != nil {
		t.Fatal(err)
	}

	spec := specFromInspect(inspect)
	if spec == nil {
		t.Fatal("captured nothing")
	}
	if spec.name != "demo-app" {
		t.Fatalf("name not un-prefixed: %q", spec.name)
	}
	if spec.imageID != "sha256:deadbeef" {
		t.Fatalf("image id not captured: %q - a tag can stop resolving before the rebuild", spec.imageID)
	}
	if len(spec.mounts) != 1 || spec.mounts[0].Source != "anon-vol" {
		t.Fatalf("anonymous volume not captured: %v", spec.mounts)
	}
	if _, ok := spec.networks[restoreNet]; !ok {
		t.Fatalf("networks not captured: %v", spec.networks)
	}
}

func TestSpecFromInspectRefusesAPartialInspect(t *testing.T) {
	if specFromInspect(container.InspectResponse{}) != nil {
		t.Fatal("built a spec from an inspect with no base, config or host config")
	}
}

// Stopping the container is what breaks it, so a spec captured afterwards would
// record the broken state - no endpoint, no published ports - and rebuilding
// from it would faithfully reproduce the bug.
func TestReleaseCapturesTheSpecBeforeStoppingTheContainer(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: true, publishedPorts: true})
	agent := restoreAgentWith(fake, nil)

	wasRunning, err := agent.releaseSourceContainer(context.Background(), "demo-app")
	if err != nil || !wasRunning {
		t.Fatalf("releaseSourceContainer() = %v, %v", wasRunning, err)
	}

	if inspectAt, stopAt := fake.callIndex("inspect:"), fake.callIndex("stop:"); inspectAt == -1 || stopAt < inspectAt {
		t.Fatalf("call order was %v; the capture has to come first", fake.calls)
	}
	spec := agent.fromContainerSpec
	if spec == nil {
		t.Fatal("captured no spec, so teardown has nothing to rebuild from")
	}
	if endpoint := spec.networks[restoreNet]; endpoint == nil || len(endpoint.Aliases) == 0 {
		t.Fatalf("captured the network without what identifies the container on it: %+v", endpoint)
	}
	if len(spec.hostConfig.PortBindings) == 0 {
		t.Fatal("captured no port bindings, so a rebuild would publish nothing")
	}
}

// A container that was already stopped is not keploy's to start again, and
// there is nothing to put back.
func TestReleaseLeavesAnAlreadyStoppedContainerAlone(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: false})
	agent := restoreAgentWith(fake, nil)

	wasRunning, err := agent.releaseSourceContainer(context.Background(), "demo-app")

	if err != nil || wasRunning {
		t.Fatalf("releaseSourceContainer() = %v, %v; want false, nil", wasRunning, err)
	}
	if fake.stops != 0 {
		t.Fatalf("stopped a container that was not running: stops=%d", fake.stops)
	}
}

// --from-container takes an id as happily as a name, and the rebuilt container
// has a new one. Verifying against the id that was just removed reports a
// working rebuild as a failure, after stalling the whole teardown budget.
func TestRestoreAddressesTheContainerByNameEvenWhenGivenAnID(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})

	runRestore(t, restoreAgentWith(fake, demoSpec()), "9f2c1ab4de77")

	if fake.creates != 1 {
		t.Fatalf("rebuilt %d times", fake.creates)
	}
	for _, call := range fake.calls {
		if strings.HasSuffix(call, ":9f2c1ab4de77") && strings.HasPrefix(call, "start:") {
			t.Fatalf("kept starting the id it had removed: %v", fake.calls)
		}
	}
}

// NetworkMode reads "default" for the default bridge and can be a network ID,
// neither of which is a key in the endpoint map - so the primary endpoint's
// aliases would be dropped and then warned about as a network to reconnect.
func TestPrimaryNetworkResolvesToTheEndpointMapKey(t *testing.T) {
	networks := map[string]*network.EndpointSettings{restoreNet: {NetworkID: restoreNetID}}

	if got := primaryNetwork(&container.HostConfig{NetworkMode: "default"}, nil); got != "bridge" {
		t.Fatalf("primaryNetwork(default) = %q, want bridge", got)
	}
	if got := primaryNetwork(&container.HostConfig{NetworkMode: restoreNet}, networks); got != restoreNet {
		t.Fatalf("primaryNetwork(name) = %q", got)
	}
	if got := primaryNetwork(&container.HostConfig{NetworkMode: container.NetworkMode(restoreNetID)}, networks); got != restoreNet {
		t.Fatalf("primaryNetwork(id) = %q, want the map key %q", got, restoreNet)
	}
}

// A container on more than one network has to come back on all of them. It is
// created on the primary so it is never briefly attached to nothing, and the
// rest are connected before it starts.
func TestRebuildPutsTheContainerBackOnEveryNetwork(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})

	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	if _, ok := fake.createdNetwork.EndpointsConfig[restoreNet]; !ok {
		t.Fatalf("not created on its primary network: %v", fake.createdNetwork.EndpointsConfig)
	}
	if len(fake.connected) != 1 || fake.connected[0] != restoreNet2 {
		t.Fatalf("did not reattach the secondary networks: %v", fake.connected)
	}
	// Connected before the start: an endpoint added afterwards is a container
	// that came up on one network and had another appear under it.
	if fake.callIndex("connect:"+restoreNet2) > fake.callIndex("start:id-demo-app") {
		t.Fatalf("attached a network after starting the container: %v", fake.calls)
	}
}

// The image is the one thing a rebuild cannot do without, and Config.Image is a
// TAG - it can stop resolving between the capture and the rebuild, while the id
// it resolved to is still on the machine.
func TestRebuildFallsBackToTheImageItWasRunning(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})
	fake.createErrForImage = "demo:local"

	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	if fake.createdConfig == nil || fake.createdConfig.Image != "sha256:deadbeef" {
		t.Fatalf("did not retry against the image it was running: %+v", fake.createdConfig)
	}
	if state := fake.state("demo-app"); state == nil || !state.running {
		t.Fatalf("did not bring the container back: %+v", state)
	}
}

// The platform is what a rebuild of an emulated container needs: without it the
// daemon picks the host's, and an amd64 image on an arm64 host silently becomes
// a different container.
func TestRebuildAsksForThePlatformItCaptured(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})

	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	if fake.createdPlatform == nil || fake.createdPlatform.Architecture != "arm64" {
		t.Fatalf("rebuilt without the captured platform: %+v", fake.createdPlatform)
	}
}

// Inspect renders Links as `/child:/parent/alias` rather than what was asked
// for, so feeding it back produces an alias containing a slash and the daemon
// refuses the whole create - losing the container over a legacy flag.
func TestRebuildDropsLegacyLinksRatherThanFailing(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})
	spec := demoSpec()
	spec.hostConfig.Links = []string{"/demo-db:/demo-app/db"}

	runRestore(t, restoreAgentWith(fake, spec), "demo-app")

	if fake.creates != 1 {
		t.Fatalf("did not rebuild: creates=%d", fake.creates)
	}
	if len(fake.createdHost.Links) != 0 {
		t.Fatalf("replayed inspect's rendering of --link: %v", fake.createdHost.Links)
	}
}

// A backup stranded by an earlier failed run must not take the one name the
// repair needs, or the container can never be rebuilt again.
func TestRebuildIsNotBlockedByAStrandedBackup(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})
	fake.containers["demo-app"+backupSuffix] = &fakeContainer{}

	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	if fake.creates != 1 {
		t.Fatalf("a leftover backup blocked the repair: creates=%d calls=%v", fake.creates, fake.calls)
	}
	if state := fake.state("demo-app"); state == nil || !state.running {
		t.Fatalf("did not bring the container back: %+v", state)
	}
}

// Failing to remove the old copy leaves a stopped duplicate under a temporary
// name. Worth saying, but not worth undoing a rebuild that worked.
func TestRebuildKeepsAWorkingReplacementWhenTheOldCopyWillNotGo(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})
	fake.removeErr = errors.New("device or resource busy")

	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	if state := fake.state("demo-app"); state == nil || !state.running {
		t.Fatalf("undid a rebuild that had already succeeded: %+v", state)
	}
}

// After a rebuild the container is already up, so the wait between attempts has
// nothing to wait for. Timing is the only signal: the sequence of calls is the
// same either way, and the wait is a whole second of teardown.
func TestRestoreDoesNotWaitAfterASuccessfulRebuild(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})

	started := time.Now()
	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	if elapsed := time.Since(started); elapsed > 750*time.Millisecond {
		t.Fatalf("took %s for an in-memory rebuild; it slept through the retry wait", elapsed)
	}
}

// The agent container publishes the app's ports on its behalf and is stopped
// asynchronously, so a rebuild can reach the start while the port it needs is
// still bound. Rolling back over that would abandon a repair that has already
// stopped the user's container, for a condition that clears itself.
func TestRebuildWaitsForThePortsToBeReleasedBeforeGivingUp(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})
	// The original starts, then the replacement's first attempt hits the bind
	// the agent has not let go of yet.
	fake.startErr = errors.New("Bind for 127.0.0.1:9410 failed: port is already allocated")
	fake.startErrAfter = 1
	fake.startErrUntil = 2

	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	if fake.creates != 1 {
		t.Fatalf("creates=%d; the rebuild should have been kept, not redone", fake.creates)
	}
	if state := fake.state("demo-app"); state == nil || !state.running {
		t.Fatalf("gave up on a bind that clears itself: %+v (calls=%v)", state, fake.calls)
	}
	if fake.callIndex("remove:id-demo-app") != -1 {
		t.Fatalf("threw away a replacement that only needed another second: %v", fake.calls)
	}
}

// A container the daemon will never accept must not be retried until the budget
// runs out - that is a minute of teardown spent on a certainty.
func TestRebuildDoesNotRetryAStartTheDaemonRefuses(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})
	fake.startErr = errdefs.InvalidParameter(errors.New("invalid mount config"))
	fake.startErrAfter = 1

	started := time.Now()
	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("spent %s retrying a start the daemon had already refused", elapsed)
	}
}

// `docker run -P` declares no port bindings at all, so there is nothing to
// compare and a container that came back publishing nothing looks restored.
func TestRestoreRebuildsAPublishAllContainerThatPublishesNothing(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: true, publishedPorts: false})
	fake.publishAllPorts = true
	spec := demoSpec()
	spec.hostConfig.PublishAllPorts = true
	spec.hostConfig.PortBindings = nil

	runRestore(t, restoreAgentWith(fake, spec), "demo-app")

	if fake.creates != 1 {
		t.Fatalf("rebuilt %d times; -P publishes per exposed port, so there is no binding to miss", fake.creates)
	}
}

// One failed inspect used to end the restore, where every other transient in
// this loop is retried.
func TestRestoreRetriesAFailedCheckInsteadOfGivingUp(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: true, publishedPorts: true})
	fake.inspectErrOnce = true

	runRestore(t, restoreAgentWith(fake, demoSpec()), "demo-app")

	if fake.creates != 0 {
		t.Fatalf("rebuilt over a check it could not complete: creates=%d", fake.creates)
	}
	if fake.starts < 2 {
		t.Fatalf("gave up after one failed check: starts=%d calls=%v", fake.starts, fake.calls)
	}
}

// Secondary networks are attached in a fixed order so a failure is reproducible
// rather than depending on map iteration.
func TestRebuildAttachesSecondaryNetworksInAStableOrder(t *testing.T) {
	fake := newRestoreFake(fakeContainer{running: true, hasEndpoint: false, publishedPorts: false})
	spec := demoSpec()
	spec.networks["demo_aaa"] = &network.EndpointSettings{Aliases: []string{"demo-app"}}
	spec.networks["demo_zzz"] = &network.EndpointSettings{Aliases: []string{"demo-app"}}

	runRestore(t, restoreAgentWith(fake, spec), "demo-app")

	if len(fake.connected) != 3 {
		t.Fatalf("connected %v; all three secondary networks should be reattached", fake.connected)
	}
	if !sort.StringsAreSorted(fake.connected) {
		t.Fatalf("attached networks in map order: %v", fake.connected)
	}
}
