package test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
	"go.keploy.io/server/pkg/platform/telemetry"
	"go.keploy.io/server/pkg/proxy"
	"go.uber.org/zap"
)

type InitialiseRunTestSetReturn struct {
	Tcs           []*models.TestCase
	ErrChan       chan error
	TestReport    *models.TestReport
	DockerID      bool
	UserIP        string
	InitialStatus models.TestRunStatus
	TcsMocks      []*models.Mock
}

type TestEnvironmentSetup struct {
	Sessions                 []string
	TestReportFS             platform.TestReportDB
	Ctx                      context.Context
	AbortStopHooksForcefully bool
	ProxySet                 *proxy.ProxySet
	ExitCmd                  chan bool
	Storage                  platform.TestCaseDB
	LoadedHooks              *hooks.Hook
	AbortStopHooksInterrupt  chan bool
}

type TestConfig struct {
	Path               string
	Proxyport          uint32
	TestReportPath     string
	AppCmd             string
	MongoPassword      string
	AppContainer       string
	AppNetwork         string
	Delay              uint64
	BuildDelay         time.Duration
	PassThroughPorts   []uint
	ApiTimeout         uint64
	WithCoverage       bool
	CoverageReportPath string
	TestReport         platform.TestReportDB
	Storage            platform.TestCaseDB
	Tele               *telemetry.Telemetry
}

type RunTestSetConfig struct {
	TestSet        string
	Path           string
	TestReportPath string
	AppCmd         string
	AppContainer   string
	AppNetwork     string
	Delay          uint64
	BuildDelay     time.Duration
	Pid            uint32
	Storage        platform.TestCaseDB
	LoadedHooks    *hooks.Hook
	TestReportFS   platform.TestReportDB
	TestRunChan    chan string
	ApiTimeout     uint64
	Ctx            context.Context
	ServeTest      bool
}

type SimulateRequestConfig struct {
	Tc           *models.TestCase
	LoadedHooks  *hooks.Hook
	AppCmd       string
	UserIP       string
	TestSet      string
	ApiTimeout   uint64
	Success      *int
	Failure      *int
	Status       *models.TestRunStatus
	TestReportFS platform.TestReportDB
	TestReport   *models.TestReport
	Path         string
	DockerID     bool
	NoiseConfig  models.GlobalNoise
}

type FetchTestResultsConfig struct {
	TestReportFS   platform.TestReportDB
	TestReport     *models.TestReport
	Status         *models.TestRunStatus
	TestSet        string
	Success        *int
	Failure        *int
	Ctx            context.Context
	TestReportPath string
	Path           string
}

func FlattenHttpResponse(h http.Header, body string) (map[string][]string, error) {
	m := map[string][]string{}
	for k, v := range h {
		m["header."+k] = []string{strings.Join(v, "")}
	}
	err := AddHttpBodyToMap(body, m)
	if err != nil {
		return m, err
	}
	return m, nil
}

// Flatten takes a map and returns a new one where nested maps are replaced
// by dot-delimited keys.
// examples of valid jsons - https://developer.mozilla.org/en-US/docs/Web/JavaScript/Reference/Global_Objects/JSON/parse#examples
func Flatten(j interface{}) map[string][]string {
	if j == nil {
		return map[string][]string{"": {""}}
	}
	o := make(map[string][]string)
	x := reflect.ValueOf(j)
	switch x.Kind() {
	case reflect.Map:
		m, ok := j.(map[string]interface{})
		if !ok {
			return map[string][]string{}
		}
		for k, v := range m {
			nm := Flatten(v)
			for nk, nv := range nm {
				fk := k
				if nk != "" {
					fk = fk + "." + nk
				}
				o[fk] = nv
			}
		}
	case reflect.Bool:
		o[""] = []string{strconv.FormatBool(x.Bool())}
	case reflect.Float64:
		o[""] = []string{strconv.FormatFloat(x.Float(), 'E', -1, 64)}
	case reflect.String:
		o[""] = []string{x.String()}
	case reflect.Slice:
		child, ok := j.([]interface{})
		if !ok {
			return map[string][]string{}
		}
		for _, av := range child {
			nm := Flatten(av)
			for nk, nv := range nm {
				if ov, exists := o[nk]; exists {
					o[nk] = append(ov, nv...)
				} else {
					o[nk] = nv
				}
			}
		}
	default:
		fmt.Println(Emoji, "found invalid value in json", j, x.Kind())
	}
	return o
}

