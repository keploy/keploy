package yaml

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// NetworkTrafficDoc stores the request-response data of a network call (ingress or egress)
type NetworkTrafficDoc struct {
	Version      models.Version `json:"version" yaml:"version"`
	Kind         models.Kind    `json:"kind" yaml:"kind"`
	Name         string         `json:"name" yaml:"name"`
	Spec         yamlLib.Node   `json:"spec" yaml:"spec"`
	Curl         string         `json:"curl" yaml:"curl,omitempty"`
	ConnectionID string         `json:"connectionId" yaml:"connectionId,omitempty"`
}

// ctxReader wraps an io.Reader with a context for cancellation support
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (cr *ctxReader) Read(p []byte) (n int, err error) {
	select {
	case <-cr.ctx.Done():
		return 0, cr.ctx.Err()
	default:
		return cr.r.Read(p)
	}
}

// ctxWriter wraps an io.Writer with a context for cancellation support
type ctxWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (cw *ctxWriter) Write(p []byte) (n int, err error) {
	for len(p) > 0 {
		var written int
		written, err = cw.writer.Write(p)
		n += written
		if err != nil {
			return n, err
		}
		p = p[written:]
	}
	return n, nil
}

func WriteFile(ctx context.Context, logger *zap.Logger, path, fileName string, docData []byte, isAppend bool) error {
	isFileEmpty, err := CreateYamlFile(ctx, logger, path, fileName)
	if err != nil {
		return err
	}
	flag := os.O_WRONLY | os.O_TRUNC
	if isAppend {
		data := []byte("---\n")
		if isFileEmpty {
			data = []byte{}
		}
		docData = append(data, docData...)
		flag = os.O_WRONLY | os.O_APPEND
	}
	yamlPath := filepath.Join(path, fileName+".yaml")
	file, err := os.OpenFile(yamlPath, flag, fs.ModePerm)
	if err != nil {
		utils.LogError(logger, err, "failed to open file for writing", zap.String("file", yamlPath))
		return err
	}
	defer func() {
		if err := file.Close(); err != nil {
			utils.LogError(logger, err, "failed to close file", zap.String("file", yamlPath))
		}
	}()

	cw := &ctxWriter{
		ctx:    ctx,
		writer: file,
	}

	_, err = cw.Write(docData)
	if err != nil {
		if err == ctx.Err() {
			return nil // Ignore context cancellation error
		}
		utils.LogError(logger, err, "failed to write the yaml document", zap.String("yaml file name", fileName))
		return err
	}
	return nil
}

func ReadFile(ctx context.Context, logger *zap.Logger, path, name string) ([]byte, error) {
	filePath := filepath.Join(path, name+".yaml")
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read the file: %v", err)
	}

	defer func() {
		if err := file.Close(); err != nil {
			utils.LogError(logger, err, "failed to close file", zap.String("file", filePath))
		}
	}()

	cr := &ctxReader{
		ctx: ctx,
		r:   file,
	}

	data, err := io.ReadAll(cr)
	if err != nil {
		if err == ctx.Err() {
			return nil, err // Ignore context cancellation error
		}
		return nil, fmt.Errorf("failed to read the file: %v", err)
	}
	return data, nil
}

func CreateYamlFile(ctx context.Context, Logger *zap.Logger, path string, fileName string) (bool, error) {
	yamlPath, err := ValidatePath(filepath.Join(path, fileName+".yaml"))
	if err != nil {
		utils.LogError(Logger, err, "failed to validate the yaml file path", zap.String("path directory", path), zap.String("yaml", fileName))
		return false, err
	}
	if _, err := os.Stat(yamlPath); err != nil {
		if ctx.Err() == nil {
			err = os.MkdirAll(filepath.Join(path), 0777)
			if err != nil {
				utils.LogError(Logger, err, "failed to create a directory for the yaml file", zap.String("path directory", path), zap.String("yaml", fileName))
				return false, err
			}
			file, err := os.OpenFile(yamlPath, os.O_CREATE, 0777) // Set file permissions to 777
			if err != nil {
				utils.LogError(Logger, err, "failed to create a yaml file", zap.String("path directory", path), zap.String("yaml", fileName))
				return false, err
			}
			err = file.Close()
			if err != nil {
				utils.LogError(Logger, err, "failed to close the yaml file", zap.String("path directory", path), zap.String("yaml", fileName))
				return false, err
			}
			return true, nil
		}
		return false, err
	}
	return false, nil
}

