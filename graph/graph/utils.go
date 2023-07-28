package graph

import (
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"strconv"

	"go.keploy.io/server/graph/graph/model"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform/fs"
)

func convertStringToPointer(s string) *string {
	return &s
}
func convertInttoPointer(i int) *int {
	return &i
}
func converMethodToPointer(m models.Method) *models.Method {
	return &m
}
func convertToKV(m map[string]string) []*model.Kv {
	var kv []*model.Kv
	for k, v := range m {
		newKv := &model.Kv{
			Key:   &k,
			Value: &v,
		}
		kv = append(kv, newKv)
	}
	return kv
}
func convertToFormData(m []models.FormData) []*model.FormData {
	var fd []*model.FormData
	for _, v := range m {
		valuePtrs := make([]*string, len(v.Values))
		for i := range v.Values {
			valuePtrs[i] = &v.Values[i]
		}
		pathPtrs := make([]*string, len(v.Paths))
		for i := range v.Paths {
			pathPtrs[i] = &v.Paths[i]
		}
		newFd := &model.FormData{
			Key:    &v.Key,
			Values: valuePtrs,
			Paths:  pathPtrs,
		}
		fd = append(fd, newFd)
	}
	return fd
}
func convertToArrayKV(m map[string][]string) []*model.Kv {
	var kv []*model.Kv
	for _, v := range m {
		for _, v2 := range v {
			newKv := &model.Kv{
				Key:   &v2,
				Value: &v2,
			}
			kv = append(kv, newKv)
		}
	}
	return kv
}
func convertMocks(m []*models.Mock) []*model.Mock {
	var mocks []*model.Mock
	var spec string
	for _, v := range m {
		newMock := &model.Mock{
			Version:  (*model.Version)(&v.Version),
			Kind:     (*model.Kind)(&v.Kind),
			MockName: &v.Name,
			Spec:     &(spec),
		}
		mocks = append(mocks, newMock)
	}
	return mocks
}
func convertToArrayString(m []string) []*string {
	var str []*string
	for _, v := range m {
		newStr := &v
		str = append(str, newStr)
	}
	return str
}
func convertBooltoPointer(b bool) *bool {
	return &b
}
func convertToModelHeader(m *models.Header) *model.Header {
	var header *model.Header
	var values []*string
	for _, v := range m.Value {
		newValue := &v
		values = append(values, newValue)
	}
	if m != nil {
		header = &model.Header{
			Key:   &m.Key,
			Value: values,
		}
	}
	return header
}
func convertToArrayHeadersResult(m []models.HeaderResult) []*model.HeaderResult {
	var headers []*model.HeaderResult
	for _, v := range m {
		newHeader := &model.HeaderResult{
			Normal:   &v.Normal,
			Expected: convertToModelHeader(&v.Expected),
			Actual:   convertToModelHeader(&v.Actual),
		}
		headers = append(headers, newHeader)
	}
	return headers
}
func convertToArrayBodyResult(m []models.BodyResult) []*model.BodyResult {
	var body *model.BodyResult
	var bodyResult []*model.BodyResult
	for _, v := range m {
		if v != (models.BodyResult{}) {
			body = &model.BodyResult{
				Expected: &v.Expected,
				Actual:   &v.Actual,
				Normal:   &v.Normal,
				//TODO: add Type
			}
			bodyResult = append(bodyResult, body)
		}
	}
	return bodyResult
}
func convertToArrayMockMetaResult(m []models.DepMetaResult) []*model.MockMetaResult {
	var dep []*model.MockMetaResult
	for _, v := range m {
		newDep := &model.MockMetaResult{
			Normal:   &v.Normal,
			Expected: (*string)(&v.Expected),
			Actual:   (*string)(&v.Actual),
			Key:      &v.Key,
		}
		dep = append(dep, newDep)
	}
	return dep
}
func convertToArrayDepResult(m []models.DepResult) []*model.MockResult {
	var dep []*model.MockResult
	for _, v := range m {
		newDep := &model.MockResult{
			Name: &v.Name,
			Type: (*model.MockType)(&v.Type),
			Meta: convertToArrayMockMetaResult(v.Meta),
		}
		dep = append(dep, newDep)
	}
	return dep
}
func convertModelsTests(m []models.TestResult) []*model.Test {
	var tests []*model.Test
	for _, v := range m {
		started := strconv.FormatInt(v.Started, 10)
		completed := strconv.FormatInt(v.Completed, 10)
		newTest := &model.Test{
			//Did not set Dep
			Status:    (*model.TestStatus)(&v.Status),
			Started:   &(started),
			Completed: &(completed),

			TestCaseID: &v.TestCaseID,
			URI:        &v.Req.URL,
			Req: &model.HTTPReq{
				Method:     (*model.Method)(&v.Req.Method),
				ProtoMajor: convertInttoPointer(v.Req.ProtoMajor),
				ProtoMinor: convertInttoPointer(v.Req.ProtoMinor),
				URL:        convertStringToPointer(v.Req.URL),
				URLParams:  convertToKV(v.Req.URLParams),
				Header:     convertToKV(v.Req.Header),
				Body:       convertStringToPointer(v.Req.Body),
				BodyType:   convertStringToPointer(v.Req.BodyType),
				Binary:     convertStringToPointer(v.Req.Binary),
				Form:       convertToFormData(v.Req.Form),
			},
			HTTPResp: &model.HTTPResp{
				ProtoMajor:    convertInttoPointer(v.Res.ProtoMajor),
				ProtoMinor:    convertInttoPointer(v.Res.ProtoMinor),
				StatusCode:    convertInttoPointer(v.Res.StatusCode),
				Header:        convertToKV(v.Res.Header),
				Body:          convertStringToPointer(v.Res.Body),
				BodyType:      convertStringToPointer(v.Res.BodyType),
				Binary:        convertStringToPointer(v.Res.Binary),
				StatusMessage: convertStringToPointer(v.Res.StatusMessage),
			},
			Noise: convertToArrayString(v.Noise),
			Result: &model.Result{
				StatusCode: &model.IntResult{
					Expected: convertInttoPointer(v.Result.StatusCode.Expected),
					Actual:   convertInttoPointer(v.Result.StatusCode.Actual),
					Normal:   convertBooltoPointer(v.Result.StatusCode.Normal),
				},
				HeadersResult: convertToArrayHeadersResult(v.Result.HeadersResult),
				BodyResult:    convertToArrayBodyResult(v.Result.BodyResult),
				MockResult:    convertToArrayDepResult(v.Result.DepResult),
			},
		}
		tests = append(tests, newTest)
	}
	return tests
}
func getHistCfg()[]fs.HistCfg {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Errorf(" Error getting home directory. Error: ", err)
		return nil
	}
	filepath := homeDir + "/.keploy-config/histCfg.yaml"
	existingData, err := os.ReadFile(filepath)
	if err != nil {
		fmt.Errorf("Error reading histCfg.yaml. Error: ", err)
		return nil
	}
	var histCfgList map[string][]fs.HistCfg
	err = yaml.Unmarshal(existingData, &histCfgList)
	if err != nil {
		fmt.Errorf("Error unmarshalling histCfg.yaml. Error: ", err)
		return nil
	}
	return histCfgList["histCfg"]
}
