package export

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
	yamlLib "gopkg.in/yaml.v3"

	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func parseCurlCommand(logger *zap.Logger, curlCommand string) map[string]interface{} {
	// Normalize the curl command by removing newlines and backslashes for easier processing
	curlCommand = strings.Replace(curlCommand, "\\\n", " ", -1)

	// Regular expressions to capture parts of the curl command
	reMethodAndUrl := regexp.MustCompile(`--request\s+(\w+)\s+--url\s+([^ ]+)`)
	reHeader := regexp.MustCompile(`--header '([^:]+): ([^']*)'`)
	reData := regexp.MustCompile(`--data(?:-raw)?\s+['"]([^'"]+)['"]`)
	reFormData := regexp.MustCompile(`--form\s+['"]([^=]+)=([^'"]+)['"]`)

	// Extract method and URL
	matches := reMethodAndUrl.FindStringSubmatch(curlCommand)
	method, extractedUrl := "GET", ""
	if len(matches) > 2 {
		method = matches[1]
		extractedUrl = matches[2]
	}

	parsedUrl, err := url.Parse(extractedUrl)
	if err != nil || parsedUrl.Hostname() == "" {
		utils.LogError(logger, err, "error parsing URL")
		return nil
	}

	// Extract headers
	headers := []map[string]string{}
	for _, match := range reHeader.FindAllStringSubmatch(curlCommand, -1) {
		headers = append(headers, map[string]string{
			"key":   match[1],
			"value": match[2],
		})
	}

	sort.SliceStable(headers, func(i, j int) bool {
		return headers[i]["key"] < headers[j]["key"]
	})
	// Extract data (JSON body or URL-encoded form data)
	var jsonData map[string]interface{}
	var formData url.Values // url.Values is a map[string][]string
	rawData := ""
	dataMatch := reData.FindStringSubmatch(curlCommand)
	if len(dataMatch) > 1 {
		rawData, err = strconv.Unquote(`"` + dataMatch[1] + `"`) // Unquote to remove escape sequences
		if err != nil {
			utils.LogError(logger, err, "error unquoting data")
			return nil
		}
		// Determine if it's JSON or URL-encoded form data
		if strings.HasPrefix(rawData, "{") && strings.HasSuffix(rawData, "}") {
			// Try to parse as JSON
			if err := json.Unmarshal([]byte(rawData), &jsonData); err != nil {
				utils.LogError(logger, err, "error parsing JSON data")
				return nil
			}
		} else {
			// Parse as URL-encoded form data
			formData, err = url.ParseQuery(rawData)
			if err != nil {
				utils.LogError(logger, err, "error parsing URL-encoded data")
				return nil
			}
		}
	}

	// Extract form data from --form flag
	formDataFromCurl := map[string]string{}
	for _, match := range reFormData.FindAllStringSubmatch(curlCommand, -1) {
		formDataFromCurl[match[1]] = match[2]
	}

	// Determine the body mode
	bodyMode := "raw"
	if len(formDataFromCurl) > 0 {
		bodyMode = "formdata"
	} else if len(formData) > 0 {
		bodyMode = "urlencoded"
	}

	// Construct the body based on the detected mode
	var body map[string]interface{}
	if bodyMode == "raw" && rawData != "" {
		body = map[string]interface{}{
			"mode": "raw",
			"raw":  rawData,
		}
	} else if bodyMode == "formdata" {
		formDataArray := []map[string]interface{}{}
		for key, value := range formDataFromCurl {
			formDataArray = append(formDataArray, map[string]interface{}{
				"key":   key,
				"value": value,
			})
		}
		body = map[string]interface{}{
			"mode":     "formdata",
			"formdata": formDataArray,
		}
	} else if bodyMode == "urlencoded" {
		urlencodedArray := []map[string]interface{}{}
		for key, values := range formData {
			for _, value := range values {
				urlencodedArray = append(urlencodedArray, map[string]interface{}{
					"key":   key,
					"value": value,
				})
			}
		}
		body = map[string]interface{}{
			"mode":       "urlencoded",
			"urlencoded": urlencodedArray,
		}
	}

	// Extract the last segment of the path as the name
	pathSegments := strings.Split(strings.Trim(parsedUrl.Path, "/"), "/")
	// Create the name by joining segments with dashes
	name := strings.Join(pathSegments, "-")

	// Constructing the response
	return map[string]interface{}{
		"name": name,
		"protocolProfileBehavior": map[string]interface{}{
			"disableBodyPruning": true,
		},
		"request": map[string]interface{}{
			"method": method,
			"header": headers,
			"body":   body,
			"url": map[string]interface{}{
				"raw":      parsedUrl.String(),
				"protocol": parsedUrl.Scheme,
				"host":     []string{parsedUrl.Hostname()},
				"port":     parsedUrl.Port(),
				"path":     []string{strings.TrimLeft(parsedUrl.Path, "/")},
				"query":    parsedUrl.Query(),
			},
		},
		"response": []interface{}{},
	}
}

