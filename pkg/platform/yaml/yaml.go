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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform"
	"go.keploy.io/server/v2/pkg/platform/telemetry"
	"go.keploy.io/server/v2/pkg/proxy/util"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

var Emoji = "\U0001F430" + " Keploy:"

type Yaml struct {
	TcsPath     string
	MockPath    string
	MockName    string
	TcsName     string
	Logger      *zap.Logger
	tele        *telemetry.Telemetry
	nameCounter int
	mutex       sync.RWMutex
}

func NewYamlStore(tcsPath string, mockPath string, tcsName string, mockName string, Logger *zap.Logger, tele *telemetry.Telemetry) platform.TestCaseDB {
	return &Yaml{
		TcsPath:     tcsPath,
		MockPath:    mockPath,
		MockName:    mockName,
		TcsName:     tcsName,
		Logger:      Logger,
		tele:        tele,
		nameCounter: 0,
		mutex:       sync.RWMutex{},
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
		fileNameParts := strings.Split(fileNameWithoutExt, "-")
		if len(fileNameParts) != 2 || (fileNameParts[0] != "test" && fileNameParts[0] != "report") {
			continue
		}
		indxStr := fileNameParts[1]
		indx, err := strconv.Atoi(indxStr)
		if err != nil {
			continue
		}
		if indx > lastIndex {
			lastIndex = indx
		}
	}
	lastIndex += 1

	return lastIndex, nil
}

