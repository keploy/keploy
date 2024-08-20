// Package consumer is a package for consumer driven contract testing
package consumer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
	"go.keploy.io/server/v2/config"
	schemaMatcher "go.keploy.io/server/v2/pkg/matcher/schema"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

const IDENTIFYMODE = 0
const COMPAREMODE = 1

type consumer struct {
	logger    *zap.Logger
	testDB    TestDB
	openAPIDB OpenAPIDB
	config    *config.Config
}

// New creates a new instance of the consumer service
func New(logger *zap.Logger, testDB TestDB, openAPIDB OpenAPIDB, config *config.Config) Service {
	return &consumer{
		logger:    logger,
		testDB:    testDB,
		openAPIDB: openAPIDB,
		config:    config,
	}
}
func (s *consumer) ConsumerDrivenValidation(ctx context.Context) error {
	// Loop over Mocks in DOwnload folder and compare them with the tests in the keploy schema folder
	downloadMocksFolder := filepath.Join("./Download", "Mocks")

	testsFolder := filepath.Join("./keploy", "schema", "tests")

	// Retrieve tests from the schema folder
	testsMapping, err := s.getTestsSchema(ctx, testsFolder)
	if err != nil {
		s.logger.Error("Failed to get test cases from schema", zap.Error(err))
		return err
	}

	// Retrieve mocks and calculate scores for each service
	scores, err := s.getMockScores(ctx, downloadMocksFolder, testsMapping)
	if err != nil {
		return err
	}
	// Compare the scores and generate a summary
	summary, err := s.ValidateMockAgainstTests(scores, testsMapping)
	if err != nil {
		return err
	}
	// Print the summary
	generateSummaryTable(summary)

	return nil
}

// getTestsSchema retrieves all the tests from the schema folder.
func (s *consumer) getTestsSchema(ctx context.Context, testsFolder string) (map[string]map[string]*models.OpenAPI, error) {
	s.openAPIDB.ChangeTcPath(testsFolder)
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

// getMockScores retrieves mocks and compares them with test cases, calculating scores.
func (s *consumer) getMockScores(ctx context.Context, downloadMocksFolder string, testsMapping map[string]map[string]*models.OpenAPI) (map[string]map[string]map[string]models.SchemaInfo, error) {
	// Read the contents of the Download Mocks folder to get all service directories.
	entries, err := os.ReadDir(downloadMocksFolder)
	if err != nil {
		// If there's an error reading the directory, return it.
		return nil, fmt.Errorf("failed to read mocks directory: %w", err)
	}

	// Initialize a map to store the scores for each service, mock set, and mock.
	scores := make(map[string]map[string]map[string]models.SchemaInfo)

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

			// Initialize the service entry in the scores map if it doesn't already exist.
			if scores[entry.Name()] == nil {
				scores[entry.Name()] = make(map[string]map[string]models.SchemaInfo)
			}
			// Loop over each mock set ID in the service folder.
			for _, mockSetID := range mockSetIDs {
				// Ensure the mock set ID is a directory.
				if !mockSetID.IsDir() {
					continue
				}
				// Initialize the mock set entry if it hasn't been initialized yet.
				if scores[entry.Name()][mockSetID.Name()] == nil {
					scores[entry.Name()][mockSetID.Name()] = make(map[string]models.SchemaInfo)
				}
				// Retrieve the mocks for the given mock set ID (e.g., schema files in the folder).
				mocks, err := s.openAPIDB.GetMocksSchemas(ctx, mockSetID.Name(), serviceFolder, "schema")
				if err != nil {
					// If there's an error retrieving mocks, return it.
					return nil, fmt.Errorf("failed to get HTTP mocks for mockSetID %s: %w", mockSetID.Name(), err)
				}

				// Compare the mocks with test cases and calculate scores.
				// The result is stored in the scores map under the respective service and mock set ID.
				s.scoresForMocks(mocks, scores[entry.Name()][mockSetID.Name()], testsMapping, mockSetID.Name())
			}
		}
	}
	// Return the calculated scores.
	return scores, nil
}

