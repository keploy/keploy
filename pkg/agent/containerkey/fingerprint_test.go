package containerkey

import (
	"maps"
	"slices"
	"testing"

	"github.com/docker/go-connections/nat"
)

// postgresImage is an image's defaults, as postgres:16 has them.
var postgresImage = ImageConfig{
	Env:        []string{"PATH=/usr/local/bin:/usr/bin", "PG_MAJOR=16", "PG_VERSION=16.4"},
	Cmd:        []string{"postgres"},
	Entrypoint: []string{"docker-entrypoint.sh"},
	Labels:     map[string]string{"org.opencontainers.image.version": "16.4"},
}

// suitePostgres is a container of postgres:16 as a suite configures it.
func suitePostgres() Config {
	return Config{
		Env:        append([]string{"POSTGRES_DB=orders", "POSTGRES_PASSWORD=test"}, postgresImage.Env...),
		Cmd:        []string{"postgres"},
		Entrypoint: []string{"docker-entrypoint.sh"},
		Labels: map[string]string{
			"org.opencontainers.image.version":    "16.4",
			"com.docker.compose.service":          "db",
			"com.docker.compose.container-number": "1",
		},
		Image: postgresImage,
	}
}

// The fingerprint is the container's own configuration: its environment,
// command, entrypoint and labels, apart from its image's defaults, in any
// order Docker lists them.
func TestFingerprintIsTheContainersOwnConfiguration(t *testing.T) {
	base := Fingerprint(suitePostgres())
	if !isFingerprint(base) {
		t.Fatalf("Fingerprint = %q", base)
	}

	// Another version of the image, set up the same: the image's own env and
	// labels differ, the container's do not.
	newer := suitePostgres()
	newer.Image = ImageConfig{
		Env: []string{"PATH=/usr/local/bin:/usr/bin", "PG_MAJOR=17", "PG_VERSION=17.0"}, Cmd: []string{"postgres"},
		Entrypoint: []string{"docker-entrypoint.sh"}, Labels: map[string]string{"org.opencontainers.image.version": "17.0"},
	}
	newer.Env = append([]string{"POSTGRES_DB=orders", "POSTGRES_PASSWORD=test"}, newer.Image.Env...)
	newer.Labels = maps.Clone(newer.Labels)
	newer.Labels["org.opencontainers.image.version"] = "17.0"
	if got := Fingerprint(newer); got != base {
		t.Fatalf("another version of the image set up the same = %s, want %s", got, base)
	}

	// The same configuration listed in another order.
	reordered := suitePostgres()
	reordered.Env = []string{"PG_VERSION=16.4", "POSTGRES_PASSWORD=test", "PATH=/usr/local/bin:/usr/bin", "POSTGRES_DB=orders", "PG_MAJOR=16"}
	if got := Fingerprint(reordered); got != base {
		t.Fatalf("the same environment in another order = %s", got)
	}

	for what, change := range map[string]func(*Config){
		"another database":        func(c *Config) { c.Env[0] = "POSTGRES_DB=users" },
		"an image env overridden": func(c *Config) { c.Env[2] = "PATH=/opt/bin" },
		"another command":         func(c *Config) { c.Cmd = []string{"postgres", "-c", "fsync=off"} },
		"no command":              func(c *Config) { c.Cmd = nil },
		"another entrypoint":      func(c *Config) { c.Entrypoint = []string{"/bin/sh"} },
		"another compose service": func(c *Config) { c.Labels["com.docker.compose.service"] = "replica" },
		"another container number": func(c *Config) {
			c.Labels["com.docker.compose.container-number"] = "2"
		},
		"a user label": func(c *Config) { c.Labels["team"] = "payments" },
		"an image label set otherwise": func(c *Config) {
			c.Labels["org.opencontainers.image.version"] = "custom"
		},
	} {
		c := suitePostgres()
		c.Labels = maps.Clone(c.Labels)
		change(&c)
		if Fingerprint(c) == base {
			t.Errorf("%s: same fingerprint", what)
		}
	}
}