func AddHttpBodyToMap(body string, m map[string][]string) error {
	// add body
	if json.Valid([]byte(body)) {
		var result interface{}

		err := json.Unmarshal([]byte(body), &result)
		if err != nil {
			return err
		}
		j := Flatten(result)
		for k, v := range j {
			nk := "body"
			if k != "" {
				nk = nk + "." + k
			}
			m[nk] = v
		}
	} else {
		// add it as raw text
		m["body"] = []string{body}
	}
	return nil
}

func LeftJoinNoise(globalNoise models.GlobalNoise, tsNoise models.GlobalNoise) models.GlobalNoise {
	noise := globalNoise
	for field, regexArr := range tsNoise["body"] {
		noise["body"][field] = regexArr
	}
	for field, regexArr := range tsNoise["header"] {
		noise["header"][field] = regexArr
	}
	return noise
}

func MatchesAnyRegex(str string, regexArray []string) (bool, string) {
	for _, pattern := range regexArray {
		re := regexp.MustCompile(pattern)
		if re.MatchString(str) {
			return true, pattern
		}
	}
	return false, ""
}

func MapToArray(mp map[string][]string) []string {
	var result []string
	for k := range mp {
		result = append(result, k)
	}
	return result
}

func CheckStringExist(s string, mp map[string][]string) ([]string, bool) {
	if val, ok := mp[s]; ok {
		return val, ok
	}
	ok, val := MatchesAnyRegex(s, MapToArray(mp))
	if ok {
		return mp[val], ok
	}
	return []string{}, false
}

func CompareHeaders(h1 http.Header, h2 http.Header, res *[]models.HeaderResult, noise map[string][]string) bool {
	if res == nil {
		return false
	}
	match := true
	_, isHeaderNoisy := noise["header"]
	for k, v := range h1 {
		regexArr, isNoisy := CheckStringExist(k, noise)
		if isNoisy && len(regexArr) != 0 {
			isNoisy, _ = MatchesAnyRegex(v[0], regexArr)
		}
		isNoisy = isNoisy || isHeaderNoisy
		val, ok := h2[k]
		if !isNoisy {
			if !ok {
				if checkKey(res, k) {
					*res = append(*res, models.HeaderResult{
						Normal: false,
						Expected: models.Header{
							Key:   k,
							Value: v,
						},
						Actual: models.Header{
							Key:   k,
							Value: nil,
						},
					})
				}

				match = false
				continue
			}
			if len(v) != len(val) {
				if checkKey(res, k) {
					*res = append(*res, models.HeaderResult{
						Normal: false,
						Expected: models.Header{
							Key:   k,
							Value: v,
						},
						Actual: models.Header{
							Key:   k,
							Value: val,
						},
					})
				}
				match = false
				continue
			}
			for i, e := range v {
				if val[i] != e {
					if checkKey(res, k) {
						*res = append(*res, models.HeaderResult{
							Normal: false,
							Expected: models.Header{
								Key:   k,
								Value: v,
							},
							Actual: models.Header{
								Key:   k,
								Value: val,
							},
						})
					}
					match = false
					continue
				}
			}
		}
		if checkKey(res, k) {
			*res = append(*res, models.HeaderResult{
				Normal: true,
				Expected: models.Header{
					Key:   k,
					Value: v,
				},
				Actual: models.Header{
					Key:   k,
					Value: val,
				},
			})
		}
	}
	for k, v := range h2 {
		regexArr, isNoisy := CheckStringExist(k, noise)
		if isNoisy && len(regexArr) != 0 {
			isNoisy, _ = MatchesAnyRegex(v[0], regexArr)
		}
		isNoisy = isNoisy || isHeaderNoisy
		val, ok := h1[k]
		if isNoisy && checkKey(res, k) {
			*res = append(*res, models.HeaderResult{
				Normal: true,
				Expected: models.Header{
					Key:   k,
					Value: val,
				},
				Actual: models.Header{
					Key:   k,
					Value: v,
				},
			})
			continue
		}
		if !ok {
			if checkKey(res, k) {
				*res = append(*res, models.HeaderResult{
					Normal: false,
					Expected: models.Header{
						Key:   k,
						Value: nil,
					},
					Actual: models.Header{
						Key:   k,
						Value: v,
					},
				})
			}

			match = false
		}
	}
	return match
}

