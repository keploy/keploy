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

// payloadSource is a yaml or json mock file that a read left its mocks'
// payloads in (IndexTestSetMocks). Each mock keeps where in the file the
// document it was decoded from is, and reads that document again, and only
// that one, when it is sent (models.Mock.WithPayload).
//
// It does not hold the file open: the file is opened for each read and must
// still be the one the mocks were read from. A rewrite of the file after a
// replay (the prune, the noise write-back) puts a new file in its place, which
// a read then refuses rather than take a payload from another document; and a
// file held open could not be replaced on every platform. A file only appended
// to since (a live recording's) is the same file, its documents where they
// were.
type payloadSource struct {
	path   string
	format yaml.Format
	// info is the file the mocks were read from, told apart from a file put
	// in its place since by os.SameFile.
	info   os.FileInfo
	mutate atomic.Pointer[models.PayloadMutator]
	// spill holds the mocks as the read decoded them, so most are read back
	// without decoding the file again (see payloadSpill). nil when none
	// could be made.
	spill *payloadSpill
}

var _ models.PayloadSource = (*payloadSource)(nil)

func newPayloadSource(reader *yaml.MockReader) (*payloadSource, error) {
	info, err := reader.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat the mock file %s: %w", reader.Path(), err)
	}
	s := &payloadSource{path: reader.Path(), format: reader.Format(), info: info}
	s.spill = newPayloadSpill(s, s.path, info.Size())
	return s, nil
}

func (s *payloadSource) SetPayloadMutator(mutate models.PayloadMutator) {
	s.mutate.Store(&mutate)
}

func (s *payloadSource) PayloadMutator() models.PayloadMutator {
	if p := s.mutate.Load(); p != nil {
		return *p
	}
	return nil
}

// LoadPayload reads back the mock of the document at at, as the read that left
// its payload decoded it: from the spill when it holds the mock of that very
// document (the file's bytes there are still the ones decoded), else by
// decoding the document again.
func (s *payloadSource) LoadPayload(ctx context.Context, at models.PayloadAt) (*models.Mock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s is gone, so mock %q cannot be read back from it", models.ErrPayloadFileReplaced, s.path, at.Name)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to open the mock file %s to read back mock %q: %w", s.path, at.Name, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat the mock file %s to read back mock %q: %w", s.path, at.Name, err)
	}
	if !os.SameFile(info, s.info) {
		return nil, fmt.Errorf("%w: %s, so mock %q cannot be read back from it", models.ErrPayloadFileReplaced, s.path, at.Name)
	}
	data := make([]byte, at.Length)
	if n, err := f.ReadAt(data, at.Offset); n != at.Length {
		if err == nil || errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("failed to read back mock %q from the mock file %s: %w", at.Name, s.path, err)
	}
	if mock, ok := s.spill.load(at, data); ok {
		return mock, nil
	}

	// The read that left the payload logged what it had to say about the
	// document; reading it again says nothing more.
	var mocks []*models.Mock
	if s.format == yaml.FormatJSON {
		var doc yaml.NetworkTrafficDocJSON
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("failed to read back mock %q from the mock file %s: %w", at.Name, s.path, err)
		}
		mocks, err = decodeMocksJSON([]*yaml.NetworkTrafficDocJSON{&doc}, zap.NewNop(), false)
	} else {
		var doc yaml.NetworkTrafficDoc
		if err := yamlLib.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("failed to read back mock %q from the mock file %s: %w", at.Name, s.path, err)
		}
		mocks, err = decodeMocks([]*yaml.NetworkTrafficDoc{&doc}, zap.NewNop(), false)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read back mock %q from the mock file %s: %w", at.Name, s.path, err)
	}
	if len(mocks) != 1 || mocks[0].Name != at.Name {
		return nil, fmt.Errorf("the mock file %s changed in place after its mocks were read: mock %q is no longer where it was read from", s.path, at.Name)
	}
	return mocks[0], nil
}
