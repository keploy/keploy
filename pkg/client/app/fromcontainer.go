package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/pkg/stdcopy"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// The replacement container's name is the source's plus this suffix. It cannot
// reuse the source's name while the source still exists, and renaming the
// source would be a visible side effect on something the user did not ask us
// to touch.
//
// A derived name costs nothing here, which is the whole reason this approach
// works: under `--network=container:<agent>` the app has no network identity of
// its own at all. Its DNS name, published ports and aliases belong to the agent
// container, so nothing inside or outside the namespace ever resolves the app
// by this name. Contrast the command-string path, where the name is load-
// bearing and keploy has to destroy the original to reuse it.
const replacementSuffix = "-keploy"

// replacementLabel marks a container as keploy's disposable copy. Teardown and
// the pre-run name check both refuse to remove anything without it: a user who
// already runs something called `<app>-keploy` owns that name, and deleting it
// to make room would be exactly the destructive behaviour this path exists to
// avoid.
const replacementLabel = "io.keploy.from-container.replacement"

// Budgets for the Engine API calls around the app container. Each is a local
// daemon round trip on an object we have just created or just inspected.
const (
	inspectSourceBudget = 10 * time.Second
	// logFlushGrace bounds how long teardown waits for the app's last lines to
	// reach the terminal. The stream is a convenience; the recording is already
	// safe by this point.
	logFlushGrace       = 2 * time.Second
	createBudget        = 30 * time.Second
	removeReplaceBudget = 15 * time.Second
	// replacementStopBudget gives the app its own StopSignal and StopTimeout
	// before the remove escalates to SIGKILL.
	replacementStopBudget = 30 * time.Second
	restoreSourceBudget   = 30 * time.Second
)

// SetupFromContainer prepares a --from-container run: it inspects the container
// the user named, stops it, and creates a replacement that is identical except
// for the namespaces keploy has to own.
//
// The replacement is built from the inspect result rather than from a
// reconstructed `docker run` string, which is the entire point. A command
// string loses everything it has no flag for and everything ExtractDockerFlags'
// regex does not match; an inspect loses nothing, so labels, resource limits,
// capabilities, env, entrypoint, healthcheck, stop signal, sysctls, tmpfs and
// mounts all carry over exactly.
//
// What it does NOT carry over is network state, and that is a daemon rule
// rather than a shortcoming: a container sharing another's network namespace
// may not declare a hostname, exposed ports, port bindings, DNS settings or
// extra hosts, and the daemon rejects a create that tries. Those belong to the
// agent container, which publishes on the app's behalf.
func (a *App) SetupFromContainer(ctx context.Context) error {
	source := a.opts.FromContainer
	if source == "" {
		return errors.New("no container named: --from-container is required for this command type")
	}
	if a.keployContainer == "" {
		return errors.New("the keploy agent container is not known yet; it has to be running before the app is re-created into its namespaces")
	}

	inspectCtx, cancel := context.WithTimeout(ctx, inspectSourceBudget)
	defer cancel()
	src, err := a.docker.ContainerInspect(inspectCtx, source)
	if err != nil {
		return fmt.Errorf("failed to inspect %q: %w", source, err)
	}
	// HostConfig, Name and State reach through an embedded *ContainerJSONBase,
	// so a nil base panics rather than yielding a nil field.
	if src.ContainerJSONBase == nil || src.Config == nil || src.HostConfig == nil {
		return fmt.Errorf("docker returned an incomplete inspect for %q", source)
	}

	a.sourceContainer = strings.TrimPrefix(src.Name, "/")
	// The source was already stopped by the agent client, before the agent
	// started - it had to be, because the agent publishes the app's ports on its
	// behalf and cannot bind them while the original still holds them. All that
	// is left here is remembering whether to start it again.
	a.sourceTTY = src.Config.Tty
	a.container = a.sourceContainer + replacementSuffix

	cfg, hostCfg := replicaConfig(src, a.keployContainer)

	// A replacement left behind by a previous run holds the name.
	if err := a.removeStaleReplacement(ctx); err != nil {
		return err
	}

	createCtx, cancelCreate := context.WithTimeout(ctx, createBudget)
	defer cancelCreate()
	// NetworkingConfig is deliberately nil: under a container: network mode the
	// daemon short-circuits network setup entirely, so anything passed here is
	// silently ignored rather than applied. The source's networks and aliases
	// reach the run through the agent container instead.
	created, err := a.docker.ContainerCreate(createCtx, cfg, hostCfg, nil, nil, a.container)
	if err != nil {
		return fmt.Errorf("failed to create a copy of %q: %w", a.sourceContainer, err)
	}
	a.replacementID = created.ID
	for _, w := range created.Warnings {
		a.logger.Warn("docker warned while creating the replacement container", zap.String("warning", w))
	}
	a.logger.Debug("created the replacement container",
		zap.String("id", created.ID), zap.String("name", a.container))
	return nil
}