func ReadSessionIndices(_ context.Context, path string, Logger *zap.Logger) ([]string, error) {
	var indices []string
	dir, err := ReadDir(path, fs.FileMode(os.O_RDONLY))
	if err != nil {
		Logger.Debug("creating a folder for the keploy generated testcases", zap.Error(err))
		return indices, nil
	}

	files, err := dir.ReadDir(0)
	if err != nil {
		return indices, err
	}

	for _, v := range files {
		if v.Name() != "reports" && v.Name() != "testReports" && v.IsDir() {
			indices = append(indices, v.Name())
		}
	}
	return indices, nil
}
func GetVariablesType(obj map[string]interface{}) map[string]map[string]string {
	types := make(map[string]map[string]string, 0)
	// Loop over body object and get the type of each value
	for key, value := range obj {
		var valueType string
		if fmt.Sprintf("%T", value) == "float64" {
			valueType = "number"
		} else {
			valueType = fmt.Sprintf("%T", value)
		}
		responseType := map[string]string{
			"type": valueType,
		}
		types[key] = responseType
	}
	return types
}

func UnmarshalAndConvertToJSON(bodyStr []byte, bodyObj map[string]interface{}) ([]byte, map[string]interface{}, error) {
	err := json.Unmarshal(bodyStr, &bodyObj)
	if err != nil {
		return nil, nil, err
	}
	// Convert the response body object back to a JSON string
	bodyJSON, err := json.Marshal(bodyObj)
	if err != nil {
		return nil, nil, err
	}
	return bodyJSON, bodyObj, nil
}

func GenerateRepsonse(responseCode int, responseMessage string, responseTypes map[string]map[string]string, responseBody map[string]interface{}) map[string]models.ResponseItem {
	byCode := map[string]models.ResponseItem{
		fmt.Sprintf("%d", responseCode): {
			Description: responseMessage,
			Content: map[string]models.MediaType{
				"application/json": {
					Schema: models.Schema{
						Type:       "object",
						Properties: responseTypes,
					},
					Example: (responseBody),
				},
			},
		},
	}
	return byCode
}

func ExtractURLPath(fullURL string) (string, string) {
	parsedURL, err := url.Parse(fullURL)

	if err != nil {
		return "", ""
	}
	return parsedURL.Path, parsedURL.Host
}

func GenerateHeader(header map[string]string) []models.Parameter {
	var parameters []models.Parameter
	for key, value := range header {
		parameters = append(parameters, models.Parameter{
			Name:     key,
			In:       "header",
			Required: true,
			Schema:   models.ParamSchema{Type: "string"},
			Example:  value,
		})
	}
	return parameters
}

func GenerateInPathParams(params map[string]string) []models.Parameter {
	var parameters []models.Parameter
	for key, value := range params {
		parameters = append(parameters, models.Parameter{
			Name:     key,
			In:       "path",
			Required: true,
			Schema:   models.ParamSchema{Type: "string"},
			Example:  value,
		})
	}
	return parameters
}

// isNumeric checks if a string is a valid numeric value (integer or float).
func isNumeric(s string) bool {
	if _, err := strconv.Atoi(s); err == nil {
		return true
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return true
	}
	return false
}

// ExtractIdentifiersAndCount extracts numeric identifiers (integers or floats) from the path.
func ExtractIdentifiersAndCount(path string) ([]string, int) {
	segments := strings.Split(path, "/")
	segments2 := strings.Split(segments[len(segments)-1], "?")
	segments = append(segments[:len(segments)-1], segments2[0])
	var identifiers []string
	for _, segment := range segments {
		if segment != "" {
			// Check if the segment is numeric (integer or float)
			if isNumeric(segment) {
				identifiers = append(identifiers, segment)
			}
		}
	}

	return identifiers, len(identifiers)
}

// ExtractQueryParams extracts the query parameters and their names from the URL.
func ExtractQueryParams(rawURL string) (map[string]string, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	queryParams := parsedURL.Query()
	params := make(map[string]string)
	for key, values := range queryParams {
		if len(values) > 0 {
			// Take the first value if multiple are present
			params[key] = values[0]
		}
	}
	return params, nil
}

