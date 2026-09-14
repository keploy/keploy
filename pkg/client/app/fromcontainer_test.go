package app

import (
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/go-connections/nat"
	"go.keploy.io/server/v3/pkg/platform/docker"
)

// A container sharing another's network namespace may not declare any of these,
// and the daemon rejects the create outright (runconfig/hostconfig.go) rather
// than ignoring them. An inspect result always carries several of them —
// Hostname is filled with the container's short id even when the user never set
// one, and ExposedPorts is merged with the image's EXPOSE set — so copying an
// inspect verbatim produces a create that cannot succeed.
func TestReplicaConfigDropsWhatASharedNamespaceRefuses(t *testing.T) {
	src := container.InspectResponse{
		Config: &container.Config{
			Hostname:     "payments-api",
			Domainname:   "svc.local",
			ExposedPorts: nat.PortSet{"9410/tcp": struct{}{}},
			Image:        "myorg/api:1",
		},
		ContainerJSONBase: &container.ContainerJSONBase{HostConfig: &container.HostConfig{
			PortBindings:    nat.PortMap{"9410/tcp": []nat.PortBinding{{HostPort: "19410"}}},
			PublishAllPorts: true,
			DNS:             []string{"1.1.1.1"},
			DNSOptions:      []string{"ndots:1"},
			DNSSearch:       []string{"svc.local"},
			ExtraHosts:      []string{"db:10.0.0.5"},
			Links:           []string{"/db:db"},
		}},
	}

	cfg, hostCfg := replicaConfig(src, "keploy-v3-abcd")

	for _, tc := range []struct {
		field string
		got   interface{}
	}{
		{"Config.Hostname", cfg.Hostname},
		{"Config.Domainname", cfg.Domainname},
		{"Config.ExposedPorts", len(cfg.ExposedPorts)},
		{"HostConfig.PortBindings", len(hostCfg.PortBindings)},
		{"HostConfig.PublishAllPorts", hostCfg.PublishAllPorts},
		{"HostConfig.DNS", len(hostCfg.DNS)},
		{"HostConfig.DNSOptions", len(hostCfg.DNSOptions)},
		{"HostConfig.DNSSearch", len(hostCfg.DNSSearch)},
		{"HostConfig.ExtraHosts", len(hostCfg.ExtraHosts)},
		{"HostConfig.Links", len(hostCfg.Links)},
	} {
		switch v := tc.got.(type) {
		case string:
			if v != "" {
				t.Errorf("%s = %q, want empty: the daemon refuses it under a container: network mode", tc.field, v)
			}
		case int:
			if v != 0 {
				t.Errorf("%s has %d entries, want none: the daemon refuses them under a container: network mode", tc.field, v)
			}
		case bool:
			if v {
				t.Errorf("%s = true, want false: the daemon refuses it under a container: network mode", tc.field)
			}
		}
	}

	if want := container.NetworkMode("container:keploy-v3-abcd"); hostCfg.NetworkMode != want {
		t.Errorf("NetworkMode = %q, want %q", hostCfg.NetworkMode, want)
	}
	if want := container.PidMode("container:keploy-v3-abcd"); hostCfg.PidMode != want {
		t.Errorf("PidMode = %q, want %q", hostCfg.PidMode, want)
	}
	// The source's image has to survive, or the copy runs something else.
	if cfg.Image != "myorg/api:1" {
		t.Errorf("Image = %q, want the source's", cfg.Image)
	}
}

