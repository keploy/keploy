package mockdb

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
)

// unownedFileName is the file a capture lands in when no scope owned it.
//
// It is deliberately the historical name. A runner that reports no scopes at
// all -- a curl script, a Go test binary, a pytest suite without a plugin --
// gives every mock an empty Owner, so the whole set lands in exactly the one
// mocks.yaml it has always landed in, byte for byte. Per-owner files are then
// an additive property of sets that DO declare ownership, not a format change
// every keploy user is forced through, and an already-recorded set still reads.
const unownedFileName = "mocks"

// ownerFileBase is the 12-hex name minted by ownerHash. Matching on that exact
// shape is what lets per-owner files sit flat beside config.yaml and
// mappings.yaml without a prefix or a subdirectory: neither can collide.
var ownerFileBase = regexp.MustCompile(`^[0-9a-f]{12}$`)

// ownerFileName is the base filename (no extension) that a mock owned by
// `owner` belongs in. One file = one owner = one server row.
func ownerFileName(owner string) string {
	if owner == "" {
		return unownedFileName
	}
	return ownerHash(owner)
}

// mockFileBases lists every mock-file base name present in a set directory,
// in a stable order: the unowned file first, then owners sorted.
//
// Order is load-bearing on the read path. pkg/util.go returns a test's own
// mocks ahead of the shared tier so a broadly-matching shared recording cannot
// win a URL tie against the test's own; concatenating owner files in map order
// would reintroduce exactly that nondeterminism between runs.
func mockFileBases(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var owners []string
	seen := map[string]bool{}
	hasUnowned := false

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		base := e.Name()
		ext := filepath.Ext(base)
		switch ext {
		case ".yaml", ".yml", ".json", ".gob":
		default:
			continue
		}
		base = base[:len(base)-len(ext)]
		if base == unownedFileName {
			hasUnowned = true
			continue
		}
		if !ownerFileBase.MatchString(base) || seen[base] {
			continue
		}
		seen[base] = true
		owners = append(owners, base)
	}

	sort.Strings(owners)
	if hasUnowned {
		return append([]string{unownedFileName}, owners...), nil
	}
	return owners, nil
}

// mockFilePaths expands mockFileBases into concrete paths that exist on disk,
// trying every extension a set may legitimately be stored in.
func mockFilePaths(dir string) ([]string, error) {
	bases, err := mockFileBases(dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, base := range bases {
		for _, ext := range []string{
			yaml.FormatYAML.FileExtension(),
			yaml.FormatJSON.FileExtension(),
			"gob",
		} {
			p := filepath.Join(dir, base+"."+ext)
			if _, err := os.Stat(p); err == nil {
				paths = append(paths, p)
			}
		}
	}
	return paths, nil
}

// forEachMockFile runs `read` over every mock file in the set and concatenates
// the results, unowned file first then owners in sorted order.
//
// An explicit --mock-name still addresses exactly one file: that flag names a
// file, and honouring it is what lets a caller read a single owner in isolation.
func (ys *MockYaml) forEachMockFile(testSetID string, read func(mockFileName string) ([]*models.Mock, error)) ([]*models.Mock, error) {
	if ys.MockName != "" {
		return read(ys.MockName)
	}
	bases, err := mockFileBases(filepath.Join(ys.MockPath, testSetID))
	if err != nil {
		return nil, err
	}
	if len(bases) == 0 {
		// Read the unowned name anyway so the not-found path keeps its existing
		// behaviour: callers rely on an absent set returning empty, not an error.
		return read(unownedFileName)
	}
	var all []*models.Mock
	for _, base := range bases {
		mocks, err := read(base)
		if err != nil {
			return nil, err
		}
		all = append(all, mocks...)
	}
	return all, nil
}

// OwnerFileName exposes the per-owner file naming to other keploy-linked
// binaries -- notably the enterprise CLI, which must write a downloaded owner
// to exactly the filename the recorder would have written. A second
// implementation of this rule would drift and silently split one set in two.
func OwnerFileName(owner string) string {
	return ownerFileName(owner)
}
