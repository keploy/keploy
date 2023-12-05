package yaml

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
	"go.keploy.io/server/pkg/platform/telemetry"
	"go.keploy.io/server/pkg/proxy/util"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

var Emoji = "\U0001F430" + " Keploy:"

type Yaml struct {
	TcsPath  string
	MockPath string
	MockName string
	TcsName  string
	Logger   *zap.Logger
	tele     *telemetry.Telemetry
}

func NewYamlStore(tcsPath string, mockPath string, tcsName string, mockName string, Logger *zap.Logger, tele *telemetry.Telemetry) *Yaml {
	return &Yaml{
		TcsPath:  tcsPath,
		MockPath: mockPath,
		MockName: mockName,
		TcsName:  tcsName,
		Logger:   Logger,
		tele:     tele,
	}
}

// findLastIndex returns the index for the new yaml file by reading the yaml file names in the given path directory
func findLastIndex(path string, Logger *zap.Logger) (int, error) {

	dir, err := os.OpenFile(path, os.O_RDONLY, fs.FileMode(os.O_RDONLY))
	if err != nil {
		return 1, nil
	}

	files, err := dir.ReadDir(0)
	if err != nil {
		return 1, nil
	}

	lastIndex := 0
	for _, v := range files {
		if v.Name() == "mocks.yaml" || v.Name() == "config.yaml" {
			continue
		}
		fileName := filepath.Base(v.Name())
		fileNameWithoutExt := fileName[:len(fileName)-len(filepath.Ext(fileName))]
		if len(strings.Split(fileNameWithoutExt, "-")) < 2 {
			Logger.Error("failed to decode the last sequence number from yaml test", zap.Any("for the file", fileName), zap.Any("at path", path))
			return 0, errors.New("failed to decode the last sequence number from yaml test")
		}
		indxStr := strings.Split(fileNameWithoutExt, "-")[1]
		indx, err := strconv.Atoi(indxStr)
		if err != nil {
			Logger.Error("failed to read the sequence number from the yaml file name", zap.Error(err), zap.Any("for the file", fileName))
			return 0, err
		}
		if indx > lastIndex {
			lastIndex = indx
		}
	}
	lastIndex += 1

	return lastIndex, nil
}

// write is used to generate the yaml file for the recorded calls and writes the yaml document.
func (ys *Yaml) Write(path, fileName string, docRead platform.Interface) error {
	//
	doc, _ := docRead.(NetworkTrafficDoc)
	isFileEmpty, err := util.CreateYamlFile(path, fileName, ys.Logger)
	if err != nil {
		return err
	}

	yamlPath, err := util.ValidatePath(filepath.Join(path, fileName+".yaml"))
	if err != nil {
		return err
	}

	file, err := os.OpenFile(yamlPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, os.ModePerm)
	if err != nil {
		ys.Logger.Error("failed to open the created yaml file", zap.Error(err), zap.Any("yaml file name", fileName))
		return err
	}

	data := []byte("---\n")
	if isFileEmpty {
		data = []byte{}
	}
	d, err := yamlLib.Marshal(&doc)
	if err != nil {
		ys.Logger.Error("failed to marshal the recorded calls into yaml", zap.Error(err), zap.Any("yaml file name", fileName))
		return err
	}
	data = append(data, d...)

	_, err = file.Write(data)
	if err != nil {
		ys.Logger.Error("failed to write the yaml document", zap.Error(err), zap.Any("yaml file name", fileName))
		return err
	}
	defer file.Close()

	return nil
}

func containsMatchingUrl(urlMethods map[string][]string, urlStr string, requestMethod models.Method) bool {
	parsedURL, err := url.Parse(urlStr)
	if err != nil {
		return false
	}

	// Check for URL path and method
	path := parsedURL.Path
	if methods, exists := urlMethods[path]; exists {
		// Loop through the methods for this path
		for _, method := range methods {
			// If the request method matches one of the allowed methods, return true
			if string(method) == string(requestMethod) {
				return true
			}
		}
		// If the request method is not in the allowed methods, return false
		return false
	}

	return false
}

func hasBannedHeaders(object map[string]string, bannedHeaders []string) bool {
	for headerName, _ := range object {
		for _, bannedHeader := range bannedHeaders {
			if headerName == bannedHeader {
				return true
			}
		}
	}
	return false
}

