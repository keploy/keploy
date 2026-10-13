package containerkey

import (
	"errors"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func TestKeyStringAndParseRoundTrip(t *testing.T) {
	for _, k := range []Key{
		{Image: "postgres:16-alpine", Port: 5432, N: 1},
		{Image: "localstack/localstack:3.8", Port: 4566, N: 2},
		{Image: "localhost:5000/team/app:1.2", Port: 8080, N: 10},
		{Image: "redis@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Port: 6379, N: 1},
		{Image: "redis:7@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Port: 6379, N: 3},
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
	if got := (Key{Image: "postgres:16", Port: 5432, N: 1}).String(); got != "container:postgres:16:5432#1" {
		t.Fatalf("String() = %q", got)
	}
}

func TestParseRefusesWhatIsNotAKey(t *testing.T) {
	for _, s := range []string{
		"",
		"127.0.0.1:5432",
		"container:postgres:16:5432",    // no #n
		"container:postgres:16:5432#0",  // numbers start at 1
		"container:postgres:16:5432#01", // not as String writes it
		"container:postgres:16:5432#x",
		"container:postgres#1",          // no port
		"container::5432#1",             // no image
		"container:postgres:0#1",        // no port 0
		"container:postgres:65536#1",    // not a port
		"container:postgres:05432#1",    // not as String writes it
		"container:postgres:16:5432#-1", // negative
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
	if got := (Key{Image: "redis:7", Port: 6379, N: 1}).Repository(); got != "redis" {
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
	m := &models.Mock{Spec: models.MockSpec{Metadata: map[string]string{MetadataKey: "container:redis:7:6379#1"}}}
	if k, ok := Recorded(m); !ok || k != "container:redis:7:6379#1" {
		t.Fatalf("Recorded = %q, %v", k, ok)
	}
}
