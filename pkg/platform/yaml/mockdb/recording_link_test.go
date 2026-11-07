package mockdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
)

// moveOutAndLink moves path out of the recording, beside base, and leaves a
// relative link to it in its place.
func moveOutAndLink(t *testing.T, base, path string) {
	t.Helper()
	out := filepath.Join(base, "outside-"+filepath.Base(path))
	if err := os.Rename(path, out); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(filepath.Dir(path), out)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, path); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
}

// requireRefusedMocks checks err refuses path, a file of the recording at
// root, naming it relative to root, and root.
func requireRefusedMocks(t *testing.T, what string, err error, root, path string) {
	t.Helper()
	rel, rerr := filepath.Rel(root, path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if want := fmt.Sprintf("%q is read only inside its recording %q", rel, root); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("%s: %v, want it to say %s", what, err, want)
	}
}

// A set's mock file (YAML or gob) is read only inside the recording: one
// that links out of it is refused, with why, by every read, and one that
// links to another set's file inside it is read.
func TestASetsMockFileIsReadOnlyInsideTheRecording(t *testing.T) {
	for _, format := range []string{string(yaml.FormatYAML), mockFormatGob} {
		t.Run(format, func(t *testing.T) {
			ctx := context.Background()
			base := t.TempDir()
			root := filepath.Join(base, "keploy")
			ys := clockStore(root, format)
			mock := genericNoiseMock("mock-0")
			mock.Spec.GenericResponses[0].Message[0].Data = "SECRET-OUTSIDE"
			for _, set := range []string{"set", "inside"} {
				if err := ys.InsertMock(ctx, mock, set); err != nil {
					t.Fatal(err)
				}
			}
			if err := ys.Close(); err != nil {
				t.Fatal(err)
			}
			ext := format
			if format == mockFormatGob {
				ext = "gob"
			}
			file := filepath.Join(root, "set", "mocks."+ext)
			moveOutAndLink(t, base, file)
			inside := filepath.Join(root, "inside", "mocks."+ext)
			if err := os.Remove(inside); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join("..", "set-in", "mocks."+ext), inside); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(root, "set-in"), 0o755); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(base, "outside-mocks."+ext))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "set-in", "mocks."+ext), raw, 0o644); err != nil {
				t.Fatal(err)
			}

			ys = clockStore(root, format)
			mocks, err := ys.GetUnFilteredMocks(ctx, "set", models.BaseTime, models.BaseTime.AddDate(100, 0, 0), nil, nil)
			requireRefusedMocks(t, "GetUnFilteredMocks", err, root, file)
			if len(mocks) != 0 {
				t.Fatalf("GetUnFilteredMocks read %d mocks out of the recording", len(mocks))
			}
			if got, skipped := ys.SkippedMocks(ctx, "set"); skipped != nil {
				t.Fatalf("SkippedMocks read %s: %v", got, skipped)
			}
			if format != mockFormatGob {
				_, err := NewMockFileReader(ctx, nil, root, filepath.Join(root, "set"), "mocks", yaml.FormatYAML)
				requireRefusedMocks(t, "NewMockFileReader", err, root, file)
			}

			mocks, err = ys.GetUnFilteredMocks(ctx, "inside", models.BaseTime, models.BaseTime.AddDate(100, 0, 0), nil, nil)
			if err != nil || len(mocks) != 1 || mocks[0].Name != "mock-0" {
				t.Fatalf("a link inside the recording: %d mocks, %v", len(mocks), err)
			}
		})
	}
}

// requireLinkKept fails unless path is still the link it was made, and the
// file it leads to out of the recording, out, still holds want.
func requireLinkKept(t *testing.T, what, path, out string, want []byte) {
	t.Helper()
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s: %s is no longer the link out of the recording (%v)", what, path, err)
	}
	if got, err := os.ReadFile(out); err != nil || string(got) != string(want) {
		t.Fatalf("%s changed the file outside the recording, %s (%v)", what, out, err)
	}
}

// What changes a set's mock file reads it only inside the recording too: a
// prune (UpdateMocks), the write-back of what a replay learned
// (PersistLearned) and, for gob, a record that adds to the set's file (its
// mocks are carried into the new stream) each refuse a file that links out of
// the recording, and leave the link and the file it leads to as they were.
func TestAChangeToASetsMockFileReadsItOnlyInsideTheRecording(t *testing.T) {
	for _, format := range []string{string(yaml.FormatYAML), mockFormatGob} {
		t.Run(format, func(t *testing.T) {
			ctx := context.Background()
			base := t.TempDir()
			root := filepath.Join(base, "keploy")
			ys := clockStore(root, format)
			mock := genericNoiseMock("mock-0")
			mock.Spec.GenericResponses[0].Message[0].Data = "SECRET-OUTSIDE"
			mock.Spec.ReqBodyNoise = map[string][]string{"body.kept": {}}
			if err := ys.InsertMock(ctx, mock, "set"); err != nil {
				t.Fatal(err)
			}
			if err := ys.Close(); err != nil {
				t.Fatal(err)
			}
			ext := format
			if format == mockFormatGob {
				ext = "gob"
			}
			file := filepath.Join(root, "set", "mocks."+ext)
			moveOutAndLink(t, base, file)
			out := filepath.Join(base, "outside-mocks."+ext)
			raw, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}

			ys = clockStore(root, format)
			err = ys.UpdateMocks(ctx, "set", map[string]models.MockState{}, models.BaseTime.AddDate(100, 0, 0), models.BaseTime)
			requireRefusedMocks(t, "UpdateMocks", err, root, file)
			requireLinkKept(t, "UpdateMocks", file, out, raw)

			states := map[string]models.MockState{"mock-0": {Name: "mock-0", ReqBodyNoise: map[string][]string{"body.learned": {}}}}
			added, err := ys.PersistLearned(ctx, "set", states)
			requireRefusedMocks(t, "PersistLearned", err, root, file)
			if len(added) != 0 {
				t.Fatalf("PersistLearned added %v to mocks read out of the recording", added)
			}
			requireLinkKept(t, "PersistLearned", file, out, raw)
			// What the replay learned is already on the mock: the read that
			// looks for a mock to change refuses the file too.
			unchanged := map[string]models.MockState{"mock-0": {Name: "mock-0", ReqBodyNoise: map[string][]string{"body.kept": {}}}}
			added, err = ys.PersistLearned(ctx, "set", unchanged)
			requireRefusedMocks(t, "PersistLearned, nothing to change", err, root, file)
			if len(added) != 0 {
				t.Fatalf("PersistLearned added %v to mocks read out of the recording", added)
			}

			if format == mockFormatGob {
				err := ys.InsertMock(ctx, genericNoiseMock("mock-1"), "set")
				if cerr := ys.Close(); err == nil {
					err = cerr
				}
				requireRefusedMocks(t, "InsertMock onto the set's gob file", err, root, file)
				requireLinkKept(t, "InsertMock onto the set's gob file", file, out, raw)
			}
		})
	}
}
