package mockdb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// The learn step's legible files (word-level learn): what the set's mocks
// learned is derived from the mocks (learn.Legible), which this store reads
// for it (ReadSetMocks), whatever the format they are in.

func init() {
	learn.RegisterSetReader(func(setDir string) ([]*models.Mock, error) {
		return ReadSetMocks(context.Background(), zap.NewNop(), setDir)
	})
}

// ReadSetMocks reads every mock of the test set in setDir (keploy/<set>/, or
// a staging copy of it), from its mocks file in whichever format it is in
// (gob, YAML or JSON), each as the store decodes it: noise.words and every
// variable's words included, whatever the switch says. A set with no mocks
// file has none.
func ReadSetMocks(ctx context.Context, logger *zap.Logger, setDir string) ([]*models.Mock, error) {
	const name = "mocks"
	var out []*models.Mock
	gobPath := filepath.Join(setDir, name+".gob")
	if _, err := os.Stat(gobPath); err == nil {
		err := forEachGobMock(gobPath, func(m *models.Mock) error {
			out = append(out, m)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", gobPath, err)
		}
		return out, nil
	}
	exists, format, err := yaml.FileExistsAny(ctx, logger, setDir, name, yaml.FormatYAML)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	reader, err := yaml.NewMockReaderF(ctx, logger, setDir, name, format)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	for {
		doc, readErr, decodeErr := nextDoc(reader, logger, true)
		if errors.Is(readErr, io.EOF) {
			return out, nil
		}
		if readErr != nil {
			return nil, fmt.Errorf("read the mocks of %s: %w", setDir, readErr)
		}
		if decodeErr != nil {
			return nil, fmt.Errorf("decode the mocks of %s: %w", setDir, decodeErr)
		}
		out = append(out, doc.mocks...)
	}
}

// WriteLearnedFiles writes, under the proven switch, the learned files of
// the test set testSetID (learn.WriteLearned): learned.yaml from the set's
// mocks, and learned.local.yaml with record's full round's values the
// consumed mocks' states carry (MockState.LearnedF), once the project's
// keploy/.gitignore ignores both. A set that learned nothing gets neither.
// With the switch off it writes nothing.
func (ys *MockYaml) WriteLearnedFiles(testSetID string, mockStates map[string]models.MockState) error {
	if !provenswitch.On() {
		return nil
	}
	var local []learn.LocalValue
	for _, st := range mockStates {
		for mock, places := range st.LearnedF {
			for path, f := range places {
				local = append(local, learn.LocalValue{Mock: mock, Path: path, F: f})
			}
		}
	}
	return learn.WriteLearned(filepath.Join(ys.MockPath, testSetID), local)
}
