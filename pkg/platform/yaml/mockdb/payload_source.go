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
// that one, when it is sent (models.Mock.WithPayload / models.WithPayloads).
//
// It does not hold the file open: the file is opened per read or per batch and
// must still be the one the mocks were read from. A rewrite of the file after a
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
	info        os.FileInfo
	mutate      atomic.Pointer[models.PayloadMutator]
	batchMutate atomic.Pointer[models.BatchPayloadMutator]
	opens       atomic.Int64 // number of os.Open calls performed (diagnostics/tests)
}

var (
	_ models.PayloadSource             = (*payloadSource)(nil)
	_ models.BatchPayloadSource        = (*payloadSource)(nil)
	_ models.BatchPayloadMutatorSource = (*payloadSource)(nil)
)

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

func (s *payloadSource) PayloadMutator() models.PayloadMutator {
	if p := s.mutate.Load(); p != nil {
		return *p
	}
	return nil
}

func (s *payloadSource) SetBatchPayloadMutator(mutate models.BatchPayloadMutator) {
	s.batchMutate.Store(&mutate)
}

func (s *payloadSource) BatchPayloadMutator() models.BatchPayloadMutator {
	if p := s.batchMutate.Load(); p != nil {
		return *p
	}
	return nil
}

// openCount returns how many times this source opened its backing mock file.
func (s *payloadSource) openCount() int64 {
	return s.opens.Load()
}

// openVerified opens s.path once and verifies via os.SameFile that it is still
// the exact file indexed. Caller must close the returned *os.File.
func (s *payloadSource) openVerified(firstMockName string) (*os.File, error) {
	s.opens.Add(1)
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s is gone, so mock %q cannot be read back from it", models.ErrPayloadFileReplaced, s.path, firstMockName)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to open the mock file %s to read back mock %q: %w", s.path, firstMockName, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("failed to stat the mock file %s to read back mock %q: %w", s.path, firstMockName, err)
	}
	if !os.SameFile(info, s.info) {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s, so mock %q cannot be read back from it", models.ErrPayloadFileReplaced, s.path, firstMockName)
	}
	return f, nil
}

// readAndDecodeAt reads the document at from the already-verified open file f
// using scratch (grown as needed) and decodes it into a single *models.Mock.
func (s *payloadSource) readAndDecodeAt(f *os.File, at models.PayloadAt, scratch *[]byte) (*models.Mock, error) {
	if at.Length < 0 {
		return nil, fmt.Errorf("failed to read back mock %q from the mock file %s: negative length %d", at.Name, s.path, at.Length)
	}
	if cap(*scratch) < at.Length {
		*scratch = make([]byte, at.Length)
	}
	data := (*scratch)[:at.Length]
	if n, err := f.ReadAt(data, at.Offset); n != at.Length {
		if err == nil || errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("failed to read back mock %q from the mock file %s: %w", at.Name, s.path, err)
	}

	// The read that left the payload logged what it had to say about the
	// document; reading it again says nothing more.
	var mocks []*models.Mock
	var err error
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

// LoadPayload decodes the document at at again, as the read that left its
// payload decoded it.
func (s *payloadSource) LoadPayload(ctx context.Context, at models.PayloadAt) (*models.Mock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := s.openVerified(at.Name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var scratch []byte
	return s.readAndDecodeAt(f, at, &scratch)
}

// LoadPayloadBatch decodes a batch of documents using a single os.Open, f.Stat,
// os.SameFile check, and f.Close (always closed before returning).
func (s *payloadSource) LoadPayloadBatch(ctx context.Context, ats []models.PayloadAt) ([]*models.Mock, error) {
	if len(ats) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := s.openVerified(ats[0].Name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	out := make([]*models.Mock, len(ats))
	var scratch []byte
	for i, at := range ats {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		m, err := s.readAndDecodeAt(f, at, &scratch)
		if err != nil {
			return nil, err
		}
		out[i] = m
	}
	return out, nil
}