// GenerateDummyNamesForIdentifiers generates dummy names for the path identifiers.
func GenerateDummyNamesForIdentifiers(identifiers []string) map[string]string {
	dummyNames := make(map[string]string)
	for i, id := range identifiers {
		dummyName := fmt.Sprintf("param%d", i+1)
		dummyNames[dummyName] = id
	}
	return dummyNames
}
func AppendInParameters(parameters []models.Parameter, inParameters map[string]string, count int, paramType string) []models.Parameter {
	if count == 0 {
		return parameters
	}
	for key, value := range inParameters {
		parameters = append(parameters, models.Parameter{
			Name:     key,
			In:       paramType,
			Required: true,
			Schema:   models.ParamSchema{Type: "string"},
			Example:  value,
		})
	}

	return parameters
}

// ReplacePathIdentifiers replaces numeric identifiers in the path with their corresponding dummy names.
func ReplacePathIdentifiers(path string, dummyNames map[string]string) string {
	segments := strings.Split(path, "/")
	var replacedPath []string
	for _, segment := range segments {
		if segment != "" {
			// Check if the segment is numeric (integer or float)
			if isNumeric(segment) {
				dummyName := ""
				for key, value := range dummyNames {
					if value == segment {
						// i want to put '{' and '}' around the key
						dummyName = "{" + key + "}"
						break
					}
				}
				if dummyName != "" {
					replacedPath = append(replacedPath, dummyName)
				} else {
					replacedPath = append(replacedPath, segment)
				}
			} else {
				replacedPath = append(replacedPath, segment)
			}
		}
	}
	finalPath := strings.Join(replacedPath, "/")
	// Add slash at the beginning of the path
	finalPath = "/" + finalPath
	return finalPath
}

func generateUniqueID() string {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		// handle error
		return ""
	}
	return hex.EncodeToString(b) + "-" + time.Now().Format("20060102150405")
}