// replicaConfig turns an inspect result into a create request that runs the
// same application inside keploy's namespaces.
//
// Two reasons a field is dropped below, and they are worth telling apart.
// The daemon REFUSES Hostname, ExposedPorts, PortBindings, PublishAllPorts,
// DNS, ExtraHosts and Links under a container: network mode
// (validateNetContainerMode, runconfig/hostconfig.go) - a create carrying any
// of them fails outright. Domainname, DNSOptions and DNSSearch are accepted but
// meaningless: the namespace they would configure belongs to the agent, which
// the daemon then copies its own Domainname over anyway. Everything else is
// carried.
func replicaConfig(src container.InspectResponse, agentContainer string) (*container.Config, *container.HostConfig) {
	cfg := *src.Config

	// Hostname is rejected outright under a shared network namespace; Domainname
	// is merely pointless there, since the namespace's identity is the agent's.
	// Docker fills Hostname with the container's short id when the user never
	// set one, so this is almost never a value the user chose.
	cfg.Hostname = ""
	cfg.Domainname = ""
	// ExposedPorts on an inspect is merged with the image's EXPOSE set, and any
	// exposed port is rejected under a shared namespace. The agent publishes.
	cfg.ExposedPorts = nil
	// MacAddress on Config is deprecated in the v28 API and the client blanks
	// it anyway; leaving it set only produces a deprecation warning.
	cfg.MacAddress = ""
	// Config.Volumes is the image-merged VOLUME set. Recreating from it would
	// provision fresh EMPTY anonymous volumes and orphan the data the app is
	// actually using; the real mounts are reconstructed from src.Mounts below.
	cfg.Volumes = nil
	// Copied, not mutated in place: `cfg := *src.Config` is shallow, so writing
	// through this map would edit the caller's inspect response.
	labels := make(map[string]string, len(cfg.Labels)+1)
	for k, v := range cfg.Labels {
		labels[k] = v
	}
	labels[replacementLabel] = "true"
	cfg.Labels = labels
	// The resolved image ID, not Config.Image - that is the reference the
	// container was created with ("app:latest"), and the whole point of
	// recording against a LIVE container is to record what it is actually
	// running. A rebuild between `docker run` and `keploy mock record` moves
	// the tag; the id is what the container is on.
	if src.Image != "" {
		cfg.Image = src.Image
	}
	cfg.Env = withTLSEnv(cfg.Env)

	hostCfg := *src.HostConfig
	hostCfg.PidMode = container.PidMode("container:" + agentContainer)
	hostCfg.NetworkMode = container.NetworkMode("container:" + agentContainer)
	// Network-scoped settings. The bindings, DNS servers and extra hosts are
	// refused outright; the resolver options and search list are accepted but
	// belong to the agent's namespace, so they are dropped rather than
	// half-applied.
	hostCfg.PortBindings = nil
	hostCfg.PublishAllPorts = false
	hostCfg.DNS = nil
	hostCfg.DNSOptions = nil
	hostCfg.DNSSearch = nil
	hostCfg.ExtraHosts = nil
	// Inspect rebuilds Links as "/child:alias" rather than returning what was
	// asked for, so it does not round-trip - and links are refused under a
	// shared namespace regardless.
	hostCfg.Links = nil
	// A restart policy would fight keploy for control of the app's lifecycle:
	// the replacement is meant to exit when the recording ends.
	hostCfg.RestartPolicy = container.RestartPolicy{}
	// Only net.* sysctls are namespace-scoped; keeping them would have the
	// replacement try to write settings that belong to the agent's namespace.
	hostCfg.Sysctls = nonNetSysctls(hostCfg.Sysctls)
	hostCfg.Mounts = MountsFrom(src, hostCfg.Mounts)
	// Binds and VolumesFrom are both superseded by the reconstructed Mounts;
	// leaving them would double-declare the same destinations.
	hostCfg.Binds = nil
	hostCfg.VolumesFrom = nil
	// The keploy CA bundle, mounted read-only exactly as the command-string
	// path splices `-v keploy-tls-certs:/tmp/keploy-tls:ro`.
	hostCfg.Mounts = append(hostCfg.Mounts, mount.Mount{
		Type:     mount.TypeVolume,
		Source:   docker.KeployTLSVolumeName,
		Target:   docker.KeployTLSMountPath,
		ReadOnly: true,
	})
	// The replacement is keploy's container, and keploy has to be able to read
	// its output. Inheriting a driver the daemon refuses to read from - none,
	// syslog, fluentd, gelf, awslogs, splunk - would cost the user every line
	// the app prints, which is the one thing an attached `docker run` gave them.
	// The source keeps its own driver; it is never re-created.
	hostCfg.LogConfig = container.LogConfig{Type: "json-file"}
	// AutoRemove is keploy's to decide: the replacement is disposable, but it
	// is removed explicitly on teardown so its exit code can be read first.
	hostCfg.AutoRemove = false

	return &cfg, &hostCfg
}