func checkKey(res *[]models.HeaderResult, key string) bool {
	for _, v := range *res {
		if key == v.Expected.Key {
			return false
		}
	}
	return true
}

func Contains(elems []string, v string) bool {
	for _, s := range elems {
		if v == s {
			return true
		}
	}
	return false
}

// Sort the mocks in such a way that the mocks that have request timestamp between the test's request and response timestamp are at the top
// and are order by the request timestamp in ascending order
// Other mocks are sorted by closest request timestamp to the middle of the test's request and response timestamp
func SortMocks(tc *models.TestCase, m []*models.Mock, logger *zap.Logger) []*models.Mock {
	filteredMocks, unFilteredMocks := FilterMocks(tc, m, logger)
	// Sort the filtered mocks based on the request timestamp
	sort.SliceStable(filteredMocks, func(i, j int) bool {
		return filteredMocks[i].Spec.ReqTimestampMock.Before(filteredMocks[j].Spec.ReqTimestampMock)
	})

	// Sort the unfiltered mocks based on some criteria (modify as needed)
	sort.SliceStable(unFilteredMocks, func(i, j int) bool {
		return unFilteredMocks[i].Spec.ReqTimestampMock.Before(unFilteredMocks[j].Spec.ReqTimestampMock)
	})

	// select first 10 mocks from the unfiltered mocks
	if len(unFilteredMocks) > 10 {
		unFilteredMocks = unFilteredMocks[:10]
	}

	// Append the unfiltered mocks to the filtered mocks
	sortedMocks := append(filteredMocks, unFilteredMocks...)
	// logger.Info("sorted mocks after sorting accornding to the testcase timestamps", zap.Any("testcase", tc.Name), zap.Any("mocks", sortedMocks))
	for _, v := range sortedMocks {
		logger.Debug("sorted mocks", zap.Any("testcase", tc.Name), zap.Any("mocks", v))
	}

	return sortedMocks
}

// Filter the mocks based on req and res timestamp of test
func FilterMocks(tc *models.TestCase, m []*models.Mock, logger *zap.Logger) ([]*models.Mock, []*models.Mock) {
	filteredMocks := make([]*models.Mock, 0)
	unFilteredMocks := make([]*models.Mock, 0)

	if tc.HttpReq.Timestamp == (time.Time{}) {
		logger.Warn("request timestamp is missing for " + tc.Name)
		return m, filteredMocks
	}

	if tc.HttpResp.Timestamp == (time.Time{}) {
		logger.Warn("response timestamp is missing for " + tc.Name)
		return m, filteredMocks
	}
	for _, mock := range m {
		if mock.Spec.ReqTimestampMock == (time.Time{}) || mock.Spec.ResTimestampMock == (time.Time{}) {
			// If mock doesn't have either of one timestamp, then, logging a warning msg and appending the mock to filteredMocks to support backward compatibility.
			logger.Warn("request or response timestamp of mock is missing for " + tc.Name)
			mock.TestModeInfo.IsFiltered = true
			filteredMocks = append(filteredMocks, mock)
			continue
		}

		// Checking if the mock's request and response timestamps lie between the test's request and response timestamp
		if mock.Spec.ReqTimestampMock.After(tc.HttpReq.Timestamp) && mock.Spec.ResTimestampMock.Before(tc.HttpResp.Timestamp) {
			mock.TestModeInfo.IsFiltered = true
			filteredMocks = append(filteredMocks, mock)
			continue
		}
		mock.TestModeInfo.IsFiltered = false
		unFilteredMocks = append(unFilteredMocks, mock)
	}
	logger.Debug("filtered mocks after filtering accornding to the testcase timestamps", zap.Any("testcase", tc.Name), zap.Any("mocks", filteredMocks))
	// TODO change this to debug
	logger.Debug("number of filtered mocks", zap.Any("testcase", tc.Name), zap.Any("number of filtered mocks", len(filteredMocks)))
	return filteredMocks, unFilteredMocks
}

// creates a directory if not exists with all user access
func makeDirectory(path string) error {
	oldUmask := syscall.Umask(0)
	err := os.MkdirAll(path, 0777)
	if err != nil {
		return err
	}
	syscall.Umask(oldUmask)
	return nil
}