// scoresForMocks compares mocks to test cases and assigns scores.
func (s *consumer) scoresForMocks(mocks []*models.OpenAPI, mockSet map[string]models.SchemaInfo, testsMapping map[string]map[string]*models.OpenAPI, mockSetID string) {
	// Ensure mockSet is initialized before assigning
	if mockSet == nil {
		mockSet = make(map[string]models.SchemaInfo)
	}
	// Loop through each mock in the provided list of mocks.
	for _, mock := range mocks {
		// Initialize the mock's score to 0.0 and store the mock's data in the mockSet map.
		// 'mockSet' is a map where the key is the mock title and the value is the SchemaInfo structure containing score and data.
		mockSet[mock.Info.Title] = models.SchemaInfo{
			Score: 0.0,
			Data:  *mock, // Store the mock data here.
		}

		// Loop through each test set (testSetID) in the testsMapping.
		// testsMapping maps test set IDs to test case titles.
		for testSetID, tests := range testsMapping {
			// Loop through each test in the current test set.
			for _, test := range tests {
				// Call 'match2' to compare the mock with the current test.
				// This function returns a candidateScore (how well the mock matches the test) and a pass boolean.
				candidateScore, pass, err := schemaMatcher.Match(*mock, *test, testSetID, mockSetID, s.logger, IDENTIFYMODE)
				// Handle any errors encountered during the comparison process.
				if err != nil {
					// Log the error and continue with the next iteration, skipping the current comparison.
					s.logger.Error("Error in matching the two models", zap.Error(err))
					continue
				}

				// If the mock passed the comparison and the candidate score is greater than the current score:
				if pass && candidateScore > mockSet[mock.Info.Title].Score {
					// Update the mock's score and store the test case information in the mockSet.
					// This keeps track of the best matching test case for the current mock.
					mockSet[mock.Info.Title] = models.SchemaInfo{
						Service:   "",              // Optional: could store service info if needed.
						TestSetID: testSetID,       // Store the test set ID that provided the highest score.
						Name:      test.Info.Title, // Store the test case name (title).
						Score:     candidateScore,  // Update the score with the highest candidate score.
						Data:      *mock,           // Store the mock data.
					}
				}
			}
		}
	}
}

// ValidateMockAgainstTests compares mock results with test cases and generates a summary report
func (s *consumer) ValidateMockAgainstTests(scores map[string]map[string]map[string]models.SchemaInfo, testsMapping map[string]map[string]*models.OpenAPI) (models.Summary, error) {
	var summary models.Summary

	// Defining color schemes for success, failure, and other statuses
	notMatchedColor := color.New(color.FgHiRed).SprintFunc()
	missedColor := color.New(color.FgHiYellow).SprintFunc()
	successColor := color.New(color.FgHiGreen).SprintFunc()
	serviceColor := color.New(color.FgHiBlue).SprintFunc()

	// Loop through the services in the scores map
	// Each "service" represents a consumer service being validated
	for service, mockSetIDs := range scores {
		// Create a new service summary for each service
		var serviceSummary models.ServiceSummary
		serviceSummary.TestSets = make(map[string]models.Status)
		serviceSummary.Service = service // Store the service name

		// Output the beginning of the validation for the current service
		fmt.Println("==========================================")
		fmt.Print("Starting Validation for Consumer Service: ")
		fmt.Print(serviceColor(service)) // Print service name in blue
		fmt.Println(" ....")
		fmt.Println("==========================================")

		// Iterate over the mockSetIDs for each service (mock set contains multiple mocks)
		for mockSetID, mockTest := range mockSetIDs {
			if _, ok := serviceSummary.TestSets[mockSetID]; !ok {
				// Initialize the Status struct if it doesn't already exist for the mockSetID
				serviceSummary.TestSets[mockSetID] = models.Status{}
			}

			// Iterate over each mock in the mockTest map
			for _, mockInfo := range mockTest {

				// Print validation information only if the score is not zero
				if mockInfo.Score != 0.0 {
					fmt.Print("Validating '")
					fmt.Print(serviceColor(service)) // Print the service name in blue
					fmt.Printf("': (%s)/%s for (%s)/%s\n", mockSetID, mockInfo.Data.Info.Title, mockInfo.TestSetID, mockInfo.Name)

				}

				// Case 1: If the score is 1.0, the mock passed the validation
				if mockInfo.Score == 1.0 {
					// Retrieve the Status struct for the given mockSetID
					status := serviceSummary.TestSets[mockSetID]

					// Append the passed mock title
					status.Passed = append(status.Passed, mockInfo.Data.Info.Title)

					// Reassign the updated status back to the map
					serviceSummary.TestSets[mockSetID] = status
					serviceSummary.PassedCount++ // Increment the passed count

					// Print a success message in green
					fmt.Print("Contract check ")
					fmt.Print(successColor("passed")) // Print "passed" in green
					fmt.Printf(" for the test '%s' / mock '%s'\n", mockInfo.Name, mockInfo.Data.Info.Title)
					fmt.Println("--------------------------------------------------------------------")
					// Case 2: If the score is between 0 and 1.0, the mock failed the validation
				} else if mockInfo.Score > 0.0 {
					// Retrieve the Status struct for the given mockSetID
					status := serviceSummary.TestSets[mockSetID]

					// Append the failed mock title
					status.Failed = append(status.Failed, mockInfo.Data.Info.Title)

					// Reassign the updated status back to the map
					serviceSummary.TestSets[mockSetID] = status
					serviceSummary.FailedCount++ // Increment the failed count

					// Print a failure message in red
					fmt.Print("Contract check")
					fmt.Print(notMatchedColor(" failed")) // Print "failed" in red
					fmt.Printf(" for the test '%s' / mock '%s'\n", mockInfo.Name, mockInfo.Data.Info.Title)

					fmt.Println()

					// Additional information: Print consumer and current service comparison
					fmt.Printf("                                    Current %s   ||   Consumer %s\n", serviceColor(s.config.Contract.Self), serviceColor(service))

					// Perform comparison between the mock and test case again
					_, _, err := schemaMatcher.Match(mockInfo.Data, *testsMapping[mockInfo.TestSetID][mockInfo.Name], mockInfo.TestSetID, mockSetID, s.logger, COMPAREMODE)
					if err != nil {
						// If an error occurs during comparison, return it
						s.logger.Error("Error in matching the two models", zap.Error(err))
						return models.Summary{}, err
					}

					// Case 3: If the score is 0.0, there was no matching test case found
				} else if mockInfo.Score == 0.0 {
					// Retrieve the Status struct for the given mockSetID
					status := serviceSummary.TestSets[mockSetID]

					// Append the missed mock title
					status.Missed = append(status.Missed, mockInfo.Data.Info.Title)

					// Reassign the updated status back to the map
					serviceSummary.TestSets[mockSetID] = status
					serviceSummary.MissedCount++ // Increment the missed count

					// Print a "missed" message in yellow
					fmt.Println(missedColor(fmt.Sprintf("No ideal test case found for the (%s)/'%s'", mockSetID, mockInfo.Data.Info.Title)))
					fmt.Println("--------------------------------------------------------------------")
				}
			}
		}

		// Append the completed service summary to the overall summary
		summary.ServicesSummary = append(summary.ServicesSummary, serviceSummary)
	}

	// Return the overall summary containing details of all services validated
	return summary, nil
}