// MountsFrom reconstructs the container's real mounts from the inspect result.
//
// src.Mounts is the only place the NAMES of anonymous volumes appear - neither
// Config.Volumes nor HostConfig.Binds carries them - so a create built without
// it silently hands the app brand new empty volumes and orphans its data.
//
// Exported because the teardown path re-creates the user's own container and
// needs exactly this: two implementations of it would drift, and the symptom of
// the drift is a user's data quietly detached from their app.
func MountsFrom(src container.InspectResponse, declared []mount.Mount) []mount.Mount {
	// src.Mounts is the only place the NAMES of anonymous volumes appear, so it
	// is indexed first: a declared mount is authoritative about SHAPE, but an
	// anonymous volume is declared with no source at all (compose writes
	// `- /data` as {Type: volume, Target: /data}), and keeping that verbatim
	// hands the container a brand new empty volume and orphans its data.
	byTarget := make(map[string]container.MountPoint, len(src.Mounts))
	for _, mp := range src.Mounts {
		byTarget[mp.Destination] = mp
	}

	seen := make(map[string]bool, len(declared))
	out := make([]mount.Mount, 0, len(declared)+len(src.Mounts))
	for _, m := range declared {
		seen[m.Target] = true
		if m.Type == mount.TypeVolume && m.Source == "" {
			if mp, ok := byTarget[m.Target]; ok {
				m.Source = mp.Name
			}
		}
		out = append(out, m)
	}
	for _, mp := range src.Mounts {
		if seen[mp.Destination] {
			continue
		}
		m := mount.Mount{
			Type:     mp.Type,
			Target:   mp.Destination,
			ReadOnly: !mp.RW,
		}
		// Mode carries bind propagation (:rslave) and the SELinux relabel flags
		// (:z, :Z). On an enforcing host a bind mount without its relabel is
		// denied to the container outright.
		if mp.Mode != "" {
			switch mp.Type {
			case mount.TypeBind:
				m.BindOptions = bindOptions(mp.Mode)
			case mount.TypeVolume:
				m.VolumeOptions = &mount.VolumeOptions{NoCopy: strings.Contains(mp.Mode, "nocopy")}
			}
		}
		switch mp.Type {
		case mount.TypeVolume:
			// Name, not Source: Source is the path inside
			// /var/lib/docker/volumes and binding it would bypass the volume
			// driver entirely.
			m.Source = mp.Name
		case mount.TypeTmpfs:
			// tmpfs has no source; size and mode live in HostConfig.Tmpfs,
			// which is copied verbatim.
			continue
		default:
			m.Source = mp.Source
		}
		out = append(out, m)
	}
	return out
}

// bindOptions translates the propagation part of a mount Mode string into the
// structured form ContainerCreate takes. Anything it does not recognise is left
// to the daemon's default rather than guessed at.
func bindOptions(mode string) *mount.BindOptions {
	for _, p := range []mount.Propagation{
		mount.PropagationRShared, mount.PropagationRSlave, mount.PropagationRPrivate,
		mount.PropagationShared, mount.PropagationSlave, mount.PropagationPrivate,
	} {
		for _, part := range strings.Split(mode, ",") {
			if part == string(p) {
				return &mount.BindOptions{Propagation: p}
			}
		}
	}
	return nil
}

