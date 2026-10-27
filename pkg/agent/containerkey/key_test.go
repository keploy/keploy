package containerkey

import (
	"errors"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func TestKeyStringAndParseRoundTrip(t *testing.T) {
	for _, k := range []Key{
		{Repo: "postgres", Port: 5432, Fingerprint: "0123456789ab"},
		{Repo: "localstack/localstack", Port: 4566, Fingerprint: "ffffffffffff"},
		{Repo: "localhost:5000/team/app", Port: 8080, Fingerprint: "a1b2c3d4e5f6"},
		{Repo: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Port: 6379, Fingerprint: "000000000000"},
	} {
		s := k.String()
		got, err := Parse(s)
		if err != nil {
			t.Fatalf("Parse(%q): %v", s, err)
		}
		if got != k {
			t.Fatalf("Parse(%q) = %+v, want %+v", s, got, k)
		}
	}
	if got := (Key{Repo: "postgres", Port: 5432, Fingerprint: "0123456789ab"}).String(); got != "container:postgres:5432@0123456789ab" {
		t.Fatalf("String() = %q", got)
	}
}

// KeyOf names a container by its repository, port and fingerprint: another
// tag of the image is the same key; a container whose configuration is not
// read yet has none.
func TestKeyOfIsTheRepositoryPortAndFingerprint(t *testing.T) {
	a, ok := KeyOf(Container{ID: "c1", Image: "postgres:15", Port: 5432, Fingerprint: "0123456789ab"})
	b, _ := KeyOf(Container{ID: "c2", Image: "docker.io/library/postgres:16", Port: 5432, Fingerprint: "0123456789ab"})
	if !ok || a != b || a.String() != "container:postgres:5432@0123456789ab" {
		t.Fatalf("KeyOf = %v %v, %v", a, ok, b)
	}
	if _, ok := KeyOf(Container{ID: "c3", Image: "postgres:16", Port: 5432}); ok {
		t.Fatal("a container whose configuration is unread has a key")
	}
}

// A listing names a container by its key, or, while its configuration is
// unread, by its repository and port with no fingerprint, which Parse refuses
// as a key: it names no recording.
func TestLabelIsTheKeyOrTheRepositoryAndPort(t *testing.T) {
	read := Container{ID: "c1", Image: "postgres:16", Port: 5432, Fingerprint: "aaaaaaaaaaaa"}
	if got := Label(read); got != "container:postgres:5432@aaaaaaaaaaaa" {
		t.Fatalf("Label = %q", got)
	}
	unread := Container{ID: "c1", Image: "postgres:16", Port: 5432}
	if got := Label(unread); got != "container:postgres:5432" {
		t.Fatalf("Label = %q", got)
	}
	if _, err := Parse(Label(unread)); err == nil {
		t.Fatal("an unread container's label parsed as a key")
	}
}

func TestParseRefusesWhatIsNotAKey(t *testing.T) {
	for _, s := range []string{
		"",
		"127.0.0.1:5432",
		"container:postgres:16:5432#1",          // the first format's number
		"container:postgres:5432",               // no fingerprint
		"container:postgres:5432@",              // empty fingerprint
		"container:postgres:5432@0123456789",    // too short
		"container:postgres:5432@0123456789abc", // too long
		"container:postgres:5432@0123456789AB",  // not as String writes it
		"container:postgres@0123456789ab",       // no port
		"container::5432@0123456789ab",          // no repository
		"container:postgres:0@0123456789ab",     // no port 0
		"container:postgres:65536@0123456789ab",
		"container:postgres:05432@0123456789ab",
	} {
		if _, err := Parse(s); !errors.Is(err, ErrNotKey) {
			t.Fatalf("Parse(%q) error = %v, want ErrNotKey", s, err)
		}
	}
}

func TestNormalizeImageGivesOneSpellingPerReference(t *testing.T) {
	for in, want := range map[string]string{
		"postgres":                            "postgres:latest",
		"postgres:latest":                     "postgres:latest",
		"library/postgres:latest":             "postgres:latest",
		"docker.io/library/postgres:latest":   "postgres:latest",
		"postgres:16-alpine":                  "postgres:16-alpine",
		"docker.io/localstack/localstack:3.8": "localstack/localstack:3.8",
		"ghcr.io/org/svc:1":                   "ghcr.io/org/svc:1",
		"localhost:5000/app":                  "localhost:5000/app:latest",
		"mirror.gcr.io/library/redis:7":       "mirror.gcr.io/library/redis:7",
		"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef":        "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	} {
		if got := NormalizeImage(in); got != want {
			t.Errorf("NormalizeImage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRepositoryDropsTagAndDigest(t *testing.T) {
	for in, want := range map[string]string{
		"redis:7":                        "redis",
		"docker.io/library/redis:latest": "redis",
		"localstack/localstack:3.8":      "localstack/localstack",
		"localhost:5000/team/app:1.2":    "localhost:5000/team/app",
		"redis:7@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef": "redis",
	} {
		if got := Repository(in); got != want {
			t.Errorf("Repository(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Repository("sha256:0123456789abcdef"); got != "sha256:0123456789abcdef" {
		t.Fatalf("Repository of an image ID = %q, want the ID", got)
	}
	if got := (Key{Repo: "redis", Port: 6379, Fingerprint: "0123456789ab"}).Repository(); got != "redis" {
		t.Fatalf("Key.Repository() = %q", got)
	}
}

func TestRecordedReadsTheMockMetadata(t *testing.T) {
	if _, ok := Recorded(nil); ok {
		t.Fatal("a nil mock has a key")
	}
	if _, ok := Recorded(&models.Mock{}); ok {
		t.Fatal("a mock without metadata has a key")
	}
	m := &models.Mock{Spec: models.MockSpec{Metadata: map[string]string{MetadataKey: "container:redis:6379@0123456789ab"}}}
	if k, ok := Recorded(m); !ok || k != "container:redis:6379@0123456789ab" {
		t.Fatalf("Recorded = %q, %v", k, ok)
	}
}