func generateSummaryTable(summary models.Summary) {
	notMatchedColor := color.New(color.FgHiRed).SprintFunc()
	missedColor := color.New(color.FgHiYellow).SprintFunc()
	successColor := color.New(color.FgHiGreen).SprintFunc()
	serviceColor := color.New(color.FgHiBlue).SprintFunc()

	// Create a new tablewriter to format the output as a table
	table := tablewriter.NewWriter(os.Stdout)

	// Set table headers
	table.SetHeader([]string{"Consumer Service", "Consumer Service Test-set", "Mock-name", "Failed", "Passed", "Missed"})
	table.SetAlignment(tablewriter.ALIGN_CENTER)
	table.SetAutoMergeCells(true)
	// Loop through each service summary to populate the table
	for idx, serviceSummary := range summary.ServicesSummary {
		failedCount := serviceSummary.FailedCount
		passedCount := serviceSummary.PassedCount
		missedCount := serviceSummary.MissedCount
		table.Append([]string{
			serviceColor(serviceSummary.Service),
			"",
			"",
			notMatchedColor(failedCount),
			successColor(passedCount),
			missedColor(missedCount),
		})
		for testSet, status := range serviceSummary.TestSets {
			for _, mock := range status.Failed {
				// Add rows for failed mocks
				table.Append([]string{
					"",
					testSet,
					notMatchedColor(mock),
					"",
					"", "",
				})
			}

			for _, mock := range status.Missed {
				table.Append([]string{
					"",
					testSet,
					missedColor(mock), "",
					"", "",
				})
			}
			table.Append([]string{
				"",
				"",
				"", "",
				"", "",
			})
		}
		// Add a blank line (or border) after each service
		if idx < len(summary.ServicesSummary)-1 {
			table.Append([]string{"----------------", "----------------", "----------------", "----------------", "----------------", "----------------"})
		}
	}

	// Render the table to stdout
	table.Render()
}
