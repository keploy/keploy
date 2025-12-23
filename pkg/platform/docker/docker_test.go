package docker

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/checkpoint"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/api/types/volume"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// MockDockerClient is a minimal mock implementation of the Docker API client
// It only implements the methods needed for testing CreateVolume
type MockDockerClient struct {
	volumeListFunc   func(ctx context.Context, opts volume.ListOptions) (volume.ListResponse, error)
	volumeRemoveFunc func(ctx context.Context, volumeID string, force bool) error
	volumeCreateFunc func(ctx context.Context, opts volume.CreateOptions) (volume.Volume, error)
}

// Implement only the methods we need for testing
func (m *MockDockerClient) VolumeList(ctx context.Context, opts volume.ListOptions) (volume.ListResponse, error) {
	if m.volumeListFunc != nil {
		return m.volumeListFunc(ctx, opts)
	}
	return volume.ListResponse{}, nil
}

func (m *MockDockerClient) VolumeRemove(ctx context.Context, volumeID string, force bool) error {
	if m.volumeRemoveFunc != nil {
		return m.volumeRemoveFunc(ctx, volumeID, force)
	}
	return nil
}

func (m *MockDockerClient) VolumeCreate(ctx context.Context, opts volume.CreateOptions) (volume.Volume, error) {
	if m.volumeCreateFunc != nil {
		return m.volumeCreateFunc(ctx, opts)
	}
	return volume.Volume{}, nil
}

