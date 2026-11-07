package testset

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func linkTo(t *testing.T, target, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(filepath.Dir(path), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, path); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
}

func refused(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "only inside its recording") {
		t.Fatalf("%s: %v, want it refused as outside the recording", what, err)
	}
}

// A test set's config.yaml is read and written only inside the recording: a
// link out is refused by every read and by the write, which leaves the link
// and what it leads to as they were. A link inside the recording is read.
func TestASetsConfigStaysInsideTheRecording(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root := filepath.Join(base, "keploy")
	out := filepath.Join(base, "outside.yaml")
	const secret = "preScript: SECRET-OUTSIDE\n"
	if err := os.WriteFile(out, []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "set", "config.yaml")
	linkTo(t, out, file)
	db := New[*models.TestSet](zap.NewNop(), root)

	conf, err := db.Read(ctx, "set")
	refused(t, "Read", err)
	if conf != nil && conf.PreScript != "" {
		t.Fatalf("Read read %q from outside the recording", conf.PreScript)
	}
	_, err = db.ReadForUpdate(ctx, "set")
	refused(t, "ReadForUpdate", err)
	refused(t, "Write", db.Write(ctx, "set", &models.TestSet{PreScript: "new"}))
	got, err := os.ReadFile(out)
	if err != nil || string(got) != secret {
		t.Fatalf("the file outside the recording holds %q (%v)", got, err)
	}
	if info, err := os.Lstat(file); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link is gone: %v, %v", info, err)
	}

	if err := db.Write(ctx, "other", &models.TestSet{PreScript: "inside"}); err != nil {
		t.Fatal(err)
	}
	linkTo(t, filepath.Join(root, "other", "config.yaml"), filepath.Join(root, "linked", "config.yaml"))
	conf, err = db.Read(ctx, "linked")
	if err != nil || conf == nil || conf.PreScript != "inside" {
		t.Fatalf("a link inside the recording: %+v, %v", conf, err)
	}
}

// A set folder that links out gets no config.yaml.
func TestASetsConfigIsWrittenOnlyInsideTheRecording(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "keploy")
	out := filepath.Join(base, "outside")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	linkTo(t, out, filepath.Join(root, "set"))
	db := New[*models.TestSet](zap.NewNop(), root)
	refused(t, "Write", db.Write(context.Background(), "set", &models.TestSet{PreScript: "new"}))
	if entries, err := os.ReadDir(out); err != nil || len(entries) != 0 {
		t.Fatalf("Write left %v (%v) outside the recording", entries, err)
	}
}

// A test set's secret.yaml is read only inside the recording: a link out is
// refused, with why, and nothing of it reaches the config.
func TestASetsSecretIsReadOnlyInsideTheRecording(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root := filepath.Join(base, "keploy")
	out := filepath.Join(base, "outside.yaml")
	if err := os.WriteFile(out, []byte("token: SECRET-OUTSIDE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkTo(t, out, filepath.Join(root, "set", "secret.yaml"))
	db := New[*models.TestSet](zap.NewNop(), root)
	values, err := db.ReadSecret(ctx, "set")
	refused(t, "ReadSecret", err)
	if len(values) != 0 {
		t.Fatalf("ReadSecret read %v from outside the recording", values)
	}
}

// A config read goes on without a secret.yaml that is refused, as it does
// with any secret it cannot read, but says so: a user whose secret file links
// out of the recording is told why the run has no secrets, at a level a run
// shows.
func TestARefusedSecretIsSaid(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "keploy")
	out := filepath.Join(base, "outside.yaml")
	if err := os.WriteFile(out, []byte("token: SECRET-OUTSIDE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkTo(t, out, filepath.Join(root, "set", "secret.yaml"))
	core, logs := observer.New(zapcore.WarnLevel)
	db := New[*models.TestSet](zap.New(core), root)
	conf, err := db.Read(context.Background(), "set")
	if err != nil || conf == nil {
		t.Fatalf("Read: %+v, %v, want a config without secrets", conf, err)
	}
	said := false
	for _, e := range logs.All() {
		for k, v := range e.ContextMap() {
			said = said || (k == "error" && strings.Contains(v.(string), "only inside its recording"))
		}
	}
	if !said {
		t.Fatalf("nothing at warn or above said why: %v", logs.All())
	}
}
