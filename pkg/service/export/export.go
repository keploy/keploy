// Package export contains the implementation of the export service which exports the curl commands from the YAML testcases to a Postman collection.
package export

import (
	"bytes"
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
	curlCommand = strings.Replace(curlCommand, "\n", " ", -1)
	// Regular expressions to capture parts of the curl command
	reMethodAndURL := regexp.MustCompile(`--request\s+(\w+)\s+--url\s+([^ ]+)`)
	reHeader := regexp.MustCompile(`--header '([^:]+): ([^']*)'`)
	reData := regexp.MustCompile(`--data(?:-raw)?\s+['"]([\s\S]*?)['"](?:\s+--|\s*$)`)

	reFormData := regexp.MustCompile(`--form\s+['"]([^=]+)=([^'"]+)['"]`)

	// Extract method and URL
	matches := reMethodAndURL.FindStringSubmatch(curlCommand)
	method, extractedURL := "GET", ""
	if len(matches) > 2 {
		method = matches[1]
		extractedURL = matches[2]
	}

	parsedURL, err := url.Parse(extractedURL)
	if err != nil || parsedURL.Hostname() == "" {
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
		rawData = dataMatch[1]

		// Check if rawData needs unquoting (e.g., it starts and ends with quotes)
		if (strings.HasPrefix(rawData, `"`) && strings.HasSuffix(rawData, `"`)) ||
			(strings.HasPrefix(rawData, `'`) && strings.HasSuffix(rawData, `'`)) {
			unquotedData, err := strconv.Unquote(rawData)
			if err != nil {
				utils.LogError(logger, err, "error unquoting data")
				return nil
			}
			rawData = unquotedData
		}
		// Determine if it's JSON or URL-encoded form data
		if strings.HasPrefix(rawData, "{") && strings.HasSuffix(rawData, "}") {
			// Try to parse as JSON
			rawData = strings.ReplaceAll(rawData, `\n`, "\n")
			rawData = strings.ReplaceAll(rawData, `\"`, `"`)

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

	// Extract query parameters from the URL
	queryParams := []map[string]string{}
	for key, values := range parsedURL.Query() {
		for _, value := range values {
			queryParams = append(queryParams, map[string]string{
				"key":   key,
				"value": value,
			})
		}
	}
	// Manually construct the raw URL to avoid escaping
	rawURL := parsedURL.Scheme + "://" + parsedURL.Host + parsedURL.Path
	if len(parsedURL.RawQuery) > 0 {
		rawURL += "?" + parsedURL.RawQuery
	}

	// Extract the last segment of the path as the name
	pathSegments := strings.Split(strings.Trim(parsedURL.Path, "/"), "/")
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
				"raw":      rawURL, // Use manually constructed raw URL
				"protocol": parsedURL.Scheme,
				"host":     []string{parsedURL.Hostname()},
				"port":     parsedURL.Port(),
				"path":     []string{strings.TrimLeft(parsedURL.Path, "/")},
				"query":    queryParams,
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

func Export(_ context.Context, logger *zap.Logger) error {
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
	folderName := filepath.Base(cwd)

	collection := PostmanCollection{
		Info: struct {
			PostmanID string `json:"_postman_id"`
			Name      string `json:"name"`
			Schema    string `json:"schema"`
		}{
			PostmanID: uuid.New().String(),
			Name:      folderName,
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
						curlRequests[string(requestJSONString)]++
					}
				}
			}
			var uniqueRequests []interface{}
			for request := range curlRequests {
				var curlRequest interface{}
				err := json.Unmarshal([]byte(request.(string)), &curlRequest)
				if err != nil {
					utils.LogError(logger, err, "failed to unmarshal the request JSON")
					continue
				}
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

	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false) // Disable HTML escaping
	encoder.SetIndent("", "    ")

	err = encoder.Encode(collection)
	if err != nil {
		utils.LogError(logger, err, "failed to encode the Postman collection")
		return err
	}

	outputData := buf.Bytes()

	if err := os.WriteFile("output.json", outputData, 0644); err != nil {
		utils.LogError(logger, err, "failed to write the output JSON file")
		return err
	}

	fmt.Println("Curls exported to output.json")

	return nil
}