// Each label a tool gives a container that differs from run to run, or from
// machine to machine, is left out, by name; every one of them is tested.
func TestFingerprintLeavesOutRunLabels(t *testing.T) {
	base := Fingerprint(suitePostgres())
	values := map[string][2]string{
		"org.testcontainers.sessionId":                {"a1b2c3", "d4e5f6"},
		"org.testcontainers.session-id":               {"a1b2c3", "d4e5f6"},
		"org.testcontainers.resource-reaper-session":  {"8b0c-1", "9d1e-2"},
		"org.testcontainers.reap":                     {"true", ""},
		"org.testcontainers.version":                  {"1.19.0", "1.20.4"},
		"com.docker.compose.project.working_dir":      {"/home/dev/app", "/builds/ci/app"},
		"com.docker.compose.project.config_files":     {"/home/dev/app/compose.yml", "/builds/ci/app/compose.yml"},
		"com.docker.compose.project.environment_file": {"/home/dev/app/.env", "/builds/ci/app/.env"},
		"com.docker.compose.project":                  {"app", "ci-1234"},
		"com.docker.compose.image":                    {"sha256:aaa", "sha256:bbb"},
		"com.docker.compose.config-hash":              {"111", "222"},
		"com.docker.compose.version":                  {"2.29.0", "2.30.1"},
		"com.docker.compose.replace":                  {"old-id", "older-id"},
		"com.docker.compose.slug":                     {"0f3a", "77b1"},
		"com.docker.compose.image-volume-digest":      {"sha256:aaa", "sha256:bbb"},
	}
	if len(values) != len(runLabels) {
		t.Fatalf("the test covers %d run labels, the list has %d", len(values), len(runLabels))
	}
	for label := range runLabels {
		v, ok := values[label]
		if !ok {
			t.Fatalf("run label %s is not tested", label)
		}
		var got []string
		for _, value := range v {
			c := suitePostgres()
			c.Labels = maps.Clone(c.Labels)
			c.Labels[label] = value
			got = append(got, Fingerprint(c))
		}
		if got[0] != base || got[1] != base {
			t.Errorf("%s changes the fingerprint: %v, want %s", label, got, base)
		}
	}
	// Docker Desktop's labels, by their prefix: a run's, never the
	// container's.
	for _, label := range []string{"desktop.docker.io/ports.scheme", "desktop.docker.io/binds/0/Source", "desktop.docker.io/mounts/1/Source"} {
		c := suitePostgres()
		c.Labels = maps.Clone(c.Labels)
		c.Labels[label] = "/Users/dev/app/data"
		if got := Fingerprint(c); got != base {
			t.Errorf("%s changes the fingerprint", label)
		}
	}
	// Podman's environment, by name, in a configuration Podman wrote: each
	// tested. Under Docker each is the user's, and counts.
	podman := suitePostgres()
	podman.Env = append(slices.Clone(podman.Env), podmanEnv)
	podmanBase := Fingerprint(podman)
	if len(runEnv) != 3 {
		t.Fatalf("the test covers 3 runtime variables, the list has %d", len(runEnv))
	}
	for name, values := range map[string][2]string{
		"HOSTNAME": {"3f2a9c1b77aa", "9e8d7c6b5a41"},
		"HOME":     {"/root", "/home/app"},
	} {
		for _, v := range values {
			c := suitePostgres()
			c.Env = append(slices.Clone(podman.Env), name+"="+v)
			if got := Fingerprint(c); got != podmanBase {
				t.Errorf("under Podman %s=%s changes the fingerprint", name, v)
			}
			c.Env = append(slices.Clone(suitePostgres().Env), name+"="+v)
			if got := Fingerprint(c); got == base {
				t.Errorf("under Docker %s=%s, which the user set, is left out", name, v)
			}
		}
	}
	if podmanBase != base {
		t.Errorf("container=podman changes the fingerprint")
	}
	docker := suitePostgres()
	docker.Env = append(slices.Clone(docker.Env), "container=docker")
	if Fingerprint(docker) == base {
		t.Errorf("under Docker container=docker, which the user set, is left out")
	}
	// A Testcontainers label that is the user's own is not left out.
	c := suitePostgres()
	c.Labels = maps.Clone(c.Labels)
	c.Labels["org.testcontainers.lang"] = "go"
	if Fingerprint(c) == base {
		t.Fatal("org.testcontainers.lang was left out")
	}
}

// The index reads a container's configuration and its image's defaults, once
// per image, into its fingerprint; an image the daemon no longer has gives
// no defaults to leave out.
func TestIndexFingerprintsWhatItReads(t *testing.T) {
	f := newFakeDocker()
	f.images["sha256:pg16"] = postgresImage
	cfg := suitePostgres()
	withConfig := func(id string, ports nat.PortMap, image string) {
		c := running("postgres:16", image, nil, ports)
		c.Config.Env, c.Config.Cmd, c.Config.Entrypoint, c.Config.Labels = cfg.Env, cfg.Cmd, cfg.Entrypoint, cfg.Labels
		f.set(id, c)
	}
	withConfig("one", nat.PortMap{"5432/tcp": published("0.0.0.0", "49701")}, "sha256:pg16")
	withConfig("two", nat.PortMap{"5432/tcp": published("0.0.0.0", "49702")}, "sha256:pg16")
	withConfig("gone-image", nat.PortMap{"5432/tcp": published("0.0.0.0", "49703")}, "sha256:removed")
	r, _ := watching(t, f)
	want := Fingerprint(cfg)
	for _, dst := range []string{"127.0.0.1:49701", "127.0.0.1:49702"} {
		if c, ok := lookup(t, r, dst); !ok || c.Fingerprint != want {
			t.Fatalf("%s = %+v, want fingerprint %s", dst, c, want)
		}
	}
	noDefaults := cfg
	noDefaults.Image = ImageConfig{}
	if c, _ := lookup(t, r, "127.0.0.1:49703"); c.Fingerprint != Fingerprint(noDefaults) || c.Fingerprint == want {
		t.Fatalf("a container whose image is gone = %+v", c)
	}
	f.mu.Lock()
	reads := f.imageReads
	f.mu.Unlock()
	if reads != 2 {
		t.Fatalf("read images %d times, want once per image", reads)
	}
}

// A command or entrypoint that is the image's own is not counted: a
// container of another version of the image whose default command differs,
// set up the same, has the same fingerprint.
func TestFingerprintLeavesOutTheImagesCommand(t *testing.T) {
	v15 := suitePostgres()
	v15.Image.Cmd, v15.Cmd = []string{"postgres"}, []string{"postgres"}
	v15.Image.Entrypoint, v15.Entrypoint = []string{"docker-entrypoint.sh"}, []string{"docker-entrypoint.sh"}
	v16 := suitePostgres()
	v16.Image.Cmd, v16.Cmd = []string{"postgres", "-c", "jit=off"}, []string{"postgres", "-c", "jit=off"}
	v16.Image.Entrypoint, v16.Entrypoint = []string{"/usr/local/bin/entry.sh"}, []string{"/usr/local/bin/entry.sh"}
	if Fingerprint(v15) != Fingerprint(v16) {
		t.Fatal("each image's own default command and entrypoint changed the fingerprint")
	}
}
