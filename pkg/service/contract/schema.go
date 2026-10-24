package contract

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"go.keploy.io/server/v3/pkg/models"
	yamlLib "gopkg.in/yaml.v3"
)

func validateSchema(openapi models.OpenAPI) error {
	openapiYAML, err := yamlLib.Marshal(openapi)
	if err != nil {
		return err
	}
	// Validate using kin-openapi
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(openapiYAML)
	if err != nil {
		return err

	}
	// Validate the OpenAPI document
	if err := doc.Validate(context.Background()); err != nil {
		return err
	}

	return nil
}

// setDirs lists the folders in dir that the contract commands read as sets
// or services: each entry that is a directory, or a symbolic link to one,
// among those named (named nil: every entry). The callers only read what
// setDirs lists -- the schemas validate checks, and the sources of a download
// in another service's checkout -- so a link is followed as a set's own
// files already are: the readers of a set follow links, and following a link
// to the set reaches nothing a link to a file inside it did not. A set or
// service shared in by a link is still one to read. An entry that is a link
// keploy cannot follow (its target missing, a loop, one it may not look at)
// is an error, not one to pass over: it may well be a set, and passing over
// it would check nothing against it. A regular file is no set, and is passed
// over as before.
func setDirs(dir string, named func(string) bool) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if named != nil && !named(e.Name()) {
			continue
		}
		if e.IsDir() {
			names = append(names, e.Name())
			continue
		}
		// Anything else but a regular file may be a link: a symbolic link,
		// or on Windows a junction, which the listing reports as irregular.
		if e.Type().IsRegular() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		info, err := os.Stat(path)
		if err != nil {
			if _, lerr := os.Lstat(path); errors.Is(lerr, fs.ErrNotExist) {
				// Gone since the listing: no entry at all.
				continue
			}
			return nil, fmt.Errorf("keploy cannot follow a symbolic link among the folders it reads as sets or services, so it cannot tell whether it is one to read; remove the link, or point it at such a folder: %w", err)
		}
		if info.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// namedLikeATestSet is the name filter the downloads apply to a service's
// keploy folder.
func namedLikeATestSet(name string) bool { return strings.Contains(name, "test") }

// GetAllTestsSchema retrieves all the tests schema from the schema folder.
func (s *contract) GetAllTestsSchema(ctx context.Context) (map[string]map[string]*models.OpenAPI, error) {
	testsFolder := filepath.Join("./keploy", "schema", "tests")

	s.openAPIDB.ChangePath(testsFolder)
	testSetIDs, err := setDirs(testsFolder, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list the tests directory: %w", err)
	}

	testsMapping := make(map[string]map[string]*models.OpenAPI)
	for _, testSetID := range testSetIDs {
		tests, err := s.openAPIDB.GetTestCasesSchema(ctx, testSetID, "")
		if err != nil {
			return nil, fmt.Errorf("failed to get test cases for testSetID %s: %w", testSetID, err)
		}

		testsMapping[testSetID] = make(map[string]*models.OpenAPI)
		for _, test := range tests {
			testsMapping[testSetID][test.Info.Title] = test
		}
	}

	return testsMapping, nil
}

func (s *contract) GetAllDownloadedMocksSchemas(ctx context.Context) ([]models.MockMapping, error) {
	// ***** TODO: See what part can be moved to DB layer *****
	downloadMocksFolder := filepath.Join("./Download", "Mocks")

	// Read the contents of the Download Mocks folder to get all service
	// directories (setDirs: a directory, or a link to one).
	services, err := setDirs(downloadMocksFolder, nil)
	if err != nil {
		// If there's an error listing the directory, return it.
		return nil, fmt.Errorf("failed to list the mocks directory: %w", err)
	}
	var mocksSchemasMapping []models.MockMapping
	// Loop over each service folder in the Download Mocks folder.
	for _, service := range services {
		// Define the path to the service folder (e.g., Download/Mocks/service-name).
		serviceFolder := filepath.Join(downloadMocksFolder, service)

		// Read the contents of the service folder to get mock set IDs (subdirectories).
		mockSetIDs, err := setDirs(serviceFolder, nil)
		if err != nil {
			// If there's an error listing the service folder, return it.
			return nil, fmt.Errorf("failed to list service directory %s: %w", serviceFolder, err)
		}

		// Loop over each mock set ID in the service folder.
		for _, mockSetID := range mockSetIDs {
			// Retrieve the mocks for the given mock set ID (e.g., schema files in the folder).
			mocks, err := s.openAPIDB.GetMocksSchemas(ctx, mockSetID, serviceFolder, "schema")
			if err != nil {
				// If there's an error retrieving mocks, return it.
				return nil, fmt.Errorf("failed to get HTTP mocks for mockSetID %s: %w", mockSetID, err)
			}
			mocksSchemasMapping = append(mocksSchemasMapping, models.MockMapping{
				Service:   service,
				TestSetID: mockSetID,
				Mocks:     mocks,
			})
		}
	}
	return mocksSchemasMapping, nil
}
