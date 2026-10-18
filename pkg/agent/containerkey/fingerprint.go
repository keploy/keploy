package containerkey

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sort"
	"strings"
)

// Config is what a container's fingerprint is taken from: the container's
// configuration as Docker's inspect gives it, and its image's, whose defaults
// the fingerprint leaves out.
type Config struct {
	Env        []string
	Cmd        []string
	Entrypoint []string
	Labels     map[string]string
	Image      ImageConfig
}

// ImageConfig is an image's defaults: what a container of it has unless its
// creator says otherwise.
type ImageConfig struct {
	Env        []string
	Cmd        []string
	Entrypoint []string
	Labels     map[string]string
}

// fingerprintLen is a fingerprint's length in hex digits: 48 bits, which no
// two configurations of one repository and port in one recording share by
// chance.
const fingerprintLen = 12

// runLabels are the labels a tool gives a container that differ from run to
// run, or from machine to machine, while the container is configured the
// same; the fingerprint leaves them out. Each is listed, and tested, by name:
// any other label is the user's, and counts.
var runLabels = map[string]string{
	// Testcontainers names the session that started a container (Java and Go
	// spell it sessionId; .NET, Node and Python, session-id), and .NET its
	// resource reaper's session: a new ID each test run.
	"org.testcontainers.sessionId":               "a new ID each Testcontainers session",
	"org.testcontainers.session-id":              "a new ID each Testcontainers session",
	"org.testcontainers.resource-reaper-session": "a new ID each Testcontainers session",
	// Testcontainers for Go marks a container its reaper (Ryuk) removes, only
	// when the reaper runs: a setting of the run, often off in CI.
	"org.testcontainers.reap": "whether the run's resource reaper is on",
	// The Testcontainers library's version: the tool's, not the container's.
	"org.testcontainers.version": "the Testcontainers library's version",
	// Docker Compose writes where the project was brought up from: paths on
	// the machine that ran it.
	"com.docker.compose.project.working_dir":      "an absolute path on the machine that ran compose",
	"com.docker.compose.project.config_files":     "absolute paths on the machine that ran compose",
	"com.docker.compose.project.environment_file": "absolute paths on the machine that ran compose",
	// The project name defaults to the directory compose ran in.
	"com.docker.compose.project": "the directory compose ran in, by default",
	// The image's ID: a new one whenever the tag moves, as the repository
	// in the key does not.
	"com.docker.compose.image": "the image ID, which moves with the tag",
	// A hash compose takes of the service's whole definition, the image's
	// tag among it: the fingerprint hashes what it means itself.
	"com.docker.compose.config-hash": "a hash of the service definition, the image tag included",
	// The compose tool's own version.
	"com.docker.compose.version": "the compose tool's version",
	// The container a recreated one replaces.
	"com.docker.compose.replace": "the ID of the container this one replaced",
	// A random suffix each "compose run" container gets.
	"com.docker.compose.slug": "random for each compose run container",
	// The digest of the image's volumes, which moves with the image.
	"com.docker.compose.image-volume-digest": "moves with the image, as the repository in the key does not",
}

// runLabelPrefixes are prefixes of labels a runtime gives every container:
// Docker Desktop writes its port scheme and the host paths of binds and
// mounts (desktop.docker.io/binds/0/Source: a path on the machine that ran
// it).
var runLabelPrefixes = []string{"desktop.docker.io/"}

// runEnv are environment variables Podman sets in a container's
// configuration, which Docker leaves to the process: the container's
// hostname (its short ID), its own name, and root's home. They are left out
// only of a configuration Podman wrote (podmanEnv in it): under Docker each
// is one the user set, and counts.
var runEnv = map[string]string{
	"HOSTNAME":  "the container's short ID, under Podman",
	"container": "the runtime's name, under Podman",
	"HOME":      "root's home, under Podman, where Docker sets none",
}

// podmanEnv marks a configuration Podman wrote.
const podmanEnv = "container=podman"

