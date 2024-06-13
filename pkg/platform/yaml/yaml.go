package yaml

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"

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
func GenerateRepsonse(responseCode int, responseMessage string, responseTypes map[string]map[string]string, responseBody []byte) map[string]models.ResponseItem {
	byCode := map[string]models.ResponseItem{
		fmt.Sprintf("%d", responseCode): {
			Description: responseMessage,
			Content: map[string]models.MediaType{
				"application/json": {
					Schema: models.Schema{Type: "object",
						Properties: responseTypes},
					Example: string(responseBody),
				},
			},
		},
	}
	return byCode
}
func ExtractURLPath(fullURL string) string {
	parsedURL, err := url.Parse(fullURL)
	if err != nil {
		return ""
	}
	return parsedURL.Path

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
func GenerateParams(params map[string]string) []models.Parameter {
	var parameters []models.Parameter
	for key, value := range params {
		parameters = append(parameters, models.Parameter{
			Name:     key,
			In:       "query",
			Required: true,
			Schema:   models.ParamSchema{Type: "string"},
			Example:  value,
		})
	}
	return parameters
}

func ConvertYamlToOpenAPI(ctx context.Context, logger *zap.Logger, filePath string, name string) bool {

	// data,err=ReadFile(ctx,logger,filePath,name)

	// Read the custom format YAML file
	file, err := os.Open("/home/ahmed/Desktop/GSOC/Keploy/Issues/keploy/keploy/test-set-1/tests/test-10.yaml")
	if err != nil {
		logger.Fatal("Error opening file", zap.Error(err))
		return false
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		logger.Fatal("Error reading file", zap.Error(err))
		return false
	}
	// Parse the custom format YAML into the HTTPSchema struct
	var custom models.HTTPSchema2
	err = yamlLib.Unmarshal(data, &custom)
	if err != nil {
		logger.Error("Error parsing YAML: %v", zap.Error(err))
		return false
	}
	var responseBodyJSON []byte
	// Convert response body to an object
	var responseBodyObject map[string]interface{}
	if custom.Spec.Response.Body != "" {
		responseBodyJSON, responseBodyObject, err = UnmarshalAndConvertToJSON([]byte(custom.Spec.Response.Body), responseBodyObject)
		if err != nil {
			logger.Error("Error converting response body object to JSON string", zap.Error(err))
			return false
		}
	}

	//Get the type of each value in the response body object
	responseTypes := GetVariablesType(responseBodyObject)

	// Convert request body to an object
	var requestBodyObject map[string]interface{}
	var requestBodyJSON []byte
	if custom.Spec.Request.Body != "" {
		requestBodyJSON, requestBodyObject, err = UnmarshalAndConvertToJSON([]byte(custom.Spec.Request.Body), requestBodyObject)
		if err != nil {
			logger.Error("Error converting response body object to JSON string", zap.Error(err))
			return false
		}

	}
	//Get the type of each value in the request body object
	requestTypes := GetVariablesType(requestBodyObject)
	//Generate response by status code
	byCode := GenerateRepsonse(custom.Spec.Response.StatusCode, custom.Spec.Response.StatusMessage, responseTypes, responseBodyJSON)
	// Add parameters to the request
	parameters := GenerateHeader(custom.Spec.Request.Header)

	// Determine if the request method is GET or POST
	var pathItem models.PathItem
	switch custom.Spec.Request.Method {
	case "GET":
		pathItem = models.PathItem{
			Get: &models.Operation{
				Summary:     "Auto-generated operation",
				Description: "Auto-generated from custom format",
				OperationID: "autoGeneratedOp",
				Parameters:  parameters,
				Responses:   byCode,
			},
		}
	case "POST":
		pathItem = models.PathItem{
			Post: &models.Operation{
				Summary:     "Auto-generated operation",
				Description: "Auto-generated from custom format",
				OperationID: "autoGeneratedOp",
				Parameters:  parameters,
				RequestBody: &models.RequestBody{
					Content: map[string]models.MediaType{
						"application/json": {
							Schema: models.Schema{Type: "object",
								Properties: requestTypes,
							},
							Example: string(requestBodyJSON),
						},
					},
				},
				Responses: byCode,
			},
		}
	default:
		logger.Fatal("Unsupported method")
		return false
	}

	// Extract the url by using net/url package
	parsedURL := ExtractURLPath(custom.Spec.Request.URL)
	if parsedURL == "" {
		logger.Error("Error extracting URL path")
		return false
	}

	// Convert to OpenAPI format
	openapi := models.OpenAPI{
		OpenAPI: "3.0.0",
		Info: models.Info{
			Title:   "Generated API",
			Version: custom.Name,
		},
		Paths: map[string]models.PathItem{
			parsedURL: pathItem,
		},
		Components: map[string]interface{}{},
	}

	// Output OpenAPI format as JSON
	openapiJSON, err := json.MarshalIndent(openapi, "", "  ")
	if err != nil {
		return false
	}

	// Save OpenAPI JSON to a file
	outputFile, err := os.Create("openapi_output.json")
	if err != nil {
		// log.Fatalf("Error creating output file: %v", err)
		return false
	}
	defer outputFile.Close()

	_, err = outputFile.Write(openapiJSON)
	if err != nil {
		// log.Fatalf("Error writing to output file: %v", err)
		return false
	}

	fmt.Println("OpenAPI JSON has been saved to openapi_output.json")
	return true
}