type PostmanCollection struct {
	Info struct {
		PostmanID string `json:"_postman_id"`
		Name      string `json:"name"`
		Schema    string `json:"schema"`
	} `json:"info"`
	Items []interface{} `json:"item"`
}

func Export(logger *zap.Logger, ctx context.Context) error {
	cwd, err := os.Getwd()
	if err != nil {
		utils.LogError(logger, err, "failed to get current working directory")
		return err
	}
	// Correctly format the directory path to include "keploy"
	keployDir := filepath.Join(cwd, "keploy")

	// Check if the directory exists
	if _, err := os.Stat(keployDir); os.IsNotExist(err) {
		utils.LogError(logger, err, "keploy directory does not exist")
		return err

	}
	dir, err := yaml.ReadDir(keployDir, fs.FileMode(os.O_RDONLY))
	if err != nil {
		utils.LogError(logger, err, "failed to open the directory containing yaml testcases", zap.Any("path", keployDir))
		return err
	}

	files, err := dir.ReadDir(0)
	if err != nil {
		utils.LogError(logger, err, "failed to read the file names of yaml testcases", zap.Any("path", keployDir))
		return err

	}
	collection := PostmanCollection{
		Info: struct {
			PostmanID string `json:"_postman_id"`
			Name      string `json:"name"`
			Schema    string `json:"schema"`
		}{
			PostmanID: uuid.New().String(),
			Name:      os.Getenv("KEPLOY_PROJECT_NAME"),
			Schema:    "https://schema.getpostman.com/json/collection/v2.0.0/collection.json",
		},
	}
	for _, v := range files {
		if v.Name() != "reports" && v.Name() != "testReports" && v.IsDir() {
			testsDir := filepath.Join(keployDir, v.Name(), "tests")
			if _, err := os.Stat(testsDir); os.IsNotExist(err) {
				utils.LogError(logger, err, "tests directory does not exist", zap.String("path", testsDir))
				continue
			}
			// Read the "tests" subfolder
			testFiles, err := os.ReadDir(testsDir)
			if err != nil {
				utils.LogError(logger, err, "failed to read the test files", zap.String("path", testsDir))
				continue
			}
			curlRequests := make(map[interface{}]int, 0)
			for _, testFile := range testFiles {
				if filepath.Ext(testFile.Name()) == ".yaml" {
					filePath := filepath.Join(testsDir, testFile.Name())

					// Read the YAML file
					data, err := os.ReadFile(filePath)
					if err != nil {
						utils.LogError(logger, err, "failed to read the YAML file", zap.String("path", filePath))
						continue
					}

					var testCase *yaml.NetworkTrafficDoc
					err = yamlLib.Unmarshal(data, &testCase)
					if err != nil {
						utils.LogError(logger, err, "failed to unmarshall YAML data")
						continue
					}

					if testCase.Curl != "" {
						requestJSON := parseCurlCommand(logger, testCase.Curl)
						// Convert the requestJSON to a string (assuming it's a map or complex type)
						requestJSONString, err := json.Marshal(requestJSON)
						if err != nil {
							utils.LogError(logger, err, "failed to marshal requestJSON to string")
							continue
						}
						curlRequests[string(requestJSONString)] += 1
					}
				}
			}
			var uniqueRequests []interface{}
			for request := range curlRequests {
				var curlRequest interface{}
				json.Unmarshal([]byte(request.(string)), &curlRequest)
				uniqueRequests = append(uniqueRequests, curlRequest)
			}
			requestFile := map[string]interface{}{
				"name": v.Name(),
				"item": uniqueRequests,
			}
			collection.Items = append(collection.Items, requestFile)
		}
	}
	sort.SliceStable(collection.Items, func(i, j int) bool {
		return collection.Items[i].(map[string]interface{})["name"].(string) < collection.Items[j].(map[string]interface{})["name"].(string)
	})
	outputData, err := json.MarshalIndent(collection, "", "    ")
	if err != nil {
		utils.LogError(logger, err, "failed to marshal the Postman collection")
		return err
	}

	if err := os.WriteFile("output.json", outputData, 0644); err != nil {
		utils.LogError(logger, err, "failed to write the output JSON file")
		return err
	}

	fmt.Println("Curls exported to output.json")

	return nil
}
