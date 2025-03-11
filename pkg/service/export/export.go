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
	"sort"
	"strings"

	"github.com/google/uuid"
	yamlLib "gopkg.in/yaml.v3"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	postmanimport "go.keploy.io/server/v2/pkg/service/import"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func ConvertKeployHTTPToPostmanCollection(logger *zap.Logger, http *models.HTTPSchema) map[string]interface{} {
	var request postmanimport.PostmanRequest
	var response postmanimport.PostmanResponse

	// Extract URL from the HTTP schema
	extractedURL := http.Request.URL

	parsedURL, err := url.Parse(extractedURL)
	if err != nil || parsedURL.Hostname() == "" {
		utils.LogError(logger, err, "error parsing URL")
		return nil
	}

	request.URL = map[string]interface{}{
		"raw":      parsedURL.String(),
		"protocol": parsedURL.Scheme,
		"host":     []string{parsedURL.Hostname()},
		"port":     parsedURL.Port(),
		"path":     []string{strings.TrimLeft(parsedURL.Path, "/")},
		"query":    http.Request.URLParams,
	}
	request.Method = string(http.Request.Method)

	for key, header := range http.Request.Header {
		request.Header = append(request.Header, map[string]interface{}{
			"key":   key,
			"value": header,
		})
	}

	if http.Request.Form != nil {
		formDataArray := []map[string]interface{}{}
		for _, form := range http.Request.Form {
			formDataArray = append(formDataArray, map[string]interface{}{
				"key":    form.Key,
				"values": form.Values,
			})
		}

		request.Body.Mode = "formdata"
		request.Body.Formdata = formDataArray
	} else {
		request.Body.Mode = "raw"
		request.Body.Raw = http.Request.Body
	}

	if strings.Contains(http.Request.Header["Content-Type"], "application/json") {
		request.Body.Options = map[string]interface{}{
			"raw": map[string]interface{}{
				"headerFamily": "json",
				"language":     "json",
			},
		}
	}

	// Extract Response Headers
	for key, header := range http.Response.Header {
		response.Header = append(response.Header, map[string]string{
			"key":   key,
			"value": header,
		})
	}

	response.Code = http.Response.StatusCode
	response.Status = http.Response.StatusMessage
	response.Body = http.Response.Body
	response.OriginalRequest = &request
	response.Name = http.Response.StatusMessage

	if strings.Contains(http.Response.Header["Content-Type"], "application/json") {
		request.Body.Options = map[string]interface{}{
			"raw": map[string]interface{}{
				"headerFamily": "json",
				"language":     "json",
			},
		}
	}

	// Extract the last segment of the path as the name
	pathSegments := strings.Split(strings.Trim(parsedURL.Path, "/"), "/")
	// Create the name by joining segments with dashes
	name := strings.Join(pathSegments, "-")

	return map[string]interface{}{
		"name": name,
		"protocolProfileBehavior": map[string]interface{}{
			"disableBodyPruning": true,
		},
		"request":  request,
		"response": []postmanimport.PostmanResponse{response},
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
				logger.Info("No tests found. Skipping export.", zap.String("path", testsDir))
				continue
			}
			// Read the "tests" subfolder
			testFiles, err := os.ReadDir(testsDir)
			if err != nil {
				utils.LogError(logger, err, "failed to read the test files", zap.String("path", testsDir))
				continue
			}
			keployRequests := make(map[interface{}]int, 0)
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

					var httpSchema models.HTTPSchema

					err = testCase.Spec.Decode(&httpSchema)
					if err != nil {
						utils.LogError(logger, err, "failed to decode the HTTP schema")
						continue
					}

					requestJSON := ConvertKeployHTTPToPostmanCollection(logger, &httpSchema)
					// Convert the requestJSON to a string (assuming it's a map or complex type)
					requestJSONString, err := json.Marshal(requestJSON)
					if err != nil {
						utils.LogError(logger, err, "failed to marshal requestJSON to string")
						continue
					}
					keployRequests[string(requestJSONString)]++
				}
			}
			var uniqueRequests []interface{}
			for request := range keployRequests {
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

	fmt.Println("✅ Curls successfully exported to output.json 🎉")

	return nil
}
