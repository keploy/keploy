package mock

import "context"

// FileStore is the OSS mock-set store: mocks live on disk under keploy/<name>/
// and are committed to the repo (the VCR "cassette" model). Pull and Push are
// no-ops because there is no remote to sync with. Enterprise replaces this with
// a registry-backed store that uploads after record and downloads before replay.
type FileStore struct{}

// Pull is a no-op for file-backed sets (already on disk).
func (FileStore) Pull(_ context.Context, _ string) error { return nil }

// Push is a no-op for file-backed sets (nothing to publish).
func (FileStore) Push(_ context.Context, _ string) error { return nil }