// write is used to generate the yaml file for the recorded calls and writes the yaml document.
func (ys *Yaml) Write(path, fileName string, docRead platform.KindSpecifier) error {
	//
	doc, _ := docRead.(*NetworkTrafficDoc)
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

func ContainsMatchingUrl(urlMethods []string, urlStr string, requestUrl string, requestMethod models.Method) (error, bool) {
	urlMatched := false
	parsedURL, err := url.Parse(requestUrl)
	if err != nil {
		return err, false
	}

	// Check for URL path and method
	regex, err := regexp.Compile(urlStr)
	if err != nil {
		return err, false
	}

	urlMatch := regex.MatchString(parsedURL.Path)

	if urlMatch && len(urlStr) != 0 {
		urlMatched = true
	}

	if len(urlMethods) != 0 && urlMatched {
		urlMatched = false
		for _, method := range urlMethods {
			if string(method) == string(requestMethod) {
				urlMatched = true
			}
		}
	}

	return nil, urlMatched
}

func HasBannedHeaders(object map[string]string, bannedHeaders map[string]string) (error, bool) {
	for headerName, headerNameValue := range object {
		for bannedHeaderName, bannedHeaderValue := range bannedHeaders {
			regex, err := regexp.Compile(headerName)
			if err != nil {
				return err, false
			}
			headerNameMatch := regex.MatchString(bannedHeaderName)

			regex, err = regexp.Compile(bannedHeaderValue)
			if err != nil {
				return err, false
			}
			headerValueMatch := regex.MatchString(headerNameValue)
			if headerNameMatch && headerValueMatch {
				return nil, true
			}
		}
	}
	return nil, false
}

func (ys *Yaml) WriteTestcase(tcRead platform.KindSpecifier, ctx context.Context, filtersRead platform.KindSpecifier) error {
	tc, ok := tcRead.(*models.TestCase)
	if !ok {
		return fmt.Errorf("%s failed to read testcase in WriteTestcase", Emoji)
	}
	testFilters, ok := filtersRead.(*models.TestFilter)

	var bypassTestCase = false

	if ok {
		for _, testFilter := range testFilters.Filters {
			if err, containsMatch := ContainsMatchingUrl(testFilter.UrlMethods, testFilter.Path, tc.HttpReq.URL, tc.HttpReq.Method); err == nil && containsMatch {
				bypassTestCase = true
			} else if err != nil {
				return fmt.Errorf("%s failed to check matching url, error %s", Emoji, err.Error())
			} else if bannerHeaderCheck, hasHeader := HasBannedHeaders(tc.HttpReq.Header, testFilter.Headers); bannerHeaderCheck == nil && hasHeader {
				bypassTestCase = true
			} else if bannerHeaderCheck != nil {
				return fmt.Errorf("%s failed to check banned header, error %s", Emoji, err.Error())
			}
		}
	}
	if !bypassTestCase {
		if ys.tele != nil {
			ys.tele.RecordedTestAndMocks()
			ys.mutex.Lock()
			testsTotal, ok := ctx.Value("testsTotal").(*int)
			if !ok {
				ys.Logger.Debug("failed to get testsTotal from context")
			} else {
				*testsTotal++
			}
			ys.mutex.Unlock()
		}

		var tcsName string
		if ys.TcsName == "" {
			if tc.Name == "" {
				// finds the recently generated testcase to derive the sequence number for the current testcase
				lastIndx, err := findLastIndex(ys.TcsPath, ys.Logger)
				if err != nil {
					return err
				}
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
		err = ys.Write(ys.TcsPath, tcsName, yamlTc)
		if err != nil {
			ys.Logger.Error("failed to write testcase yaml file", zap.Error(err))
			return err
		}
		ys.Logger.Info("🟠 Keploy has captured test cases for the user's application.", zap.String("path", ys.TcsPath), zap.String("testcase name", tcsName))

	}
	return nil
}

func (ys *Yaml) ReadTestcases(testSet string, lastSeenId platform.KindSpecifier, options platform.KindSpecifier) ([]platform.KindSpecifier, error) {
	path := ys.MockPath + "/" + testSet + "/tests"
	tcs := []*models.TestCase{}

	mockPath, err := util.ValidatePath(path)
	if err != nil {
		return nil, err
	}
	_, err = os.Stat(mockPath)
	if err != nil {
		ys.Logger.Debug("no tests are recorded for the session", zap.String("index", testSet))
		tcsRead := make([]platform.KindSpecifier, len(tcs))
		return tcsRead, nil
	}

	dir, err := os.OpenFile(mockPath, os.O_RDONLY, os.ModePerm)
	if err != nil {
		ys.Logger.Error("failed to open the directory containing yaml testcases", zap.Error(err), zap.Any("path", mockPath))
		return nil, err
	}

	files, err := dir.ReadDir(0)
	if err != nil {
		ys.Logger.Error("failed to read the file names of yaml testcases", zap.Error(err), zap.Any("path", mockPath))
		return nil, err
	}
	for _, j := range files {
		if filepath.Ext(j.Name()) != ".yaml" || strings.Contains(j.Name(), "mocks") {
			continue
		}

		name := strings.TrimSuffix(j.Name(), filepath.Ext(j.Name()))
		yamlTestcase, err := read(mockPath, name)
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

	sort.SliceStable(tcs, func(i, j int) bool {
		return tcs[i].HttpReq.Timestamp.Before(tcs[j].HttpReq.Timestamp)
	})
	tcsRead := make([]platform.KindSpecifier, len(tcs))
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

func (ys *Yaml) WriteMock(mockRead platform.KindSpecifier, ctx context.Context) error {
	mock := mockRead.(*models.Mock)
	mocksTotal, ok := ctx.Value("mocksTotal").(*map[string]int)
	if !ok {
		ys.Logger.Debug("failed to get mocksTotal from context")
	}
	(*mocksTotal)[string(mock.Kind)]++
	if ctx.Value("cli") == "mockrecord" {
		if ys.tele != nil {
			ys.tele.RecordedMock(string(mock.Kind))
		}
	}
	if ys.MockName != "" {
		mock.Name = ys.MockName
	}

	mock.Name = fmt.Sprint("mock-", getNextID())
	mockYaml, err := EncodeMock(mock, ys.Logger)
	if err != nil {
		return err
	}

	// if mock.Name == "" {
	// 	mock.Name = "mocks"
	// }

	err = ys.Write(ys.MockPath, "mocks", mockYaml)
	if err != nil {
		return err
	}

	return nil
}

func (ys *Yaml) ReadTcsMocks(tcRead platform.KindSpecifier, testSet string) ([]platform.KindSpecifier, error) {
	tc, readTcs := tcRead.(*models.TestCase)
	var (
		tcsMocks = make([]platform.KindSpecifier, 0)
	)

	mockName := "mocks"
	if ys.MockName != "" {
		mockName = ys.MockName
	}

	path := ys.MockPath + "/" + testSet
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
			if mock.Spec.Metadata["type"] != "config" && mock.Kind != "Generic" {
				tcsMocks = append(tcsMocks, mock)
			}
			//if postgres type confgi
		}
	}
	filteredMocks := make([]platform.KindSpecifier, 0)
	if !readTcs {
		return tcsMocks, nil
	}
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
		} else if mock.Version != "api.keploy.io/v1beta1" && mock.Version != "api.keploy.io/v1beta2" {
			nonKeployMocks = append(nonKeployMocks, mock.Name)
		}
		if (mock.Spec.ReqTimestampMock == (time.Time{}) || mock.Spec.ResTimestampMock == (time.Time{})) && mock.Kind != "SQL" {
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

func (ys *Yaml) ReadConfigMocks(testSet string) ([]platform.KindSpecifier, error) {
	var (
		configMocks = make([]platform.KindSpecifier, 0)
	)

	mockName := "mocks"
	if ys.MockName != "" {
		mockName = ys.MockName
	}
	path := ys.MockPath + "/" + testSet

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
			if mock.Spec.Metadata["type"] == "config" || mock.Kind == "Postgres" || mock.Kind == "Generic" {
				configMocks = append(configMocks, mock)
			}
		}
	}

	return configMocks, nil

}

func (ys *Yaml) update(path, fileName string, docRead platform.KindSpecifier) error {
	doc, _ := docRead.(*NetworkTrafficDoc)

	yamlPath, err := util.ValidatePath(filepath.Join(path, fileName+".yaml"))
	if err != nil {
		return err
	}

	file, err := os.OpenFile(yamlPath, os.O_WRONLY|os.O_TRUNC, os.ModePerm)
	if err != nil {
		ys.Logger.Error("failed to open the yaml file", zap.Error(err), zap.Any("yaml file name", fileName))
		return err
	}

	d, err := yamlLib.Marshal(&doc)
	if err != nil {
		ys.Logger.Error("failed to marshal the updated calls into yaml", zap.Error(err), zap.Any("yaml file name", fileName))
		return err
	}

	_, err = file.Write(d)
	if err != nil {
		ys.Logger.Error("failed to tools the yaml document", zap.Error(err), zap.Any("yaml file name", fileName))
		return err
	}
	defer file.Close()

	return nil
}

func (ys *Yaml) UpdateTestCase(tcRead platform.KindSpecifier, path, tcsName string, ctx context.Context) error {
	tc, ok := tcRead.(*models.TestCase)
	if !ok {
		return fmt.Errorf("%s failed to read testcase in UpdateTest", Emoji)
	}

	// encode the testcase into yaml docs
	yamlTc, err := EncodeTestcase(*tc, ys.Logger)
	if err != nil {
		return fmt.Errorf("%s failed to encode the testcase into yamldocs in UpdateTest:%v", Emoji, err)
	}

	yamlTc.Name = tcsName

	// tools testcase yaml
	err = ys.update(path, tcsName, yamlTc)
	if err != nil {
		ys.Logger.Error("failed to tools testcase yaml file", zap.Error(err))
		return err
	}
	ys.Logger.Info("🔄 Keploy has updated the test case for the user's application.", zap.String("path", path), zap.String("testcase name", tcsName))
	return nil
}

func (ys *Yaml) DeleteTest(mock *models.Mock, ctx context.Context) error {
	return nil
}

var idCounter int64 = -1

func getNextID() int64 {
	return atomic.AddInt64(&idCounter, 1)
}

func (ys *Yaml) ReadTestSessionIndices() ([]string, error) {
	return pkg.ReadSessionIndices(ys.MockPath, ys.Logger)
}
