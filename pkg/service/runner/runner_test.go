// Tests for the runner package (package name: integration).
//
// loadMappingsForSet is the regression surface here. The fix changed its
// behaviour when mappings.yaml is absent on disk: previously it errored
// with "no mock mappings found for test set %q" and the entire runner
// blew up; the fix returns empty maps + nil error so the runner can
// degrade to "use every mock in the set" (DisableMapping-equivalent).
// This file pins all three branches of that function so a future refactor
// can't accidentally reintroduce the error-on-empty behaviour or break
// the meaningful-mappings success path or the genuine I/O-error
// propagation path.
package integration

import (
	"context"
	"errors"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// stubMappingDB is a hand-rolled minimal stub. We don't pull in testify
// mocks because the surface is a single method and the call shape is
// already deterministic per sub-test.
type stubMappingDB struct {
	mappings      map[string][]models.MockEntry
	hasMeaningful bool
	err           error
	calledWithSet string
	callCount     int
}

func (s *stubMappingDB) Get(_ context.Context, testSetID string) (map[string][]models.MockEntry, bool, error) {
	s.callCount++
	s.calledWithSet = testSetID
	return s.mappings, s.hasMeaningful, s.err
}

// TestLoadMappingsForSet_MissingFile pins the tolerance for an absent
// mappings.yaml. The fix replaced an outright error with "return empty
// maps so the runner falls back to loading every mock in the set"; this
// is the path every OSS-shape sandbox replay takes (no mappings.yaml on
// disk, just keploy/<set>/{tests,mocks.yaml}).
func TestLoadMappingsForSet_MissingFile(t *testing.T) {
	t.Run("missing_returns_empty_maps_no_error", func(t *testing.T) {
		// hasMeaningful = false, no error — the "no mappings on disk"
		// shape MappingDB returns when mappings.yaml is absent.
		stub := &stubMappingDB{
			mappings:      map[string][]models.MockEntry{},
			hasMeaningful: false,
			err:           nil,
		}
		r := &Runner{mappingDB: stub}

		mappings, mocksThatHaveMappings, mocksWeNeed, err := r.loadMappingsForSet(context.Background(), "set-without-mappings")

		if err != nil {
			t.Fatalf("expected nil error when mappings absent, got %v", err)
		}
		if mappings == nil {
			t.Fatalf("expected non-nil empty mappings map, got nil (downstream code dereferences this directly)")
		}
		if len(mappings) != 0 {
			t.Fatalf("expected empty mappings map, got %d entries", len(mappings))
		}
		if mocksThatHaveMappings == nil {
			t.Fatalf("expected non-nil empty mocksThatHaveMappings map, got nil")
		}
		if len(mocksThatHaveMappings) != 0 {
			t.Fatalf("expected empty mocksThatHaveMappings map, got %d entries", len(mocksThatHaveMappings))
		}
		if mocksWeNeed == nil {
			t.Fatalf("expected non-nil empty mocksWeNeed map, got nil")
		}
		if len(mocksWeNeed) != 0 {
			t.Fatalf("expected empty mocksWeNeed map, got %d entries", len(mocksWeNeed))
		}
		if stub.calledWithSet != "set-without-mappings" {
			t.Fatalf("Get was called with %q, expected %q", stub.calledWithSet, "set-without-mappings")
		}
	})

	t.Run("meaningful_mappings_returned_unchanged", func(t *testing.T) {
		// Success path — confirms the fix did NOT break the path
		// that loads real mappings.yaml content.
		want := map[string][]models.MockEntry{
			"test-1": {{Name: "mock-1", Kind: "Http"}, {Name: "mock-2", Kind: "Postgres"}},
			"test-2": {{Name: "mock-2", Kind: "Postgres"}, {Name: "mock-3", Kind: "MySQL"}},
		}
		stub := &stubMappingDB{
			mappings:      want,
			hasMeaningful: true,
			err:           nil,
		}
		r := &Runner{mappingDB: stub}

		mappings, mocksThatHaveMappings, mocksWeNeed, err := r.loadMappingsForSet(context.Background(), "set-with-mappings")
		if err != nil {
			t.Fatalf("expected nil error on success, got %v", err)
		}
		if len(mappings) != 2 {
			t.Fatalf("expected 2 test entries in mappings, got %d", len(mappings))
		}
		// UNION of mock names across both tests = {mock-1, mock-2, mock-3}.
		expectedUnion := map[string]bool{"mock-1": true, "mock-2": true, "mock-3": true}
		if len(mocksThatHaveMappings) != len(expectedUnion) {
			t.Fatalf("mocksThatHaveMappings: expected %d entries, got %d",
				len(expectedUnion), len(mocksThatHaveMappings))
		}
		for name := range expectedUnion {
			if !mocksThatHaveMappings[name] {
				t.Fatalf("mocksThatHaveMappings missing %q", name)
			}
			if !mocksWeNeed[name] {
				t.Fatalf("mocksWeNeed missing %q", name)
			}
		}
	})

	t.Run("genuine_error_is_propagated", func(t *testing.T) {
		// An I/O error from the underlying mappingDB.Get MUST NOT be
		// swallowed by the tolerance branch — only the
		// hasMeaningful=false path should degrade gracefully.
		sentinel := errors.New("disk read failed")
		stub := &stubMappingDB{
			mappings:      nil,
			hasMeaningful: false,
			err:           sentinel,
		}
		r := &Runner{mappingDB: stub}

		mappings, mthm, mwn, err := r.loadMappingsForSet(context.Background(), "set-broken")
		if err == nil {
			t.Fatalf("expected non-nil error when mappingDB.Get returns err, got nil")
		}
		if !errors.Is(err, sentinel) {
			t.Fatalf("expected returned error to wrap %v, got %v", sentinel, err)
		}
		if mappings != nil || mthm != nil || mwn != nil {
			t.Fatalf("expected nil maps on error path, got mappings=%v mthm=%v mwn=%v",
				mappings, mthm, mwn)
		}
	})

	t.Run("nil_mappingDB_returns_error", func(t *testing.T) {
		// Sanity check: keeps the nil-guard at the top of
		// loadMappingsForSet honoured. If a future refactor removes
		// that guard a nil-pointer panic would silently take down the
		// runner; better to surface the configuration error.
		r := &Runner{mappingDB: nil}
		_, _, _, err := r.loadMappingsForSet(context.Background(), "anything")
		if err == nil {
			t.Fatalf("expected error when mappingDB is nil, got nil")
		}
	})
}