// isRunLabel reports whether label k is a run's (runLabels,
// runLabelPrefixes), not the container's own.
func isRunLabel(k string) bool {
	if _, ok := runLabels[k]; ok {
		return true
	}
	for _, p := range runLabelPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// Fingerprint names a container's own configuration: its environment,
// command, entrypoint and labels, apart from what its image gives every
// container of it (so a container of another version of the image, set up
// the same, has the same fingerprint) and apart from what a tool or runtime
// gives each run (runLabels, runLabelPrefixes, runEnv). An
// environment variable or label the creator set to the image's own value is
// the image's. The command and entrypoint count only where they are not the
// image's.
//
// No secret's value counts (redactSecrets): a recording holds the
// fingerprint, and one a password's value changed was an offline oracle for
// it -- hash a guess with the rest of the configuration, compare. A secret's
// presence still counts. So two containers that differ only in a secret
// share a fingerprint: where both answer one scope their key is ambiguous,
// withheld, and a call to either is a miss, never answered with the other's
// recording. Across runs, a container whose only change is a secret's value
// (CI's password where the recording had a developer's) is the recording's.
// The fingerprint carries its form ("s" before the hash), which
// TestTheFingerprintsFormIsPinned pins: a change of what is redacted needs a
// form of its own. Recordings made before carry the legacy one
// (legacyFingerprint), which each of their mocks is compared with (SameKey).
func Fingerprint(c Config) string {
	sum := sha256.Sum256(append([]byte("containerkey-fingerprint-s\x00"), configJSON(redactSecrets(c))...))
	return fingerprintForm + hex.EncodeToString(sum[:])[:fingerprintLen]
}

// fingerprintForm marks a fingerprint Fingerprint gives, apart from the
// legacy ones (legacyFingerprint), twelve hex digits alone.
const fingerprintForm = "s"

// legacyFingerprint is the fingerprint recordings made before Fingerprint
// left secrets out carry: the configuration's values as they are, secrets
// included. It is only ever compared with a mock of such a recording
// (SameKey), never written, logged or shown in a miss.
func legacyFingerprint(c Config) string {
	sum := sha256.Sum256(configJSON(c))
	return hex.EncodeToString(sum[:])[:fingerprintLen]
}

// configJSON is what a fingerprint hashes of c: its own environment,
// command, entrypoint and labels, apart from its image's and a run's.
func configJSON(c Config) []byte {
	env := slices.Clone(c.Env)
	podman := slices.Contains(c.Env, podmanEnv)
	env = slices.DeleteFunc(env, func(e string) bool {
		name, _, _ := strings.Cut(e, "=")
		_, run := runEnv[name]
		return (podman && run) || slices.Contains(c.Image.Env, e)
	})
	sort.Strings(env)
	var labels [][2]string
	for k, v := range c.Labels {
		if isRunLabel(k) {
			continue
		}
		if iv, ok := c.Image.Labels[k]; ok && iv == v {
			continue
		}
		labels = append(labels, [2]string{k, v})
	}
	sort.Slice(labels, func(i, j int) bool { return labels[i][0] < labels[j][0] })
	own := func(v, image []string) []string {
		if slices.Equal(v, image) {
			return nil
		}
		if v == nil {
			return []string{}
		}
		return v
	}
	b, _ := json.Marshal(struct {
		Env        []string    `json:"env"`
		Cmd        []string    `json:"cmd"`
		Entrypoint []string    `json:"entrypoint"`
		Labels     [][2]string `json:"labels"`
	}{env, own(c.Cmd, c.Image.Cmd), own(c.Entrypoint, c.Image.Entrypoint), labels})
	return b
}

// isFingerprint reports whether s is a fingerprint Fingerprint gives, or a
// legacy one (isLegacyFingerprint).
func isFingerprint(s string) bool {
	rest, ok := strings.CutPrefix(s, fingerprintForm)
	return (ok && isHexFingerprint(rest)) || isLegacyFingerprint(s)
}

// isLegacyFingerprint reports whether s is a fingerprint of the legacy form
// (legacyFingerprint): a recording made before secrets were left out.
func isLegacyFingerprint(s string) bool { return isHexFingerprint(s) }

func isHexFingerprint(s string) bool {
	if len(s) != fingerprintLen {
		return false
	}
	return strings.Trim(s, "0123456789abcdef") == ""
}

// instanceToken is the opaque token a recording names a container's
// instance by (InstanceKey): its ID's hash, so the ID is never written.
func instanceToken(id string) string {
	sum := sha256.Sum256([]byte("containerkey-instance\x00" + id))
	return hex.EncodeToString(sum[:])[:fingerprintLen]
}