func ConvertYamlToOpenAPI(ctx context.Context, logger *zap.Logger, filePath string, name string, outputPath string, readData bool, data models.HTTPSchema2, isAppend bool) (success bool) {

	var custom models.HTTPSchema2
	if readData {
		data, err := ReadFile(ctx, logger, filePath, name)
		if err != nil {
			logger.Fatal("Error reading file", zap.Error(err))
			return false
		}

		// Parse the custom format YAML into the HTTPSchema struct
		err = yamlLib.Unmarshal(data, &custom)
		if err != nil {
			logger.Error("Error parsing YAML: %v", zap.Error(err))
			return false
		}
	} else {
		custom = data
	}
	var err error
	// Convert response body to an object
	var responseBodyObject map[string]interface{}
	if custom.Spec.Response.Body != "" {
		_, responseBodyObject, err = UnmarshalAndConvertToJSON([]byte(custom.Spec.Response.Body), responseBodyObject)
		if err != nil {
			logger.Error("Error converting response body object to JSON string", zap.Error(err))
			return false
		}
	}

	// Get the type of each value in the response body object
	responseTypes := GetVariablesType(responseBodyObject)

	// Convert request body to an object
	var requestBodyObject map[string]interface{}
	if custom.Spec.Request.Body != "" {
		_, requestBodyObject, err = UnmarshalAndConvertToJSON([]byte(custom.Spec.Request.Body), requestBodyObject)
		if err != nil {
			logger.Error("Error converting response body object to JSON string", zap.Error(err))
			return false
		}
	}

	// Get the type of each value in the request body object
	requestTypes := GetVariablesType(requestBodyObject)

	// Generate response by status code
	byCode := GenerateRepsonse(custom.Spec.Response.StatusCode, custom.Spec.Response.StatusMessage, responseTypes, responseBodyObject)

	// Add parameters to the request
	parameters := GenerateHeader(custom.Spec.Request.Header)

	// Extract In Path parameters
	identifiers, count := ExtractIdentifiersAndCount(custom.Spec.Request.URL)
	// Generate Dummy Names for the identifiers
	dummyNames := GenerateDummyNamesForIdentifiers(identifiers)
	// Add In Path parameters to the parameters
	parameters = AppendInParameters(parameters, dummyNames, count, "path")
	// Extract Query parameters
	queryParams, err := ExtractQueryParams(custom.Spec.Request.URL)
	if err != nil {
		logger.Error("Error extracting query parameters", zap.Error(err))
		return false
	}
	// Add Query parameters to the parameters
	parameters = AppendInParameters(parameters, queryParams, len(queryParams), "query")
	// Generate Operation ID
	operationID := generateUniqueID()
	// Determine if the request method is GET or POST
	var pathItem models.PathItem
	switch custom.Spec.Request.Method {
	case "GET":
		pathItem = models.PathItem{
			Get: &models.Operation{
				Summary:     "Auto-generated operation",
				Description: "Auto-generated from custom format",
				OperationID: operationID,
				Parameters:  parameters,
				Responses:   byCode,
			},
		}
	case "POST":
		pathItem = models.PathItem{
			Post: &models.Operation{
				Summary:     "Auto-generated operation",
				Description: "Auto-generated from custom format",
				Parameters:  parameters,
				OperationID: operationID,
				RequestBody: &models.RequestBody{
					Content: map[string]models.MediaType{
						"application/json": {
							Schema: models.Schema{
								Type:       "object",
								Properties: requestTypes,
							},
							Example: requestBodyObject,
						},
					},
				},
				Responses: byCode,
			},
		}
	case "PUT":
		pathItem.Put = &models.Operation{
			Summary:     "Update an employee by ID",
			Description: "Update an employee by ID",
			Parameters:  parameters,
			OperationID: operationID,
			RequestBody: &models.RequestBody{
				Content: map[string]models.MediaType{
					"application/json": {
						Schema: models.Schema{
							Type:       "object",
							Properties: requestTypes,
						},
						Example: (requestBodyObject),
					},
				},
			},
			Responses: byCode,
		}
	case "PATCH":
		pathItem.Patch = &models.Operation{
			Summary:     "Auto-generated operation",
			Description: "Auto-generated from custom format",
			Parameters:  parameters,
			OperationID: operationID,
			RequestBody: &models.RequestBody{
				Content: map[string]models.MediaType{
					"application/json": {
						Schema: models.Schema{
							Type:       "object",
							Properties: requestTypes,
						},
						Example: (requestBodyObject),
					},
				},
			},
			Responses: byCode,
		}
	case "DELETE":
		pathItem.Delete = &models.Operation{
			Summary:     "Delete an employee by ID",
			Description: "Delete an employee by ID",
			OperationID: operationID,
			Parameters:  parameters,
			Responses:   byCode,
		}
	default:
		logger.Fatal("Unsupported method")
		return false
	}

	// Extract the URL path
	parsedURL, hostName := ExtractURLPath(custom.Spec.Request.URL)
	if parsedURL == "" {
		logger.Error("Error extracting URL path")
		return false
	}
	// Replace numeric identifiers in the path with dummy names (if exists)
	parsedURL = ReplacePathIdentifiers(parsedURL, dummyNames)
	//If it's mock so there is no hostname so put it temp
	if hostName == "" {
		hostName = "temp"
	}
	// Convert to OpenAPI format
	openapi := models.OpenAPI{
		OpenAPI: "3.0.0",
		Info: models.Info{
			Title:       custom.Name,
			Version:     custom.Version,
			Description: custom.Kind,
		},
		Servers: []map[string]string{
			{
				"url": hostName,
			},
		},

		Paths: map[string]models.PathItem{
			parsedURL: pathItem,
		},
		Components: map[string]interface{}{},
	}

	// Output OpenAPI format as YAML
	openapiYAML, err := yamlLib.Marshal(openapi)
	if err != nil {
		return false
	}
	// Validate using kin-openapi
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(openapiYAML)
	if err != nil {
		logger.Fatal("Error loading OpenAPI document: %v", zap.Error(err))
		return false

	}

	// Validate the OpenAPI document
	if err := doc.Validate(context.Background()); err != nil {
		logger.Fatal("Error validating OpenAPI document: %v", zap.Error(err))
	}

	fmt.Println("OpenAPI document is valid.")
	_, err = os.Stat(outputPath)
	if os.IsNotExist(err) {
		// Create the directory if it doesn't exist
		err = os.MkdirAll(outputPath, os.ModePerm)
		if err != nil {
			logger.Error("Failed to create directory", zap.String("directory", outputPath), zap.Error(err))
			return false
		}
		logger.Info("Directory created", zap.String("directory", outputPath))
	}

	err = WriteFile(ctx, logger, outputPath, name, openapiYAML, isAppend)
	if err != nil {
		logger.Error("Failed to write OpenAPI YAML to a file", zap.Error(err))
		return false
	}

	// // Save OpenAPI YAML to a file
	// outputFilePath := outputPath + "/" + name + ".yaml"
	// outputFile, err := os.Create(outputFilePath)
	// if err != nil {
	// 	return false
	// }
	// defer func() {
	// 	if cerr := outputFile.Close(); cerr != nil {
	// 		logger.Error("Error closing output file", zap.Error(cerr))
	// 		success = false
	// 	}
	// }()
	// _, err = outputFile.Write(openapiYAML)
	// if err != nil {
	// 	return false
	// }

	// fmt.Println("OpenAPI YAML has been saved to " + outputFilePath)
	return true
}
func decodeMocks(yamlMocks []*NetworkTrafficDoc, logger *zap.Logger) ([]*models.Mock, error) {
	mocks := []*models.Mock{}

	for _, m := range yamlMocks {
		mock := models.Mock{
			Version:      m.Version,
			Name:         m.Name,
			Kind:         m.Kind,
			ConnectionID: m.ConnectionID,
		}
		mockCheck := strings.Split(string(m.Kind), "-")
		if len(mockCheck) > 1 {
			logger.Debug("This dependency does not belong to open source version, will be skipped", zap.String("mock kind:", string(m.Kind)))
			continue
		}
		switch m.Kind {
		case models.HTTP:
			httpSpec := models.HTTPSchema{}
			err := m.Spec.Decode(&httpSpec)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal a yaml doc into http mock", zap.Any("mock name", m.Name))
				return nil, err
			}
			mock.Spec = models.MockSpec{
				Metadata: httpSpec.Metadata,
				HTTPReq:  &httpSpec.Request,
				HTTPResp: &httpSpec.Response,

				Created:          httpSpec.Created,
				ReqTimestampMock: httpSpec.ReqTimestampMock,
				ResTimestampMock: httpSpec.ResTimestampMock,
			}

		default:
			utils.LogError(logger, nil, "failed to unmarshal a mock yaml doc of unknown type", zap.Any("type", m.Kind))
			continue
		}
		mocks = append(mocks, &mock)
	}

	return mocks, nil
}
func contains(services []string, service string) bool {
	for _, s := range services {
		if s == service {
			return true
		}
	}
	return false
}
func GenerateHelper(ctx context.Context, logger *zap.Logger, services []string, genTests bool) error {
	var config config.Config
	configData, err := ReadFile(ctx, logger, "./", "keploy")
	if err != nil {
		logger.Fatal("Error reading file", zap.Error(err))
		return err
	}
	err = yamlLib.Unmarshal(configData, &config)
	if err != nil {
		logger.Error("Error parsing YAML: %v", zap.Error(err))
		return err
	}
	mappings := config.Contract.ServicesMapping
	keployFolder := "./keploy/"
	// Read the directory contents
	entries, err := os.ReadDir(keployFolder)
	if err != nil {
		logger.Error("Failed to read directory", zap.String("directory", keployFolder), zap.Error(err))
		return err
	}
	if genTests {

		// Iterate over directory entries
		for _, entry := range entries {
			if entry.IsDir() && strings.Contains(entry.Name(), "test") {
				testsFolder := entry.Name() + "/tests"
				// Read the directory contents
				testEntries, err := os.ReadDir(keployFolder + testsFolder)

				if err != nil {
					logger.Error("Failed to read directory", zap.String("directory", keployFolder+testsFolder), zap.Error(err))
					return err
				}
				for _, testEntry := range testEntries {
					done := ConvertYamlToOpenAPI(ctx, logger, keployFolder+entry.Name()+"/tests", strings.TrimSuffix(testEntry.Name(), ".yaml"), keployFolder+"schema/tests/"+entry.Name(), true, models.HTTPSchema2{}, false)
					if !done {
						logger.Error("Failed to convert the yaml file to openapi")
						return fmt.Errorf("failed to convert the yaml file to openapi")
					}

				}

			}
		}
	} else {
		for _, service := range services {
			// Check if the service is in the services mapping
			if _, exists := mappings[service]; !exists {
				logger.Warn("service not found in services mapping so no contract generation", zap.String("service", service))
				continue
			}
		}
		for _, entry := range entries {
			if entry.IsDir() && strings.Contains(entry.Name(), "test") {
				mockFolder := keployFolder + entry.Name()
				////////////////////////////////////
				var mockYamls []*models.HTTPSchema2
				// var tcsMocks = make([]*models.Mock, 0)
				data, err := ReadFile(ctx, logger, mockFolder, "mocks")
				if err != nil {
					utils.LogError(logger, err, "failed to read the mocks from config yaml", zap.Any("session", filepath.Base(mockFolder)))
					return err
				}
				dec := yamlLib.NewDecoder(bytes.NewReader(data))
				for {
					var doc *models.HTTPSchema2
					err := dec.Decode(&doc)
					if errors.Is(err, io.EOF) {
						break
					}
					if err != nil {
						return fmt.Errorf("failed to decode the yaml file documents. error: %v", err.Error())
					}
					mockYamls = append(mockYamls, doc)
				}
				var duplicateMocks []string
				for _, mock := range mockYamls {
					// Loop over mappings
					var isAppend bool
					for service, serviceMappings := range mappings {
						if !contains(services, service) {
							continue
						}
						var mappingFound bool

						for _, mapping := range serviceMappings {
							if mapping == mock.Spec.Request.URL {
								var mockCode string
								if mock.Spec.Request.URLParams != nil {
									mockCode = fmt.Sprintf("%v", mock.Spec.Request.Method) + "-" + fmt.Sprintf("%v", mock.Spec.Request.URL) + "-0"
								} else {
									mockCode = fmt.Sprintf("%v", mock.Spec.Request.Method) + "-" + fmt.Sprintf("%v", mock.Spec.Request.URL) + "-1"
								}
								if contains(duplicateMocks, mockCode) {
									isAppend = true
								} else {
									duplicateMocks = append(duplicateMocks, mockCode)
								}

								mappingFound = true
								done := ConvertYamlToOpenAPI(ctx, logger, keployFolder+entry.Name(), "mocks", keployFolder+"schema/mocks/"+service+"/"+entry.Name(), false, *mock, isAppend)
								if !done {

									logger.Error("Failed to convert the yaml file to openapi")
									return fmt.Errorf("failed to convert the yaml file to openapi")
								}
								break
							}
						}
						if mappingFound {
							break
						}
					}
				}

			}
		}

	}
	return nil

}