// Everything that is NOT network-scoped is the reason this path exists: each of
// these is lost by the docker-run string the command path reconstructs, because
// that string only carries flags someone remembered to emit.
func TestReplicaConfigKeepsEverythingElse(t *testing.T) {
	src := container.InspectResponse{
		Config: &container.Config{
			Labels:      map[string]string{"com.example.team": "payments", "com.docker.compose.project": "checkout"},
			Env:         []string{"PLAIN=1"},
			Cmd:         []string{"python", "app.py"},
			Entrypoint:  []string{"/tini", "--"},
			WorkingDir:  "/w",
			User:        "1000:1000",
			StopSignal:  "SIGUSR1",
			Healthcheck: &container.HealthConfig{Test: []string{"CMD-SHELL", "exit 0"}},
		},
		ContainerJSONBase: &container.ContainerJSONBase{HostConfig: &container.HostConfig{
			Resources:     container.Resources{Memory: 268435456, CPUShares: 512},
			CapAdd:        []string{"NET_ADMIN"},
			Tmpfs:         map[string]string{"/scratch": ""},
			Sysctls:       map[string]string{"kernel.shm_rmid_forced": "1"},
			Privileged:    true,
			ShmSize:       67108864,
			RestartPolicy: container.RestartPolicy{Name: "always"},
			LogConfig:     container.LogConfig{Type: "fluentd"},
		}},
	}

	cfg, hostCfg := replicaConfig(src, "agent")

	if cfg.Labels["com.example.team"] != "payments" || cfg.Labels["com.docker.compose.project"] != "checkout" {
		t.Errorf("labels lost: %v", cfg.Labels)
	}
	if cfg.StopSignal != "SIGUSR1" {
		t.Errorf("StopSignal = %q, want SIGUSR1", cfg.StopSignal)
	}
	if cfg.Healthcheck == nil || len(cfg.Healthcheck.Test) != 2 {
		t.Errorf("Healthcheck lost: %v", cfg.Healthcheck)
	}
	if cfg.User != "1000:1000" || cfg.WorkingDir != "/w" {
		t.Errorf("User/WorkingDir lost: %q %q", cfg.User, cfg.WorkingDir)
	}
	if len(cfg.Entrypoint) != 2 || len(cfg.Cmd) != 2 {
		t.Errorf("Entrypoint/Cmd lost: %v %v", cfg.Entrypoint, cfg.Cmd)
	}
	if hostCfg.Memory != 268435456 || hostCfg.CPUShares != 512 {
		t.Errorf("resource limits lost: mem=%d cpu=%d", hostCfg.Memory, hostCfg.CPUShares)
	}
	if len(hostCfg.CapAdd) != 1 || hostCfg.CapAdd[0] != "NET_ADMIN" {
		t.Errorf("CapAdd lost: %v", hostCfg.CapAdd)
	}
	if hostCfg.Tmpfs["/scratch"] != "" {
		t.Errorf("Tmpfs lost: %v", hostCfg.Tmpfs)
	}
	if !hostCfg.Privileged || hostCfg.ShmSize != 67108864 {
		t.Errorf("Privileged/ShmSize lost: %v %d", hostCfg.Privileged, hostCfg.ShmSize)
	}
	// A restart policy is deliberately NOT carried: it would fight keploy for
	// the replacement's lifecycle and restart the app after the session ends.
	// The user's own container keeps its policy, because it is never re-created.
	if hostCfg.RestartPolicy.Name != "" {
		t.Errorf("RestartPolicy = %q, want cleared", hostCfg.RestartPolicy.Name)
	}
}

// replicaConfig has to CALL mountsFrom, not merely have it available: dropping
// the call leaves mountsFrom perfectly tested in isolation while the app comes
// up with brand-new empty anonymous volumes.
func TestReplicaConfigReconstructsMountsAndForcesAReadableLogDriver(t *testing.T) {
	src := container.InspectResponse{
		Config: &container.Config{},
		ContainerJSONBase: &container.ContainerJSONBase{
			Image:      "sha256:deadbeef",
			HostConfig: &container.HostConfig{LogConfig: container.LogConfig{Type: "syslog"}},
		},
		Mounts: []container.MountPoint{
			{Type: mount.TypeVolume, Name: "pgdata", Destination: "/var/lib/postgresql/data", RW: true},
		},
	}

	cfg, hostCfg := replicaConfig(src, "agent")

	var found bool
	for _, m := range hostCfg.Mounts {
		if m.Target == "/var/lib/postgresql/data" && m.Source == "pgdata" {
			found = true
		}
	}
	if !found {
		t.Error("the container's data volume was not carried onto the replacement; it would start with an empty data directory")
	}
	// A driver the daemon refuses to read costs the user every line the app
	// prints, and the replacement is keploy's container to configure.
	if hostCfg.LogConfig.Type != "json-file" {
		t.Errorf("LogConfig.Type = %q, want json-file: an unreadable driver loses all app output", hostCfg.LogConfig.Type)
	}
	// The reference the container was created with can have moved since; the
	// resolved id is what it is actually running.
	if cfg.Image != "sha256:deadbeef" {
		t.Errorf("Image = %q, want the resolved id", cfg.Image)
	}
	if cfg.Labels[replacementLabel] != "true" {
		t.Error("the replacement is not labelled, so teardown cannot tell it from a container the user owns")
	}
	// withTLSEnv has to be WIRED, not just correct in isolation: without it the
	// CA bundle is mounted but nothing trusts it.
	var sawCert bool
	for _, e := range cfg.Env {
		if e == "SSL_CERT_FILE="+docker.KeployTLSMountPath+"/ca.crt" {
			sawCert = true
		}
	}
	if !sawCert {
		t.Error("the CA-bundle environment was not applied; every HTTPS dependency would fail to verify")
	}
}