func (ys *Yaml) WriteTestcase(tcRead platform.Interface, ctx context.Context, filtersRead platform.Interface) error {
	tc, ok := tcRead.(*models.TestCase)
	if !ok {
		return fmt.Errorf(Emoji, "failed to read testcase in WriteTestcase.")
	}
	filters, ok := filtersRead.(*models.Filters)
	if !ok {
		return fmt.Errorf(Emoji, "failed to read testcase in filter.")
	}
	var bypassTestCase = false
	if filters != nil {
		if containsMatchingUrl(filters.URLMethods, tc.HttpReq.URL, tc.HttpReq.Method) {
			bypassTestCase = true
		} else if hasBannedHeaders(tc.HttpReq.Header, filters.ReqHeader) {
			bypassTestCase = true
		}
	}

	if !bypassTestCase {
		ys.tele.RecordedTestAndMocks()
		testsTotal, ok := ctx.Value("testsTotal").(*int)
		if !ok {
			ys.Logger.Debug("failed to get testsTotal from context")
		} else {
			*testsTotal++
		}
		var tcsName string
		if ys.TcsName == "" {
			// finds the recently generated testcase to derive the sequence number for the current testcase
			lastIndx, err := findLastIndex(ys.TcsPath, ys.Logger)
			if err != nil {
				return err
			}
			if tc.Name == "" {
				tcsName = fmt.Sprintf("test-%v", lastIndx)
			} else {
				tcsName = tc.Name
			}
		} else {
			tcsName = ys.TcsName
		}

		// encode the testcase and its mocks into yaml docs
		yamlTc, err := EncodeTestcase(*tc, ys.Logger)
		if err != nil {
			return err
		}

		// write testcase yaml
		yamlTc.Name = tcsName
		err = ys.Write(ys.TcsPath, tcsName, *yamlTc)
		if err != nil {
			ys.Logger.Error("failed to write testcase yaml file", zap.Error(err))
			return err
		}
		ys.Logger.Info("🟠 Keploy has captured test cases for the user's application.", zap.String("path", ys.TcsPath), zap.String("testcase name", tcsName))

	}
	return nil
}

func (ys *Yaml) ReadTestcase(path string, lastSeenId platform.Interface, options platform.Interface) ([]platform.Interface, error) {

	if path == "" {
		path = ys.TcsPath
	}

	tcs := []*models.TestCase{}

	_, err := os.Stat(path)
	if err != nil {
		dirNames := strings.Split(path, "/")
		suitName := ""
		if len(dirNames) > 1 {
			suitName = dirNames[len(dirNames)-2]
		}
		ys.Logger.Debug("no tests are recorded for the session", zap.String("index", suitName))
		tcsRead := make([]platform.Interface, len(tcs))
		for i, tc := range tcs {
			tcsRead[i] = tc
		}
		return tcsRead, nil
	}

	dir, err := os.OpenFile(path, os.O_RDONLY, os.ModePerm)
	if err != nil {
		ys.Logger.Error("failed to open the directory containing yaml testcases", zap.Error(err), zap.Any("path", path))
		return nil, err
	}

	files, err := dir.ReadDir(0)
	if err != nil {
		ys.Logger.Error("failed to read the file names of yaml testcases", zap.Error(err), zap.Any("path", path))
		return nil, err
	}
	for _, j := range files {
		if filepath.Ext(j.Name()) != ".yaml" || strings.Contains(j.Name(), "mocks") {
			continue
		}

		name := strings.TrimSuffix(j.Name(), filepath.Ext(j.Name()))
		yamlTestcase, err := read(path, name)
		if err != nil {
			ys.Logger.Error("failed to read the testcase from yaml", zap.Error(err))
			return nil, err
		}
		// Unmarshal the yaml doc into Testcase
		tc, err := Decode(yamlTestcase[0], ys.Logger)
		if err != nil {
			return nil, err
		}
		// Append the encoded testcase
		tcs = append(tcs, tc)
	}

	sort.Slice(tcs, func(i, j int) bool {
		return tcs[i].Created < tcs[j].Created
	})
	tcsRead := make([]platform.Interface, len(tcs))
	for i, tc := range tcs {
		tcsRead[i] = tc
	}
	return tcsRead, nil
}

func read(path, name string) ([]*NetworkTrafficDoc, error) {
	file, err := os.OpenFile(filepath.Join(path, name+".yaml"), os.O_RDONLY, os.ModePerm)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := yamlLib.NewDecoder(file)
	yamlDocs := []*NetworkTrafficDoc{}
	for {
		var doc NetworkTrafficDoc
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to decode the yaml file documents. error: %v", err.Error())
		}
		yamlDocs = append(yamlDocs, &doc)
	}
	return yamlDocs, nil
}

func (ys *Yaml) WriteMock(mockRead platform.Interface, ctx context.Context) error {
	mock := mockRead.(*models.Mock)
	mocksTotal, ok := ctx.Value("mocksTotal").(*map[string]int)
	if !ok {
		ys.Logger.Debug("failed to get mocksTotal from context")
	}
	(*mocksTotal)[string(mock.Kind)]++
	if ctx.Value("cmd") == "mockrecord" {
		ys.tele.RecordedMock(string(mock.Kind))
	}
	if ys.MockName != "" {
		mock.Name = ys.MockName
	}

	mockYaml, err := EncodeMock(mock, ys.Logger)
	if err != nil {
		return err
	}

	if mock.Name == "" {
		mock.Name = "mocks"
	}

	err = ys.Write(ys.MockPath, mock.Name, *mockYaml)
	if err != nil {
		return err
	}

	return nil
}