// CopyFile copies a single file from src to dst
func CopyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	if err != nil {
		return err
	}

	// Ensure the copied file has the same permissions as the original file
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	err = os.Chmod(dst, srcInfo.Mode())
	if err != nil {
		return err
	}

	return nil
}

// CopyDir recursively copies a directory tree, attempting to preserve permissions
func CopyDir(srcDir, destDir string) error {
	// Ensure the destination directory exists
	if _, err := os.Stat(destDir); os.IsNotExist(err) {
		err := os.MkdirAll(destDir, os.ModePerm)
		if err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(srcDir, entry.Name())
		destPath := filepath.Join(destDir, entry.Name())

		info, err := entry.Info()
		if err != nil {
			return err
		}

		if info.IsDir() {
			err = os.MkdirAll(destPath, info.Mode())
			if err != nil {
				return err
			}
			err = CopyDir(srcPath, destPath)
			if err != nil {
				return err
			}
		} else {
			err = CopyFile(srcPath, destPath)
			if err != nil {
				return err
			}
		}
	}
	return nil
}
func DeleteFile(_ context.Context, logger *zap.Logger, path, name string) error {
	filePath := filepath.Join(path, name+".yaml")
	err := os.Remove(filePath)
	if err != nil {
		utils.LogError(logger, err, "failed to delete the file", zap.String("file", filePath))
		return fmt.Errorf("failed to delete the file: %v", err)
	}
	return nil
}

func DeleteDir(_ context.Context, logger *zap.Logger, path string) error {
	err := os.RemoveAll(path)
	if err != nil {
		utils.LogError(logger, err, "failed to delete the directory", zap.String("path", path))
		return fmt.Errorf("failed to delete the directory: %v", err)
	}
	return nil
}