// JAVA_TOOL_OPTIONS is a list of JVM flags, not a single value, and it is
// routinely already set. Treating it like the others leaves the JVM on its
// stock truststore for most real Java containers.
func TestWithTLSEnvAppendsToAnExistingJavaToolOptions(t *testing.T) {
	got := withTLSEnv([]string{"JAVA_TOOL_OPTIONS=-Xmx1g -javaagent:/apm.jar"})
	var line string
	for _, e := range got {
		if len(e) > len(javaToolOptions) && e[:len(javaToolOptions)] == javaToolOptions {
			line = e
		}
	}
	if !strings.Contains(line, "-Xmx1g") || !strings.Contains(line, "/apm.jar") {
		t.Errorf("clobbered the container's own JVM flags: %q", line)
	}
	if !strings.Contains(line, keployTrustStoreFlag) {
		t.Errorf("keploy's truststore was not added: %q", line)
	}
	// Applying it twice must not stack duplicates.
	if again := withTLSEnv(got); len(again) != len(got) {
		t.Errorf("re-applying added %d more entries", len(again)-len(got))
	}
}

// net.* sysctls are network-namespace scoped, so under a shared namespace they
// belong to the agent; everything else is the container's own.
func TestNonNetSysctls(t *testing.T) {
	got := nonNetSysctls(map[string]string{
		"net.ipv4.tcp_keepalive_time": "321",
		"net.core.somaxconn":          "1024",
		"kernel.shm_rmid_forced":      "1",
	})
	if len(got) != 1 || got["kernel.shm_rmid_forced"] != "1" {
		t.Fatalf("kept %v, want only the kernel.* entry", got)
	}
	if got := nonNetSysctls(map[string]string{"net.core.somaxconn": "1"}); got != nil {
		t.Errorf("an all-net map should collapse to nil, got %v", got)
	}
}

// The names of anonymous volumes exist ONLY in InspectResponse.Mounts. Building
// the create from Config.Volumes instead provisions brand new EMPTY volumes and
// orphans the data the application is actually using — silently.
func TestMountsFromCarriesAnonymousVolumesByName(t *testing.T) {
	src := container.InspectResponse{
		Mounts: []container.MountPoint{
			{Type: mount.TypeVolume, Name: "a1b2c3deadbeef", Source: "/var/lib/docker/volumes/a1b2c3deadbeef/_data", Destination: "/var/lib/postgresql/data", RW: true},
			{Type: mount.TypeBind, Source: "/home/me/src", Destination: "/w", RW: true},
			{Type: mount.TypeVolume, Name: "named-vol", Destination: "/cache", RW: false},
			{Type: mount.TypeTmpfs, Destination: "/scratch"},
		},
	}

	got := mountsFrom(src, nil)

	byTarget := map[string]mount.Mount{}
	for _, m := range got {
		byTarget[m.Target] = m
	}
	data, ok := byTarget["/var/lib/postgresql/data"]
	if !ok {
		t.Fatal("the anonymous volume was dropped; the app would come up with an empty data directory")
	}
	if data.Source != "a1b2c3deadbeef" {
		t.Errorf("anonymous volume Source = %q, want the volume NAME — the /var/lib/docker path bypasses the volume driver", data.Source)
	}
	if byTarget["/w"].Source != "/home/me/src" {
		t.Errorf("bind mount lost: %+v", byTarget["/w"])
	}
	if !byTarget["/cache"].ReadOnly {
		t.Error("a read-only volume came back writable")
	}
	// tmpfs carries its options in HostConfig.Tmpfs, which is copied verbatim;
	// emitting it as a Mount too would double-declare the destination.
	if _, ok := byTarget["/scratch"]; ok {
		t.Error("tmpfs should not be re-emitted as a Mount")
	}
}