// Stub implementations for all other required methods
func (m *MockDockerClient) ClientVersion() string    { return "" }
func (m *MockDockerClient) DaemonHost() string       { return "" }
func (m *MockDockerClient) HTTPClient() *http.Client { return nil }
func (m *MockDockerClient) ServerVersion(ctx context.Context) (types.Version, error) {
	return types.Version{}, nil
}
func (m *MockDockerClient) NegotiateAPIVersion(ctx context.Context) { /* stub */ }
func (m *MockDockerClient) NegotiateAPIVersionPing(p types.Ping)    { /* stub */ }
func (m *MockDockerClient) DialHijack(ctx context.Context, url, proto string, meta map[string][]string) (net.Conn, error) {
	return nil, nil
}
func (m *MockDockerClient) Dialer() func(context.Context) (net.Conn, error) { return nil }
func (m *MockDockerClient) Close() error                                    { return nil }
func (m *MockDockerClient) CheckpointCreate(ctx context.Context, containerID string, options checkpoint.CreateOptions) error {
	return nil
}
func (m *MockDockerClient) CheckpointDelete(ctx context.Context, containerID string, options checkpoint.DeleteOptions) error {
	return nil
}
func (m *MockDockerClient) CheckpointList(ctx context.Context, containerID string, options checkpoint.ListOptions) ([]checkpoint.Summary, error) {
	return nil, nil
}
func (m *MockDockerClient) ContainerAttach(ctx context.Context, containerID string, options container.AttachOptions) (types.HijackedResponse, error) {
	return types.HijackedResponse{}, nil
}
func (m *MockDockerClient) ContainerCommit(ctx context.Context, containerID string, options container.CommitOptions) (container.CommitResponse, error) {
	return container.CommitResponse{}, nil
}
func (m *MockDockerClient) ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *v1.Platform, containerName string) (container.CreateResponse, error) {
	return container.CreateResponse{}, nil
}
func (m *MockDockerClient) ContainerDiff(ctx context.Context, container string) ([]container.FilesystemChange, error) {
	return nil, nil
}
func (m *MockDockerClient) ContainerExecAttach(ctx context.Context, execID string, config container.ExecAttachOptions) (types.HijackedResponse, error) {
	return types.HijackedResponse{}, nil
}
func (m *MockDockerClient) ContainerExecCreate(ctx context.Context, containerID string, config container.ExecOptions) (container.ExecCreateResponse, error) {
	return container.ExecCreateResponse{}, nil
}
func (m *MockDockerClient) ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error) {
	return container.ExecInspect{}, nil
}
func (m *MockDockerClient) ContainerExecResize(ctx context.Context, execID string, options container.ResizeOptions) error {
	return nil
}
func (m *MockDockerClient) ContainerExecStart(ctx context.Context, execID string, config container.ExecStartOptions) error {
	return nil
}
func (m *MockDockerClient) ContainerExport(ctx context.Context, container string) (io.ReadCloser, error) {
	return nil, nil
}
func (m *MockDockerClient) ContainerInspect(ctx context.Context, container string) (types.ContainerJSON, error) {
	return types.ContainerJSON{}, nil
}
func (m *MockDockerClient) ContainerInspectWithRaw(ctx context.Context, container string, getSize bool) (types.ContainerJSON, []byte, error) {
	return types.ContainerJSON{}, nil, nil
}
func (m *MockDockerClient) ContainerKill(ctx context.Context, container, signal string) error {
	return nil
}
func (m *MockDockerClient) ContainerList(ctx context.Context, options container.ListOptions) ([]types.Container, error) {
	return nil, nil
}
func (m *MockDockerClient) ContainerLogs(ctx context.Context, containerID string, options container.LogsOptions) (io.ReadCloser, error) {
	return nil, nil
}
func (m *MockDockerClient) ContainerPause(ctx context.Context, container string) error { return nil }
func (m *MockDockerClient) ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error {
	return nil
}
func (m *MockDockerClient) ContainerRename(ctx context.Context, containerID, newContainerName string) error {
	return nil
}
func (m *MockDockerClient) ContainerResize(ctx context.Context, containerID string, options container.ResizeOptions) error {
	return nil
}
func (m *MockDockerClient) ContainerRestart(ctx context.Context, container string, options container.StopOptions) error {
	return nil
}
func (m *MockDockerClient) ContainerStatPath(ctx context.Context, containerID, path string) (container.PathStat, error) {
	return container.PathStat{}, nil
}
func (m *MockDockerClient) ContainerStats(ctx context.Context, containerID string, stream bool) (container.StatsResponseReader, error) {
	return container.StatsResponseReader{}, nil
}
func (m *MockDockerClient) ContainerStatsOneShot(ctx context.Context, containerID string) (container.StatsResponseReader, error) {
	return container.StatsResponseReader{}, nil
}
func (m *MockDockerClient) ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error {
	return nil
}
func (m *MockDockerClient) ContainerStop(ctx context.Context, container string, options container.StopOptions) error {
	return nil
}
func (m *MockDockerClient) ContainerTop(ctx context.Context, containerID string, arguments []string) (container.ContainerTopOKBody, error) {
	return container.ContainerTopOKBody{}, nil
}
func (m *MockDockerClient) ContainerUnpause(ctx context.Context, containerID string) error {
	return nil
}
func (m *MockDockerClient) ContainerUpdate(ctx context.Context, containerID string, updateConfig container.UpdateConfig) (container.ContainerUpdateOKBody, error) {
	return container.ContainerUpdateOKBody{}, nil
}
func (m *MockDockerClient) ContainerWait(ctx context.Context, container string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	return nil, nil
}
func (m *MockDockerClient) CopyFromContainer(ctx context.Context, containerID, srcPath string) (io.ReadCloser, container.PathStat, error) {
	return nil, container.PathStat{}, nil
}
func (m *MockDockerClient) CopyToContainer(ctx context.Context, containerID, path string, content io.Reader, options container.CopyToContainerOptions) error {
	return nil
}
func (m *MockDockerClient) ContainersPrune(ctx context.Context, pruneFilters filters.Args) (container.PruneReport, error) {
	return container.PruneReport{}, nil
}
func (m *MockDockerClient) DistributionInspect(ctx context.Context, image, encodedRegistryAuth string) (registry.DistributionInspect, error) {
	return registry.DistributionInspect{}, nil
}
func (m *MockDockerClient) ImageBuild(ctx context.Context, buildContext io.Reader, options types.ImageBuildOptions) (types.ImageBuildResponse, error) {
	return types.ImageBuildResponse{}, nil
}
func (m *MockDockerClient) BuildCachePrune(ctx context.Context, opts types.BuildCachePruneOptions) (*types.BuildCachePruneReport, error) {
	return nil, nil
}
func (m *MockDockerClient) BuildCancel(ctx context.Context, id string) error { return nil }
func (m *MockDockerClient) ImageCreate(ctx context.Context, parentReference string, options image.CreateOptions) (io.ReadCloser, error) {
	return nil, nil
}
func (m *MockDockerClient) ImageHistory(ctx context.Context, imageID string) ([]image.HistoryResponseItem, error) {
	return nil, nil
}
func (m *MockDockerClient) ImageImport(ctx context.Context, source image.ImportSource, ref string, options image.ImportOptions) (io.ReadCloser, error) {
	return nil, nil
}
func (m *MockDockerClient) ImageInspectWithRaw(ctx context.Context, imageID string) (types.ImageInspect, []byte, error) {
	return types.ImageInspect{}, nil, nil
}
func (m *MockDockerClient) ImageList(ctx context.Context, options image.ListOptions) ([]image.Summary, error) {
	return nil, nil
}
func (m *MockDockerClient) ImageLoad(ctx context.Context, input io.Reader, quiet bool) (image.LoadResponse, error) {
	return image.LoadResponse{}, nil
}
func (m *MockDockerClient) ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error) {
	return nil, nil
}
func (m *MockDockerClient) ImagePush(ctx context.Context, ref string, options image.PushOptions) (io.ReadCloser, error) {
	return nil, nil
}
func (m *MockDockerClient) ImageRemove(ctx context.Context, imageID string, options image.RemoveOptions) ([]image.DeleteResponse, error) {
	return nil, nil
}
func (m *MockDockerClient) ImageSearch(ctx context.Context, term string, options registry.SearchOptions) ([]registry.SearchResult, error) {
	return nil, nil
}
func (m *MockDockerClient) ImageSave(ctx context.Context, images []string) (io.ReadCloser, error) {
	return nil, nil
}
func (m *MockDockerClient) ImageTag(ctx context.Context, image, ref string) error { return nil }
func (m *MockDockerClient) ImagesPrune(ctx context.Context, pruneFilter filters.Args) (image.PruneReport, error) {
	return image.PruneReport{}, nil
}
func (m *MockDockerClient) NodeInspectWithRaw(ctx context.Context, nodeID string) (swarm.Node, []byte, error) {
	return swarm.Node{}, nil, nil
}
func (m *MockDockerClient) NodeList(ctx context.Context, options types.NodeListOptions) ([]swarm.Node, error) {
	return nil, nil
}
func (m *MockDockerClient) NodeRemove(ctx context.Context, nodeID string, options types.NodeRemoveOptions) error {
	return nil
}
func (m *MockDockerClient) NodeUpdate(ctx context.Context, nodeID string, version swarm.Version, node swarm.NodeSpec) error {
	return nil
}
func (m *MockDockerClient) NetworkConnect(ctx context.Context, networkID, containerID string, config *network.EndpointSettings) error {
	return nil
}
func (m *MockDockerClient) NetworkCreate(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error) {
	return network.CreateResponse{}, nil
}
func (m *MockDockerClient) NetworkDisconnect(ctx context.Context, network, container string, force bool) error {
	return nil
}
func (m *MockDockerClient) NetworkInspect(ctx context.Context, networkID string, options network.InspectOptions) (network.Inspect, error) {
	return network.Inspect{}, nil
}
func (m *MockDockerClient) NetworkInspectWithRaw(ctx context.Context, networkID string, options network.InspectOptions) (network.Inspect, []byte, error) {
	return network.Inspect{}, nil, nil
}
func (m *MockDockerClient) NetworkList(ctx context.Context, options network.ListOptions) ([]network.Inspect, error) {
	return nil, nil
}
func (m *MockDockerClient) NetworkRemove(ctx context.Context, networkID string) error { return nil }
func (m *MockDockerClient) NetworksPrune(ctx context.Context, pruneFilter filters.Args) (network.PruneReport, error) {
	return network.PruneReport{}, nil
}
func (m *MockDockerClient) PluginList(ctx context.Context, filter filters.Args) (types.PluginsListResponse, error) {
	return types.PluginsListResponse{}, nil
}
func (m *MockDockerClient) PluginRemove(ctx context.Context, name string, options types.PluginRemoveOptions) error {
	return nil
}
func (m *MockDockerClient) PluginEnable(ctx context.Context, name string, options types.PluginEnableOptions) error {
	return nil
}
func (m *MockDockerClient) PluginDisable(ctx context.Context, name string, options types.PluginDisableOptions) error {
	return nil
}
func (m *MockDockerClient) PluginInstall(ctx context.Context, name string, options types.PluginInstallOptions) (io.ReadCloser, error) {
	return nil, nil
}
func (m *MockDockerClient) PluginUpgrade(ctx context.Context, name string, options types.PluginInstallOptions) (io.ReadCloser, error) {
	return nil, nil
}
func (m *MockDockerClient) PluginPush(ctx context.Context, name string, registryAuth string) (io.ReadCloser, error) {
	return nil, nil
}
func (m *MockDockerClient) PluginSet(ctx context.Context, name string, args []string) error {
	return nil
}
func (m *MockDockerClient) PluginInspectWithRaw(ctx context.Context, name string) (*types.Plugin, []byte, error) {
	return nil, nil, nil
}
func (m *MockDockerClient) PluginCreate(ctx context.Context, createContext io.Reader, options types.PluginCreateOptions) error {
	return nil
}
func (m *MockDockerClient) ServiceCreate(ctx context.Context, service swarm.ServiceSpec, options types.ServiceCreateOptions) (swarm.ServiceCreateResponse, error) {
	return swarm.ServiceCreateResponse{}, nil
}
func (m *MockDockerClient) ServiceInspectWithRaw(ctx context.Context, serviceID string, options types.ServiceInspectOptions) (swarm.Service, []byte, error) {
	return swarm.Service{}, nil, nil
}
func (m *MockDockerClient) ServiceList(ctx context.Context, options types.ServiceListOptions) ([]swarm.Service, error) {
	return nil, nil
}
func (m *MockDockerClient) ServiceRemove(ctx context.Context, serviceID string) error { return nil }
func (m *MockDockerClient) ServiceUpdate(ctx context.Context, serviceID string, version swarm.Version, service swarm.ServiceSpec, options types.ServiceUpdateOptions) (swarm.ServiceUpdateResponse, error) {
	return swarm.ServiceUpdateResponse{}, nil
}
func (m *MockDockerClient) ServiceLogs(ctx context.Context, serviceID string, options container.LogsOptions) (io.ReadCloser, error) {
	return nil, nil
}
func (m *MockDockerClient) TaskLogs(ctx context.Context, taskID string, options container.LogsOptions) (io.ReadCloser, error) {
	return nil, nil
}
func (m *MockDockerClient) TaskInspectWithRaw(ctx context.Context, taskID string) (swarm.Task, []byte, error) {
	return swarm.Task{}, nil, nil
}
func (m *MockDockerClient) TaskList(ctx context.Context, options types.TaskListOptions) ([]swarm.Task, error) {
	return nil, nil
}
func (m *MockDockerClient) SwarmInit(ctx context.Context, req swarm.InitRequest) (string, error) {
	return "", nil
}
func (m *MockDockerClient) SwarmJoin(ctx context.Context, req swarm.JoinRequest) error { return nil }
func (m *MockDockerClient) SwarmGetUnlockKey(ctx context.Context) (types.SwarmUnlockKeyResponse, error) {
	return types.SwarmUnlockKeyResponse{}, nil
}
func (m *MockDockerClient) SwarmUnlock(ctx context.Context, req swarm.UnlockRequest) error {
	return nil
}
func (m *MockDockerClient) SwarmLeave(ctx context.Context, force bool) error { return nil }
func (m *MockDockerClient) SwarmInspect(ctx context.Context) (swarm.Swarm, error) {
	return swarm.Swarm{}, nil
}
func (m *MockDockerClient) SwarmUpdate(ctx context.Context, version swarm.Version, swarm swarm.Spec, flags swarm.UpdateFlags) error {
	return nil
}
func (m *MockDockerClient) SecretList(ctx context.Context, options types.SecretListOptions) ([]swarm.Secret, error) {
	return nil, nil
}
func (m *MockDockerClient) SecretCreate(ctx context.Context, secret swarm.SecretSpec) (types.SecretCreateResponse, error) {
	return types.SecretCreateResponse{}, nil
}
func (m *MockDockerClient) SecretRemove(ctx context.Context, id string) error { return nil }
func (m *MockDockerClient) SecretInspectWithRaw(ctx context.Context, name string) (swarm.Secret, []byte, error) {
	return swarm.Secret{}, nil, nil
}
func (m *MockDockerClient) SecretUpdate(ctx context.Context, id string, version swarm.Version, secret swarm.SecretSpec) error {
	return nil
}
func (m *MockDockerClient) Events(ctx context.Context, options events.ListOptions) (<-chan events.Message, <-chan error) {
	return nil, nil
}
func (m *MockDockerClient) Info(ctx context.Context) (system.Info, error) { return system.Info{}, nil }
func (m *MockDockerClient) RegistryLogin(ctx context.Context, auth registry.AuthConfig) (registry.AuthenticateOKBody, error) {
	return registry.AuthenticateOKBody{}, nil
}
func (m *MockDockerClient) DiskUsage(ctx context.Context, options types.DiskUsageOptions) (types.DiskUsage, error) {
	return types.DiskUsage{}, nil
}
func (m *MockDockerClient) Ping(ctx context.Context) (types.Ping, error) { return types.Ping{}, nil }
func (m *MockDockerClient) VolumeInspect(ctx context.Context, volumeID string) (volume.Volume, error) {
	return volume.Volume{}, nil
}
func (m *MockDockerClient) VolumeInspectWithRaw(ctx context.Context, volumeID string) (volume.Volume, []byte, error) {
	return volume.Volume{}, nil, nil
}
func (m *MockDockerClient) VolumesPrune(ctx context.Context, pruneFilter filters.Args) (volume.PruneReport, error) {
	return volume.PruneReport{}, nil
}
func (m *MockDockerClient) VolumeUpdate(ctx context.Context, volumeID string, version swarm.Version, options volume.UpdateOptions) error {
	return nil
}
func (m *MockDockerClient) ConfigList(ctx context.Context, options types.ConfigListOptions) ([]swarm.Config, error) {
	return nil, nil
}
func (m *MockDockerClient) ConfigCreate(ctx context.Context, config swarm.ConfigSpec) (types.ConfigCreateResponse, error) {
	return types.ConfigCreateResponse{}, nil
}
func (m *MockDockerClient) ConfigRemove(ctx context.Context, id string) error { return nil }
func (m *MockDockerClient) ConfigInspectWithRaw(ctx context.Context, name string) (swarm.Config, []byte, error) {
	return swarm.Config{}, nil, nil
}
func (m *MockDockerClient) ConfigUpdate(ctx context.Context, id string, version swarm.Version, config swarm.ConfigSpec) error {
	return nil
}
func (m *MockDockerClient) ContainersPruneReport(ctx context.Context, cfg filters.Args) (container.PruneReport, error) {
	return container.PruneReport{}, nil
}
func (m *MockDockerClient) VolumesPruneReport(ctx context.Context, cfg filters.Args) (volume.PruneReport, error) {
	return volume.PruneReport{}, nil
}
func (m *MockDockerClient) ImagesPruneReport(ctx context.Context, cfg filters.Args) (image.PruneReport, error) {
	return image.PruneReport{}, nil
}
func (m *MockDockerClient) NetworksPruneReport(ctx context.Context, cfg filters.Args) (network.PruneReport, error) {
	return network.PruneReport{}, nil
}
func (m *MockDockerClient) BuildCachePruneReport(ctx context.Context, cfg types.BuildCachePruneOptions) (types.BuildCachePruneReport, error) {
	return types.BuildCachePruneReport{}, nil
}

