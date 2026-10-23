package mockdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// payloadSource is a yaml or json mock file that a read left mocks' payloads
// in (GetTestSetMocksPayloadOnDisk). Each mock it left one in keeps where in
// the file the document it was decoded from is, and reads that document again,
// and only that one, when the mock is sent (models.Mock.WithPayload).
//
// It does not hold the file open: the file is opened for each read and must
// still be the one the mocks were read from. A rewrite of the file after a
// replay (the prune, the noise write-back) replaces it with a new file, which
// a read then refuses rather than taking a payload from another document; and
// an open file could not be replaced on every platform.
type payloadSource struct {
	path   string
	format yaml.Format
	// info is the file the mocks were read from, told apart from a file put
	// in its place since by os.SameFile.
	info   os.FileInfo
	mutate atomic.Pointer[models.PayloadMutator]
}

var _ models.PayloadSource = (*payloadSource)(nil)

func newPayloadSource(reader *yaml.MockReader) (*payloadSource, error) {
	info, err := reader.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat the mock file %s: %w", reader.Path(), err)
	}
	return &payloadSource{path: reader.Path(), format: reader.Format(), info: info}, nil
}

func (s *payloadSource) SetPayloadMutator(mutate models.PayloadMutator) {
	s.mutate.Store(&mutate)
}

// loader reads back the payload of the mock named name, the index'th mock
// decoded from the n bytes of the file at off.
func (s *payloadSource) loader(off int64, n int, index int, name string) models.PayloadLoader {
	return func(ctx context.Context, m *models.Mock) error {
		mock, err := s.read(off, n, index, name)
		if err != nil {
			return err
		}
		m.Spec.MongoRequests = mock.Spec.MongoRequests
		m.Spec.MongoResponses = mock.Spec.MongoResponses
		if mutate := s.mutate.Load(); mutate != nil && *mutate != nil {
			return (*mutate)(ctx, m)
		}
		return nil
	}
}

// read decodes the mocks of the document at [off, off+n) again, as the read
// that left their payloads here decoded them, and returns the index'th.
func (s *payloadSource) read(off int64, n int, index int, name string) (*models.Mock, error) {
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s is gone, so mock %q cannot be read back from it", models.ErrPayloadFileReplaced, s.path, name)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to open the mock file %s to read back mock %q: %w", s.path, name, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat the mock file %s to read back mock %q: %w", s.path, name, err)
	}
	if !os.SameFile(info, s.info) {
		return nil, fmt.Errorf("%w: %s, so mock %q cannot be read back from it", models.ErrPayloadFileReplaced, s.path, name)
	}
	data := make([]byte, n)
	if _, err := f.ReadAt(data, off); err != nil && err != io.EOF {
		return nil, fmt.Errorf("failed to read back mock %q from the mock file %s: %w", name, s.path, err)
	}

	// The read that left the payload logged what it had to say about the
	// document; reading it again says nothing more.
	var mocks []*models.Mock
	if s.format == yaml.FormatJSON {
		var doc yaml.NetworkTrafficDocJSON
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("failed to read back mock %q from the mock file %s: %w", name, s.path, err)
		}
		mocks, err = decodeMocksJSON([]*yaml.NetworkTrafficDocJSON{&doc}, zap.NewNop(), false)
	} else {
		var doc yaml.NetworkTrafficDoc
		if err := yamlLib.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("failed to read back mock %q from the mock file %s: %w", name, s.path, err)
		}
		mocks, err = decodeMocks([]*yaml.NetworkTrafficDoc{&doc}, zap.NewNop(), false)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read back mock %q from the mock file %s: %w", name, s.path, err)
	}
	if index >= len(mocks) || mocks[index].Name != name {
		return nil, fmt.Errorf("the mock file %s changed after its mocks were read: mock %q is no longer where it was read from", s.path, name)
	}
	return mocks[index], nil
}
