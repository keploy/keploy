package contract

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/getkin/kin-openapi/openapi3"
	"go.keploy.io/server/v2/pkg/models"
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

// GetAllTestsSchema retrieves all the tests schema from the schema folder.
func (s *contract) GetAllTestsSchema(ctx context.Context) (map[string]map[string]*models.OpenAPI, error) {
	testsFolder := filepath.Join("./keploy", "schema", "tests")

	s.openAPIDB.ChangePath(testsFolder)
	testSetIDs, err := os.ReadDir(testsFolder)
	if err != nil {
		return nil, fmt.Errorf("failed to read tests directory: %w", err)
	}

	testsMapping := make(map[string]map[string]*models.OpenAPI)
	for _, testSetID := range testSetIDs {
		if !testSetID.IsDir() {
			continue
		}

		tests, err := s.openAPIDB.GetTestCasesSchema(ctx, testSetID.Name(), "")
		if err != nil {
			return nil, fmt.Errorf("failed to get test cases for testSetID %s: %w", testSetID.Name(), err)
		}

		testsMapping[testSetID.Name()] = make(map[string]*models.OpenAPI)
		for _, test := range tests {
			testsMapping[testSetID.Name()][test.Info.Title] = test
		}
	}

	return testsMapping, nil
}

func (s *contract) GetAllDownloadedMocksSchemas(ctx context.Context) ([]models.MockMapping, error) {
	// ***** TODO: See what part can be moved to DB layer *****
	downloadMocksFolder := filepath.Join("./Download", "Mocks")

	// Read the contents of the Download Mocks folder to get all service directories.
	entries, err := os.ReadDir(downloadMocksFolder)
	if err != nil {
		// If there's an error reading the directory, return it.
		return nil, fmt.Errorf("failed to read mocks directory: %w", err)
	}
	var mocksSchemasMapping []models.MockMapping
	// Loop over each entry in the Download Mocks folder.
	for _, entry := range entries {
		// Check if the entry is a directory (indicating a service folder).
		if entry.IsDir() {
			// Define the path to the service folder (e.g., Download/Mocks/service-name).
			serviceFolder := filepath.Join(downloadMocksFolder, entry.Name())

			// Read the contents of the service folder to get mock set IDs (subdirectories).
			mockSetIDs, err := os.ReadDir(serviceFolder)
			if err != nil {
				// If there's an error reading the service folder, return it.
				return nil, fmt.Errorf("failed to read service directory %s: %w", serviceFolder, err)
			}

			// Loop over each mock set ID in the service folder.
			for _, mockSetID := range mockSetIDs {
				// Ensure the mock set ID is a directory.
				if !mockSetID.IsDir() {
					continue
				}

				// Retrieve the mocks for the given mock set ID (e.g., schema files in the folder).
				mocks, err := s.openAPIDB.GetMocksSchemas(ctx, mockSetID.Name(), serviceFolder, "schema")
				if err != nil {
					// If there's an error retrieving mocks, return it.
					return nil, fmt.Errorf("failed to get HTTP mocks for mockSetID %s: %w", mockSetID.Name(), err)
				}
				mocksSchemasMapping = append(mocksSchemasMapping, models.MockMapping{
					Service:   entry.Name(),
					TestSetID: mockSetID.Name(),
					Mocks:     mocks,
				})
			}
		}
	}
	return mocksSchemasMapping, nil
}
