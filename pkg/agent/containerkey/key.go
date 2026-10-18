// Package containerkey names a container destination the way the proven
// model's recordings and matchers do: by the container, not by the address
// the app happened to reach it at.
//
// A dependency the tests reach through a container's published port, or at a
// container's own IP, gets an address that changes from run to run:
// Testcontainers publishes on a random host port, and Docker hands out
// container IPs as it pleases. Its key does not change:
//
//	container:<repository>:<containerPort>@<fingerprint>
//
// <repository> is the image the container was created from, without its tag
// or digest, in Docker's short form (Repository): a container of another
// version of the image (postgres:15 recorded, postgres:16 replayed) is the
// same dependency. <containerPort> is the container's own port. <fingerprint>
// names the container's own configuration (Fingerprint): its environment,
// command, entrypoint and labels, apart from its image's defaults and from
// the labels a tool gives each run (a Testcontainers session ID). Two
// containers of one image configured differently are two dependencies, in
// whatever order the tests start them. Two configured the same cannot be
// told apart: where both are in use in one scope, neither is named by its key
// (Keyer), so their calls miss rather than take each other's recordings.
// (Founder decision 19 supersedes design §3.1's first-use number "#n", which
// concurrent first uses could swap.)
//
// The agent names each connection from an index of the local daemon's
// containers that Docker's event stream keeps (dockerIndex), so naming never
// waits on Docker; a key waits only for its own container's configuration to
// be read, when a call is matched or a mock stamped before the read is in.
//
// The key is recorded on the mock (its metadata, MetadataKey), with the
// container's instance and the scope it was used in (InstanceKey,
// ScopeKey), and handed to the parsers with the connection (FromContext).
// It stands in for the address where a matcher compares one: S1 compares it
// in place of the Host. A recording made without the proven switch, or of a
// destination that is not a container, has no key, and is compared by its
// address as before.
package containerkey

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/distribution/reference"
	"go.keploy.io/server/v3/pkg/models"
)

// MetadataKey is the mock metadata field that holds a container destination's
// key. Recordings made before container keys, and mocks of any other
// destination, do not have it.
const MetadataKey = "containerKey"

// InstanceKey and ScopeKey are the mock metadata fields that say which
// container a mock of a container destination came from (an opaque token, not
// its ID, the same for every mock of that container in one recording) and in
// which scope it was used (an opaque token, the same for every mock of one
// test, or of the run outside tests). A recording's mocks of one key from two
// instances in one scope are of two containers configured the same, which
// the key cannot tell apart (RecordedAmbiguity). A mock with an instance and
// no key came from a container whose key could not be read.
const (
	InstanceKey = "containerInstance"
	ScopeKey    = "containerScope"
)

// UnreadKey is the mock metadata field that says the mock's container could
// not be named because the Docker daemon did not answer keploy's read of its
// configuration (the container, named for a message): the mock's own test
// is not judged, whenever the read stalled (a record charges it by the mock,
// not by when the stall came).
const UnreadKey = models.ContainerUnreadMetadata

// prefix starts every key, and every `container:` entry of mock.real.
const prefix = "container:"

// Container is a container destination as the agent resolved it.
type Container struct {
	// ID is the container's Docker ID. It names the container only for the
	// run that saw it, so it is never written into a recording.
	ID string
	// Image is the image reference the container was created from, in the
	// form NormalizeImage gives.
	Image string
	// Port is the container's own port the connection reached.
	Port uint16
	// Fingerprint names the container's own configuration (Fingerprint); ""
	// until the index has read it.
	Fingerprint string
	// Legacy is its fingerprint of the legacy form (legacyFingerprint), which
	// a replay of a recording made before secrets were left out compares
	// with; never written.
	Legacy string
}

// Key is a parsed container key.
type Key struct {
	// Repo is the image's repository, in the form Repository gives.
	Repo string
	Port uint16
	// Fingerprint names the container's own configuration (Fingerprint).
	Fingerprint string
}