func (ys *Yaml) ReadTcsMocks(tcRead platform.Interface, path string) ([]platform.Interface, error) {
	tc, _ := tcRead.(*models.TestCase)
	var (
		tcsMocks = make([]platform.Interface, 0)
	)

	if path == "" {
		path = ys.MockPath
	}

	mockName := "mocks"
	if ys.MockName != "" {
		mockName = ys.MockName
	}

	mockPath, err := util.ValidatePath(path + "/" + mockName + ".yaml")
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(mockPath); err == nil {

		yamls, err := read(path, mockName)
		if err != nil {
			ys.Logger.Error("failed to read the mocks from config yaml", zap.Error(err), zap.Any("session", filepath.Base(path)))
			return nil, err
		}
		mocks, err := decodeMocks(yamls, ys.Logger)
		if err != nil {
			ys.Logger.Error("failed to decode the config mocks from yaml docs", zap.Error(err), zap.Any("session", filepath.Base(path)))
			return nil, err
		}

		for _, mock := range mocks {
			if mock.Spec.Metadata["type"] != "config" {
				tcsMocks = append(tcsMocks, mock)
			}
		}
	}
	filteredMocks := make([]platform.Interface, 0)

	if tc.HttpReq.Timestamp == (time.Time{}) {
		ys.Logger.Warn("request timestamp is missing for " + tc.Name)
		return tcsMocks, nil
	}

	if tc.HttpResp.Timestamp == (time.Time{}) {
		ys.Logger.Warn("response timestamp is missing for " + tc.Name)
		return tcsMocks, nil
	}
	var entMocks, nonKeployMocks []string
	for _, readMock := range tcsMocks {
		mock := readMock.(*models.Mock)
		if mock.Version == "api.keploy-enterprise.io/v1beta1" {
			entMocks = append(entMocks, mock.Name)
		} else if mock.Version != "api.keploy.io/v1beta1" {
			nonKeployMocks = append(nonKeployMocks, mock.Name)
		}
		if mock.Spec.ReqTimestampMock == (time.Time{}) || mock.Spec.ResTimestampMock == (time.Time{}) {
			// If mock doesn't have either of one timestamp, then, logging a warning msg and appending the mock to filteredMocks to support backward compatibility.
			ys.Logger.Warn("request or response timestamp of mock is missing for " + tc.Name)
			filteredMocks = append(filteredMocks, mock)
			continue
		}

		// Checking if the mock's request and response timestamps lie between the test's request and response timestamp
		if mock.Spec.ReqTimestampMock.After(tc.HttpReq.Timestamp) && mock.Spec.ResTimestampMock.Before(tc.HttpResp.Timestamp) {
			filteredMocks = append(filteredMocks, mock)
		}
	}
	if len(entMocks) > 0 {
		ys.Logger.Warn("These mocks have been recorded with Keploy Enterprise, may not work properly with the open-source version", zap.Strings("enterprise mocks:", entMocks))
	}
	if len(nonKeployMocks) > 0 {
		ys.Logger.Warn("These mocks have not been recorded by Keploy, may not work properly with Keploy.", zap.Strings("non-keploy mocks:", nonKeployMocks))
	}

	return filteredMocks, nil

}

func (ys *Yaml) ReadConfigMocks(path string) ([]platform.Interface, error) {
	var (
		configMocks = make([]platform.Interface, 0)
	)

	if path == "" {
		path = ys.MockPath
	}

	mockName := "mocks"
	if ys.MockName != "" {
		mockName = ys.MockName
	}

	mockPath, err := util.ValidatePath(path + "/" + mockName + ".yaml")
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(mockPath); err == nil {

		yamls, err := read(path, mockName)
		if err != nil {
			ys.Logger.Error("failed to read the mocks from config yaml", zap.Error(err), zap.Any("session", filepath.Base(path)))
			return nil, err
		}
		mocks, err := decodeMocks(yamls, ys.Logger)
		if err != nil {
			ys.Logger.Error("failed to decode the config mocks from yaml docs", zap.Error(err), zap.Any("session", filepath.Base(path)))
			return nil, err
		}

		for _, mock := range mocks {
			if mock.Spec.Metadata["type"] == "config" {
				configMocks = append(configMocks, mock)
			}
		}
	}

	return configMocks, nil

}
func (ys *Yaml) UpdateTest(mock *models.Mock, ctx context.Context) error {
	return nil
}

func (ys *Yaml) DeleteTest(mock *models.Mock, ctx context.Context) error {
	return nil
}