// TestCreateVolumeVolumeInUse replicates the exact error scenario from the user:
//
//	ERROR: "Error response from daemon: remove keploy-sockets-vol: volume is in use - [container-id1, container-id2]"
//
// This test:
// 1. Calls the ACTUAL CreateVolume() function from pkg/platform/docker/docker.go (line 566)
// 2. Creates real Docker containers using the volume (simulating orphaned containers)
// 3. Demonstrates the error would occur (volume in use)
// 4. Shows the fix automatically removes blocking containers and recreates the volume
//
// Run with: go test -v -run TestCreateVolumeVolumeInUse ./pkg/platform/docker/
func TestCreateVolumeVolumeInUse(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	logger, _ := zap.NewDevelopment()

	// Create a real Docker client using the actual New() function
	client, err := New(logger)
	if err != nil {
		t.Skipf("Docker not available: %v", err)
	}

	ctx := context.Background()
	testVolName := "keploy-test-recreate-in-use"
	container1Name := "keploy-test-container-1"
	container2Name := "keploy-test-container-2"

	// Cleanup function
	defer func() {
		t.Logf("→ Cleaning up test containers and volume...")
		_ = client.ContainerRemove(ctx, container1Name, container.RemoveOptions{Force: true})
		_ = client.ContainerRemove(ctx, container2Name, container.RemoveOptions{Force: true})
		_ = client.VolumeRemove(ctx, testVolName, true)
		t.Logf("✓ Cleanup complete")
	}()

	// Step 1: Create initial volume with no special driver options (like keploy-sockets-vol)
	t.Logf("Step 1: Creating volume '%s' with default options...", testVolName)
	err = client.(*Impl).CreateVolume(ctx, testVolName, false, nil)
	if err != nil {
		t.Fatalf("Failed to create initial volume: %v", err)
	}
	t.Logf("✓ Volume created successfully")

	// Step 2: Pull busybox image
	t.Logf("Step 2: Pulling busybox image...")
	pullReader, err := client.ImagePull(ctx, "busybox:latest", image.PullOptions{})
	if err != nil {
		t.Skipf("Cannot pull image (Docker registry might be unavailable): %v", err)
	}
	_, _ = io.ReadAll(pullReader)
	pullReader.Close()
	t.Logf("✓ Image ready")

	// Step 3: Create TWO containers using the volume (simulating the user's scenario)
	t.Logf("Step 3: Creating two containers that use the volume...")

	containerConfig1 := &container.Config{
		Image: "busybox:latest",
		Cmd:   []string{"sleep", "300"},
	}
	hostConfig1 := &container.HostConfig{
		Binds: []string{testVolName + ":/tmp"},
	}
	resp1, err := client.ContainerCreate(ctx, containerConfig1, hostConfig1, nil, nil, container1Name)
	if err != nil {
		t.Fatalf("Failed to create container 1: %v", err)
	}
	err = client.ContainerStart(ctx, resp1.ID, container.StartOptions{})
	if err != nil {
		t.Fatalf("Failed to start container 1: %v", err)
	}
	t.Logf("✓ Container 1 started: %s", resp1.ID[:12])

	containerConfig2 := &container.Config{
		Image: "busybox:latest",
		Cmd:   []string{"sleep", "300"},
	}
	hostConfig2 := &container.HostConfig{
		Binds: []string{testVolName + ":/tmp"},
	}
	resp2, err := client.ContainerCreate(ctx, containerConfig2, hostConfig2, nil, nil, container2Name)
	if err != nil {
		t.Fatalf("Failed to create container 2: %v", err)
	}
	err = client.ContainerStart(ctx, resp2.ID, container.StartOptions{})
	if err != nil {
		t.Fatalf("Failed to start container 2: %v", err)
	}
	t.Logf("✓ Container 2 started: %s", resp2.ID[:12])
	t.Logf("✓ Volume is now IN USE by 2 containers")

	// Step 4: Try to recreate the volume with different options while it's in use
	// This calls the ACTUAL CreateVolume function from pkg/platform/docker/docker.go:566
	// With the fix, it should automatically remove the containers and recreate the volume
	t.Logf("Step 4: Attempting to recreate volume with different options (recreate=true)...")
	t.Logf("   Expected behavior: Automatically remove containers and recreate volume")
	differentOpts := map[string]string{
		"type":   "tmpfs",
		"device": "tmpfs",
	}

	// With the fix, this should succeed by automatically removing the containers
	err = client.(*Impl).CreateVolume(ctx, testVolName, true, differentOpts)

	// Verify the operation succeeded
	if err != nil {
		t.Fatalf("Expected successful volume recreation, but got error: %v", err)
	}

	// Verify the volume now exists with the new options
	filter := filters.NewArgs()
	filter.Add("name", testVolName)
	volumeList, err := client.VolumeList(ctx, volume.ListOptions{Filters: filter})
	if err != nil {
		t.Fatalf("Failed to list volumes: %v", err)
	}
	if len(volumeList.Volumes) == 0 {
		t.Fatalf("Volume was not created")
	}

	// Verify the new volume has the correct options
	newVolume := volumeList.Volumes[0]
	if newVolume.Options["type"] != "tmpfs" {
		t.Errorf("Expected volume with tmpfs type, got: %v", newVolume.Options)
	}
	t.Logf("✓ Verified: Volume exists with new options: %v", newVolume.Options)
}