// Label names container c in a listing (the topology's mocked rows): its key,
// or, while its configuration is unread, its repository and port without a
// fingerprint (container:<repository>:<port>), which names no recording.
func Label(c Container) string {
	if k, ok := KeyOf(c); ok {
		return k.String()
	}
	return prefix + Repository(c.Image) + ":" + strconv.Itoa(int(c.Port))
}

// KeyOf is c's key; ok is false while its configuration is unread.
func KeyOf(c Container) (Key, bool) {
	if c.Fingerprint == "" {
		return Key{}, false
	}
	return Key{Repo: Repository(c.Image), Port: c.Port, Fingerprint: c.Fingerprint}, true
}

// legacyKeyOf is c's key of the legacy form, as a recording made before
// secrets were left out names it (legacyFingerprint); ok is false while its
// configuration is unread.
func legacyKeyOf(c Container) (Key, bool) {
	if c.Legacy == "" {
		return Key{}, false
	}
	return Key{Repo: Repository(c.Image), Port: c.Port, Fingerprint: c.Legacy}, true
}

// String is the key as recordings and matchers carry it.
func (k Key) String() string {
	return prefix + k.Repo + ":" + strconv.Itoa(int(k.Port)) + "@" + k.Fingerprint
}

// Repository is the key's repository, which is what a mock.real
// `container:<repo>` entry names.
func (k Key) Repository() string { return k.Repo }

// ErrNotKey is what Parse returns for a string that is not a container key.
var ErrNotKey = errors.New("not a container key")

// Parse reads a key String wrote. A repository has no '@', and its last ':'
// before the '@' is the one String put before the port, so a registry port
// (localhost:5000/app) never makes the parse ambiguous. A key of the first
// format ("container:<image>:<port>#<n>") is not one: its number named a
// container by the order of first use, which this format replaced.
func Parse(s string) (Key, error) {
	rest, ok := strings.CutPrefix(s, prefix)
	if !ok {
		return Key{}, fmt.Errorf("%w: %q has no %q prefix", ErrNotKey, s, prefix)
	}
	at := strings.LastIndexByte(rest, '@')
	if at < 0 {
		return Key{}, fmt.Errorf("%w: %q has no @<fingerprint>", ErrNotKey, s)
	}
	fp := rest[at+1:]
	if !isFingerprint(fp) {
		return Key{}, fmt.Errorf("%w: %q has no fingerprint after its @", ErrNotKey, s)
	}
	colon := strings.LastIndexByte(rest[:at], ':')
	if colon <= 0 {
		return Key{}, fmt.Errorf("%w: %q has no repository and port", ErrNotKey, s)
	}
	port, err := strconv.ParseUint(rest[colon+1:at], 10, 16)
	if err != nil || port == 0 || strconv.FormatUint(port, 10) != rest[colon+1:at] {
		return Key{}, fmt.Errorf("%w: %q has no port", ErrNotKey, s)
	}
	return Key{Repo: rest[:colon], Port: uint16(port), Fingerprint: fp}, nil
}

// NormalizeImage writes an image reference the way Docker's short form does,
// so that two spellings of one reference give one key: postgres,
// postgres:latest, library/postgres:latest and docker.io/library/postgres:latest
// are all postgres:latest. A tag or digest is kept: a container of another
// version of an image is another dependency. A reference Docker's grammar does
// not read is kept as it is. (The index keeps an image ID, which this would
// take for a repository name, before it gets here.)
func NormalizeImage(ref string) string {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return ref
	}
	return reference.FamiliarString(reference.TagNameOnly(named))
}

// Repository is an image reference without its tag or digest, in Docker's
// short form: what a mock.real `container:<repo>` entry names (design §4). An
// image ID names no repository and is kept as it is.
func Repository(image string) string {
	if strings.HasPrefix(image, "sha256:") {
		return image
	}
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return image
	}
	return reference.FamiliarName(named)
}

// Recorded is the container key a recorded mock was stamped with, if any.
func Recorded(m *models.Mock) (string, bool) {
	if m == nil {
		return "", false
	}
	k := m.Spec.Metadata[MetadataKey]
	return k, k != ""
}