// An explicit --mount is already authoritative and already in the right shape;
// src.Mounts must not add a second declaration for the same destination.
func TestMountsFromDoesNotDuplicateDeclaredTargets(t *testing.T) {
	declared := []mount.Mount{{Type: mount.TypeVolume, Source: "chosen", Target: "/data"}}
	src := container.InspectResponse{
		Mounts: []container.MountPoint{{Type: mount.TypeVolume, Name: "other", Destination: "/data", RW: true}},
	}
	got := mountsFrom(src, declared)
	if len(got) != 1 {
		t.Fatalf("got %d mounts for one destination: %+v", len(got), got)
	}
	if got[0].Source != "chosen" {
		t.Errorf("Source = %q, want the declared mount to win", got[0].Source)
	}
}

// The CA bundle reaches the app the same way the command path splices
// `-v keploy-tls-certs:/tmp/keploy-tls:ro` in.
func TestReplicaConfigMountsTheKeployCABundle(t *testing.T) {
	_, hostCfg := replicaConfig(container.InspectResponse{
		Config: &container.Config{}, ContainerJSONBase: &container.ContainerJSONBase{HostConfig: &container.HostConfig{}},
	}, "agent")
	for _, m := range hostCfg.Mounts {
		if m.Source == docker.KeployTLSVolumeName {
			if m.Target != docker.KeployTLSMountPath || !m.ReadOnly {
				t.Fatalf("CA bundle mounted wrong: %+v", m)
			}
			return
		}
	}
	t.Fatal("the keploy CA bundle was not mounted; HTTPS dependencies would fail to verify")
}

// The TLS env is additive: a container that already sets one of these names
// chose that value, and overwriting it would break its own trust setup.
func TestWithTLSEnvDoesNotClobber(t *testing.T) {
	got := withTLSEnv([]string{"SSL_CERT_FILE=/etc/mine.pem", "PLAIN=1"})
	var ssl, node int
	for _, e := range got {
		switch {
		case e == "SSL_CERT_FILE=/etc/mine.pem":
			ssl++
		case len(e) > 20 && e[:20] == "NODE_EXTRA_CA_CERTS=":
			node++
		case e == "SSL_CERT_FILE="+docker.KeployTLSMountPath+"/ca.crt":
			t.Error("overwrote the container's own SSL_CERT_FILE")
		}
	}
	if ssl != 1 {
		t.Errorf("the container's own SSL_CERT_FILE appears %d times, want exactly 1", ssl)
	}
	if node != 1 {
		t.Errorf("NODE_EXTRA_CA_CERTS appears %d times, want exactly 1 (added)", node)
	}
}

// MountPoint.Mode carries bind propagation and the SELinux relabel flags. A
// bind mount that loses its relabel is denied to the container outright on an
// enforcing host, and one that loses :rslave stops seeing nested mounts.
func TestMountsFromCarriesBindPropagation(t *testing.T) {
	src := container.InspectResponse{
		Mounts: []container.MountPoint{
			{Type: mount.TypeBind, Source: "/var/run", Destination: "/host/run", RW: true, Mode: "rslave"},
			{Type: mount.TypeBind, Source: "/data", Destination: "/data", RW: true, Mode: "z"},
		},
	}
	byTarget := map[string]mount.Mount{}
	for _, m := range mountsFrom(src, nil) {
		byTarget[m.Target] = m
	}

	run := byTarget["/host/run"]
	if run.BindOptions == nil || run.BindOptions.Propagation != mount.PropagationRSlave {
		t.Errorf("bind propagation lost: %+v", run.BindOptions)
	}
	// A relabel-only Mode names no propagation, so there is nothing to set -
	// what matters is that it does not invent one.
	if data := byTarget["/data"]; data.BindOptions != nil {
		t.Errorf("invented a propagation from a relabel-only mode: %+v", data.BindOptions)
	}
}