// TestVolumeOptionsMatch tests the volumeOptionsMatch helper function
func TestVolumeOptionsMatch(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	impl := &Impl{logger: logger}

	tests := []struct {
		name        string
		existing    map[string]string
		desired     map[string]string
		shouldMatch bool
	}{
		{
			name:        "Both empty",
			existing:    map[string]string{},
			desired:     map[string]string{},
			shouldMatch: true,
		},
		{
			name:        "Both nil",
			existing:    nil,
			desired:     nil,
			shouldMatch: true,
		},
		{
			name:        "One empty, one nil",
			existing:    map[string]string{},
			desired:     nil,
			shouldMatch: true,
		},
		{
			name: "Matching options",
			existing: map[string]string{
				"type":   "tmpfs",
				"device": "tmpfs",
			},
			desired: map[string]string{
				"type":   "tmpfs",
				"device": "tmpfs",
			},
			shouldMatch: true,
		},
		{
			name: "Different values",
			existing: map[string]string{
				"type":   "nfs",
				"device": "nfs-server",
			},
			desired: map[string]string{
				"type":   "tmpfs",
				"device": "tmpfs",
			},
			shouldMatch: false,
		},
		{
			name: "Different keys",
			existing: map[string]string{
				"type": "tmpfs",
			},
			desired: map[string]string{
				"device": "tmpfs",
			},
			shouldMatch: false,
		},
		{
			name: "Different lengths",
			existing: map[string]string{
				"type":   "tmpfs",
				"device": "tmpfs",
			},
			desired: map[string]string{
				"type": "tmpfs",
			},
			shouldMatch: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := impl.volumeOptionsMatch(tt.existing, tt.desired)
			assert.Equal(t, tt.shouldMatch, result)
		})
	}
}