// nonNetSysctls drops the net.* sysctls, which are network-namespace scoped and
// therefore the agent container's to set, and keeps everything else.
func nonNetSysctls(in map[string]string) map[string]string {
	if len(in) == 0 {
		return in
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if strings.HasPrefix(k, "net.") {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// withTLSEnv adds the CA-bundle environment the command-string path splices in
// as `-e` flags, without clobbering a value the container already sets.
func withTLSEnv(env []string) []string {
	certPath := docker.KeployTLSMountPath + "/ca.crt"
	want := [][2]string{
		{"NODE_EXTRA_CA_CERTS", certPath},
		{"REQUESTS_CA_BUNDLE", certPath},
		{"SSL_CERT_FILE", certPath},
		{"CARGO_HTTP_CAINFO", certPath},
		{javaToolOptions, javaTLSOptions()},
	}
	existing := make(map[string]string, len(env))
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok {
			existing[k] = v
		}
	}
	out := make([]string, 0, len(env)+len(want))
	for _, e := range env {
		// JAVA_TOOL_OPTIONS is a list of JVM flags, not a single value, and it
		// is routinely already set - a heap cap, an APM javaagent. Skipping it
		// because it is present would leave the JVM on its stock truststore and
		// every HTTPS dependency would fail to verify against keploy's CA. It
		// is the one name here that has to be APPENDED to rather than left
		// alone, and appending wins because later JVM flags take precedence.
		if k, v, ok := strings.Cut(e, "="); ok && k == javaToolOptions && !strings.Contains(v, keployTrustStoreFlag) {
			out = append(out, k+"="+v+" "+javaTLSOptions())
			continue
		}
		out = append(out, e)
	}
	for _, kv := range want {
		if _, ok := existing[kv[0]]; ok {
			continue
		}
		out = append(out, kv[0]+"="+kv[1])
	}
	return out
}

const (
	javaToolOptions      = "JAVA_TOOL_OPTIONS"
	keployTrustStoreFlag = "-Djavax.net.ssl.trustStore="
)

// javaTLSOptions is the JVM flag pair that points at keploy's truststore.
func javaTLSOptions() string {
	return fmt.Sprintf("%s%s/truststore.jks -Djavax.net.ssl.trustStorePassword=changeit",
		keployTrustStoreFlag, docker.KeployTLSMountPath)
}

// runFromContainer starts the replacement, streams its output, and blocks until
// it exits - the Engine API equivalent of shelling out `docker run` and waiting
// on the process.
//
// ContainerWait is subscribed BEFORE ContainerStart on purpose: a container
// that exits immediately would otherwise be gone before the wait is armed, and
// the run would hang on a result that has already happened.
func (a *App) runFromContainer(ctx context.Context) models.AppError {
	if a.replacementID == "" {
		return models.AppError{AppErrorType: models.ErrInternal, Err: errors.New("no replacement container was created")}
	}

	// NextExit, not NotRunning. A created-but-unstarted container is already
	// "not running", so that condition is met before ContainerStart is even
	// issued: the daemon answers straight away with exit code 0 and every run
	// reports success. NextExit is the condition the SDK documents for
	// subscribing ahead of a start, and it is what makes the pre-start
	// subscription meaningful - a container that exits immediately cannot slip
	// between the start and the wait.
	waitCh, waitErrCh := a.docker.ContainerWait(ctx, a.replacementID, container.WaitConditionNextExit)

	if err := a.docker.ContainerStart(ctx, a.replacementID, container.StartOptions{}); err != nil {
		return models.AppError{AppErrorType: models.ErrCommandError, Err: fmt.Errorf("failed to start the replacement container: %w", err)}
	}
	a.logger.Info("your app is running under keploy", zap.String("container", a.container))

	streamDone := a.streamContainerLogs(ctx)
	// The stream is for the user's benefit only. Waiting for it UNBOUNDED would
	// hand the app's lifetime to the log driver: one that cannot be read ends
	// the stream immediately, and one that stalls never ends it at all.
	flush := func() {
		select {
		case <-streamDone:
		case <-time.After(logFlushGrace):
		}
	}

	select {
	case err := <-waitErrCh:
		if ctx.Err() != nil {
			flush()
			return models.AppError{AppErrorType: models.ErrCtxCanceled}
		}
		flush()
		return models.AppError{AppErrorType: models.ErrUnExpected, Err: fmt.Errorf("failed while waiting for the app container: %w", err)}
	case done := <-waitCh:
		flush()
		if done.Error != nil && done.Error.Message != "" {
			return models.AppError{AppErrorType: models.ErrUnExpected, Err: errors.New(done.Error.Message), AppLogs: a.recentAppLogs(ctx)}
		}
		code := int(done.StatusCode)
		if code == 0 {
			return models.AppError{AppErrorType: models.ErrAppStopped, ExitCode: 0}
		}
		return models.AppError{
			AppErrorType: models.ErrUnExpected,
			ExitCode:     code,
			Err:          fmt.Errorf("the app container exited with code %d", code),
			AppLogs:      a.recentAppLogs(ctx),
		}
	case <-ctx.Done():
		flush()
		return models.AppError{AppErrorType: models.ErrCtxCanceled}
	}
}

// streamContainerLogs mirrors the replacement's output onto keploy's own, the
// way an attached `docker run` does. Returns a channel closed when the stream
// ends, so the caller does not tear the container down mid-line.
func (a *App) streamContainerLogs(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	// Captured before the goroutine starts. When the flush grace expires this
	// outlives runFromContainer, and RemoveReplacement clears replacementID -
	// so reading either field from inside would race the caller's teardown.
	id, tty := a.replacementID, a.sourceTTY
	go func() {
		defer utils.Recover(a.logger)
		defer close(done)
		var err error
		logs, err := a.docker.ContainerLogs(ctx, id, container.LogsOptions{
			ShowStdout: true, ShowStderr: true, Follow: true,
		})
		if err != nil {
			a.logger.Debug("could not follow the app container's logs", zap.Error(err))
			return
		}
		defer func() {
			if err := logs.Close(); err != nil {
				a.logger.Debug("failed to close the app log stream", zap.Error(err))
			}
		}()
		// A TTY container's log stream is RAW; every other container's is
		// multiplexed with an 8-byte frame header. StdCopy only understands the
		// second kind - it has no TTY detection and reads the first byte as a
		// stream id, so on a raw stream it fails on the app's first line of
		// output (and, for a byte in 0x00-0x03, sizes an allocation from
		// app-controlled bytes).
		if tty {
			_, err = io.Copy(os.Stdout, logs)
		} else {
			_, err = stdcopy.StdCopy(os.Stdout, os.Stderr, logs)
		}
		if err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
			a.logger.Debug("the app log stream ended with an error", zap.Error(err))
		}
	}()
	return done
}

// RemoveReplacement stops and removes keploy's copy of the user's container.
//
// Stopped before removed, rather than force-removed: replicaConfig carries the
// source's StopSignal and StopTimeout onto the replacement, and a force remove
// is an immediate SIGKILL that ignores both. An app that flushes on SIGTERM
// would lose that flush on every clean Ctrl+C - the same courtesy every other
// docker kind gets from `docker run`.
//
// Idempotent, and safe to call from any teardown path.
func (a *App) RemoveReplacement() {
	if a.kind != utils.FromContainer || a.replacementID == "" {
		return
	}
	id := a.replacementID
	a.replacementID = ""

	stopCtx, cancelStop := context.WithTimeout(context.Background(), replacementStopBudget)
	if err := a.docker.ContainerStop(stopCtx, id, container.StopOptions{}); err != nil {
		a.logger.Debug("the replacement container did not stop cleanly; removing it anyway",
			zap.String("container", a.container), zap.Error(err))
	}
	cancelStop()

	removeCtx, cancel := context.WithTimeout(context.Background(), removeReplaceBudget)
	defer cancel()
	if err := a.docker.ContainerRemove(removeCtx, id, container.RemoveOptions{Force: true}); err != nil {
		a.logger.Warn("failed to remove keploy's replacement container; remove it by hand",
			zap.String("container", a.container), zap.Error(err))
	}
}

// removeStaleReplacement clears a replacement left behind by an earlier run.
//
// It refuses to touch a container that is not labelled as keploy's. The name it
// wants is derived from the user's own container name, so a user who already
// runs something called `<app>-keploy` owns that name - and removing it to make
// room would be precisely the destructive behaviour this whole path exists to
// avoid.
func (a *App) removeStaleReplacement(ctx context.Context) error {
	inspectCtx, cancel := context.WithTimeout(ctx, inspectSourceBudget)
	defer cancel()
	existing, err := a.docker.ContainerInspect(inspectCtx, a.container)
	if err != nil {
		return nil // nothing by that name, which is the ordinary case
	}
	if existing.Config == nil || existing.Config.Labels[replacementLabel] != "true" {
		return fmt.Errorf("a container named %q already exists and was not created by keploy; "+
			"rename or remove it, or record against a differently-named container", a.container)
	}
	a.logger.Debug("removing a replacement container left behind by an earlier run",
		zap.String("container", a.container))
	removeCtx, cancelRemove := context.WithTimeout(ctx, removeReplaceBudget)
	defer cancelRemove()
	return a.docker.ContainerRemove(removeCtx, existing.ID, container.RemoveOptions{Force: true})
}
