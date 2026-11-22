package mockdb

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// newMockYamlForFormat builds a MockYaml that writes the given format into dir.
// gob is selected through the per-instance MockFormat field (the same knob the
// gob round-trip tests use), yaml is the default.
func newMockYamlForFormat(dir, format string) *MockYaml {
	ys := New(zap.NewNop(), dir, "mocks")
	if format == mockFormatGob {
		ys.MockFormat = mockFormatGob
	}
	return ys
}

// httpMockURL is httpMock with a distinct URL. InsertMock renames mocks by an
// internal counter ("mock-<id>"), so the URL — not the Name — is what tells an
// old recording's mock apart from a freshly-captured one on read-back.
func httpMockURL(url string) *models.Mock {
	m := httpMock("x")
	m.Spec.HTTPReq.URL = url
	return m
}

func mockURLs(mocks []*models.Mock) []string {
	out := make([]string, 0, len(mocks))
	for _, m := range mocks {
		if m.Spec.HTTPReq != nil {
			out = append(out, m.Spec.HTTPReq.URL)
		}
	}
	return out
}

// TestPromoteStagedSet_ReplacesTargetAtomically is the yaml-layer guard for the
// non-destructive-record fix (gaps W1/W14; design §P0b). It checks, for both the
// yaml writer and the async truncate-on-open gob writer, that promoting a staged
// capture over an existing set (a) leaves a complete, corruption-free recording
// in the target — the gob tail must be flushed before the file is moved — and
// (b) fully replaces the prior recording rather than appending to it.
func TestPromoteStagedSet_ReplacesTargetAtomically(t *testing.T) {
	for _, format := range []string{"yaml", mockFormatGob} {
		t.Run(format, func(t *testing.T) {
			clearRegistry(t)
			ctx := context.Background()
			dir := t.TempDir()
			const target = "orders"
			staging := target + ".keploy-staging"

			// A prior recording, written and closed by its own instance — the
			// shape a previous record run left on disk.
			seed := newMockYamlForFormat(dir, format)
			if err := seed.InsertMock(ctx, httpMockURL("http://old/"), target); err != nil {
				t.Fatalf("seed InsertMock: %v", err)
			}
			if err := seed.Close(); err != nil {
				t.Fatalf("seed Close: %v", err)
			}

			// A fresh run captures two mocks into the staging set, then promotes.
			rec := newMockYamlForFormat(dir, format)
			for i := 0; i < 2; i++ {
				if err := rec.InsertMock(ctx, httpMockURL("http://new/"), staging); err != nil {
					t.Fatalf("staging InsertMock: %v", err)
				}
			}
			if err := rec.PromoteStagedSet(ctx, staging, target); err != nil {
				t.Fatalf("PromoteStagedSet: %v", err)
			}

			if _, err := os.Stat(filepath.Join(dir, staging)); !os.IsNotExist(err) {
				t.Fatalf("staging dir still present after promote: stat err=%v", err)
			}

			read := newMockYamlForFormat(dir, format)
			got, err := read.GetUnFilteredMocks(ctx, target, models.BaseTime, time.Now(), nil, nil)
			if err != nil {
				t.Fatalf("GetUnFilteredMocks: %v", err)
			}
			urls := mockURLs(got)
			if len(urls) != 2 {
				t.Fatalf("target has %d mocks after promote, want 2 (%v)", len(urls), urls)
			}
			for _, u := range urls {
				if u != "http://new/" {
					t.Fatalf("promote did not fully replace the old recording: %v", urls)
				}
			}
		})
	}
}

// TestDiscardStagedSet_LeavesTargetIntact checks the failed / interrupted /
// zero-capture path: discarding a staging set removes only the staging set and
// never touches the existing target recording.
func TestDiscardStagedSet_LeavesTargetIntact(t *testing.T) {
	for _, format := range []string{"yaml", mockFormatGob} {
		t.Run(format, func(t *testing.T) {
			clearRegistry(t)
			ctx := context.Background()
			dir := t.TempDir()
			const target = "orders"
			staging := target + ".keploy-staging"

			seed := newMockYamlForFormat(dir, format)
			if err := seed.InsertMock(ctx, httpMockURL("http://keep/"), target); err != nil {
				t.Fatalf("seed InsertMock: %v", err)
			}
			if err := seed.Close(); err != nil {
				t.Fatalf("seed Close: %v", err)
			}

			rec := newMockYamlForFormat(dir, format)
			if err := rec.InsertMock(ctx, httpMockURL("http://partial/"), staging); err != nil {
				t.Fatalf("staging InsertMock: %v", err)
			}
			if err := rec.DiscardStagedSet(ctx, staging); err != nil {
				t.Fatalf("DiscardStagedSet: %v", err)
			}

			if _, err := os.Stat(filepath.Join(dir, staging)); !os.IsNotExist(err) {
				t.Fatalf("staging dir still present after discard: stat err=%v", err)
			}

			read := newMockYamlForFormat(dir, format)
			got, err := read.GetUnFilteredMocks(ctx, target, models.BaseTime, time.Now(), nil, nil)
			if err != nil {
				t.Fatalf("GetUnFilteredMocks: %v", err)
			}
			if urls := mockURLs(got); len(urls) != 1 || urls[0] != "http://keep/" {
				t.Fatalf("discard changed the target recording: got %v, want [http://keep/]", urls)
			}
		})
	}
}

// TestPromoteStagedSet_EmptyStagingRefused is the safety guard: a promote with
// nothing staged must error and leave the target untouched, so a bug upstream
// can never blank a good recording.
func TestPromoteStagedSet_EmptyStagingRefused(t *testing.T) {
	clearRegistry(t)
	ctx := context.Background()
	dir := t.TempDir()
	const target = "orders"
	staging := target + ".keploy-staging"

	seed := New(zap.NewNop(), dir, "mocks")
	if err := seed.InsertMock(ctx, httpMockURL("http://keep/"), target); err != nil {
		t.Fatalf("seed InsertMock: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed Close: %v", err)
	}

	rec := New(zap.NewNop(), dir, "mocks")
	if err := rec.PromoteStagedSet(ctx, staging, target); err == nil {
		t.Fatalf("PromoteStagedSet with empty staging returned nil; want an error that leaves the target intact")
	}

	read := New(zap.NewNop(), dir, "mocks")
	got, err := read.GetUnFilteredMocks(ctx, target, models.BaseTime, time.Now(), nil, nil)
	if err != nil {
		t.Fatalf("GetUnFilteredMocks: %v", err)
	}
	if urls := mockURLs(got); len(urls) != 1 || urls[0] != "http://keep/" {
		t.Fatalf("empty-staging promote damaged the target: got %v, want [http://keep/]", urls)
	}
}
