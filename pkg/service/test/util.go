package test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
	"go.keploy.io/server/pkg/platform/yaml"
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

type InitialiseTestReturn struct {
	SessionsMap              map[string]string
	TestReportFS             *yaml.TestReport
	Ctx                      context.Context
	AbortStopHooksForcefully bool
	ProxySet                 *proxy.ProxySet
	ExitCmd                  chan bool
	YamlStore                platform.TestCaseDB
	LoadedHooks              *hooks.Hook
	AbortStopHooksInterrupt  chan bool
}

type TestConfig struct {
	Path             string
	Proxyport        uint32
	TestReportPath   string
	AppCmd           string
	MongoPassword    string
	Testsets         *[]string
	AppContainer     string
	AppNetwork       string
	Delay            uint64
	PassThroughPorts []uint
	ApiTimeout       uint64
}

type RunTestSetConfig struct {
	TestSet        string
	Path           string
	TestReportPath string
	AppCmd         string
	AppContainer   string
	AppNetwork     string
	Delay          uint64
	Pid            uint32
	YamlStore      platform.TestCaseDB
	LoadedHooks    *hooks.Hook
	TestReportFS   yaml.TestReportFS
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
	TestReportFS yaml.TestReportFS
	TestReport   *models.TestReport
	Path         string
	DockerID     bool
	NoiseConfig  models.GlobalNoise
}

type FetchTestResultsConfig struct {
	TestReportFS   yaml.TestReportFS
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

// Filter the mocks based on req and res timestamp of test
func FilterTcsMocks(tc *models.TestCase, m []*models.Mock, logger *zap.Logger) []*models.Mock {
	filteredMocks := make([]*models.Mock, 0)

	if tc.HttpReq.Timestamp == (time.Time{}) {
		logger.Warn("request timestamp is missing for " + tc.Name)
		return m
	}

	if tc.HttpResp.Timestamp == (time.Time{}) {
		logger.Warn("response timestamp is missing for " + tc.Name)
		return m
	}
	for _, mock := range m {
		if mock.Spec.ReqTimestampMock == (time.Time{}) || mock.Spec.ResTimestampMock == (time.Time{}) {
			// If mock doesn't have either of one timestamp, then, logging a warning msg and appending the mock to filteredMocks to support backward compatibility.
			logger.Warn("request or response timestamp of mock is missing for " + tc.Name)
			filteredMocks = append(filteredMocks, mock)
			continue
		}

		// Checking if the mock's request and response timestamps lie between the test's request and response timestamp
		if mock.Spec.ReqTimestampMock.After(tc.HttpReq.Timestamp) && mock.Spec.ResTimestampMock.Before(tc.HttpResp.Timestamp) {
			filteredMocks = append(filteredMocks, mock)
		}
	}
	return filteredMocks
}
