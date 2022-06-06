package graph

import (
	"context"
	"net/http"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"go.keploy.io/server/graph/model"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/service/run"
)

const DEFAULT_COMPANY = "default_company"
const DEFAULT_USER = "default_user"

func ConvertTestRunStatus(s run.TestRunStatus) model.TestRunStatus {
	switch s {
	case run.TestRunStatusFailed:
		return model.TestRunStatusFailed
	case run.TestRunStatusRunning:
		return model.TestRunStatusRunning
	default:
		return model.TestRunStatusPassed
	}
}

func ConvertTestStatus(s run.TestStatus) model.TestStatus {
	switch s {
	case run.TestStatusFailed:
		return model.TestStatusFailed
	case run.TestStatusPassed:
		return model.TestStatusPassed
	case run.TestStatusPending:
		return model.TestStatusPending
	default:
		return model.TestStatusRunning
	}
}

func ConvertMethod(m models.Method) model.Method {
	switch m {
	case models.MethodGet:
		return model.MethodGet
	case models.MethodPost:
		return model.MethodPost
	case models.MethodPut:
		return model.MethodPut
	case models.MethodDelete:
		return model.MethodDelete
	case models.MethodHead:
		return model.MethodHead
	case models.MethodOptions:
		return model.MethodOptions
	case models.MethodTrace:
		return model.MethodTrace
	default:
		return model.MethodPatch
	}
}

func ConvertMapToKV(m map[string]string) []*model.Kv {
	var kv []*model.Kv
	for k, v := range m {
		kv = append(kv, &model.Kv{
			Key:   k,
			Value: v,
		})
	}
	return kv
}

func ConvertHttpReq(r models.HttpReq) *model.HTTPReq {
	params := ConvertMapToKV(r.URLParams)
	var header []*model.Header
	for k, v := range r.Header {
		header = append(header, &model.Header{
			Key:   k,
			Value: v,
		})
	}

	return &model.HTTPReq{
		ProtoMajor: r.ProtoMajor,
		ProtoMinor: r.ProtoMinor,
		URLParam:   params,
		Header:     header,
		Method:     ConvertMethod(r.Method),
		Body:       r.Body,
		URL:        &r.URL,
	}
}

func ConvertIntResult(i run.IntResult) *model.IntResult {
	return &model.IntResult{
		Normal:   &i.Normal,
		Expected: i.Expected,
		Actual:   i.Actual,
	}
}

func ConvertHeader(h run.Header) *model.Header {
	return &model.Header{
		Key:   h.Key,
		Value: h.Value,
	}
}

func ConvertHeaderResult(h run.HeaderResult) *model.HeaderResult {
	return &model.HeaderResult{
		Normal:   &h.Normal,
		Expected: ConvertHeader(h.Expected),
		Actual:   ConvertHeader(h.Actual),
	}
}

func ConvertBodyType(b run.BodyType) model.BodyType {
	switch b {
	case run.BodyTypeJSON:
		return model.BodyTypeJSON
	default:
		return model.BodyTypePlain
	}
}

func ConvertResult(r run.Result) *model.Result {
	var headers []*model.HeaderResult
	for _, h := range r.HeadersResult {
		headers = append(headers, ConvertHeaderResult(h))
	}
	return &model.Result{
		StatusCode:    ConvertIntResult(r.StatusCode),
		HeadersResult: headers,
		BodyResult: &model.BodyResult{
			Normal:   r.BodyResult.Normal,
			Type:     ConvertBodyType(r.BodyResult.Type),
			Expected: r.BodyResult.Expected,
			Actual:   r.BodyResult.Actual,
		},
		DepResult: nil,
	}
}

func ConvertHeaderInput(h []*model.HeaderInput) http.Header {
	headers := http.Header{}
	for _, v := range h {
		headers[v.Key] = v.Value
	}
	return headers
}

func ConvertTestCaseInput(input *model.TestCaseInput) models.TestCase {
	tc := models.TestCase{
		ID: input.ID,
		//Anchors:  anchors,
		Noise: input.Noise,
	}
	if input.Created != nil {
		tc.Created = input.Created.Unix()
	}
	if input.Updated != nil {
		tc.Updated = input.Updated.Unix()
	}
	if input.Captured != nil {
		tc.Captured = input.Captured.Unix()
	}
	if input.Cid != nil {
		tc.CID = *input.Cid
	}
	if input.App != nil {
		tc.AppID = *input.App
	}

	if input.URI != nil {
		tc.URI = *input.URI
	}

	if input.HTTPReq != nil {
		params := map[string]string{}
		for _, v := range input.HTTPReq.URLParam {
			params[v.Key] = v.Value
		}
		req := models.HttpReq{
			URLParams: params,
			Header:    ConvertHeaderInput(input.HTTPReq.Header),
		}
		if input.HTTPReq.Method != nil {
			req.Method = models.Method(*input.HTTPReq.Method)
		}

		if input.HTTPReq.ProtoMajor != nil {
			req.ProtoMajor = *input.HTTPReq.ProtoMajor
		}

		if input.HTTPReq.ProtoMinor != nil {
			req.ProtoMinor = *input.HTTPReq.ProtoMinor
		}

		if input.HTTPReq.Body != nil {
			req.Body = *input.HTTPReq.Body
		}
		if input.HTTPReq.URL != nil {
			req.Body = *input.HTTPReq.URL
		}
		tc.HttpReq = req
	}
	if input.HTTPResp != nil {
		resp := models.HttpResp{
			Header: ConvertHeaderInput(input.HTTPResp.Header),
		}
		if input.HTTPResp.StatusCode != nil {
			resp.StatusCode = *input.HTTPResp.StatusCode
		}

		if input.HTTPResp.Body != nil {
			resp.Body = *input.HTTPResp.Body
		}
		tc.HttpResp = resp
	}

	if input.Deps != nil {
		var deps []models.Dependency
		for _, v := range input.Deps {
			meta := map[string]string{}
			for _, m := range v.Meta {
				if m != nil {
					meta[m.Key] = m.Value
				}
			}
			deps = append(deps, models.Dependency{
				Name: v.Name,
				Type: models.DependencyType(v.Type),
				Meta: meta,
			})
		}
		tc.Deps = deps
	}

	return tc
}

func ConvertDeps(deps []models.Dependency) []*model.Dependency {
	var res []*model.Dependency
	for _, d := range deps {
		res = append(res, &model.Dependency{
			Name: d.Name,
			Type: model.DependencyType(d.Type),
			Meta: ConvertMapToKV(d.Meta),
		})
	}
	return res
}

func ConvertTestCase(t models.TestCase) *model.TestCase {
	var h []*model.Header
	for k, v := range t.HttpResp.Header {
		h = append(h, &model.Header{
			Key:   k,
			Value: v,
		})
	}

	var anchors []string
	for k := range t.Anchors {
		anchors = append(anchors, k)
	}

	return &model.TestCase{
		ID:       t.ID,
		Created:  time.Unix(t.Created, 0).UTC(),
		Updated:  time.Unix(t.Updated, 0).UTC(),
		Captured: time.Unix(t.Captured, 0).UTC(),
		Cid:      t.CID,
		App:      t.AppID,
		URI:      t.URI,
		HTTPReq:  ConvertHttpReq(t.HttpReq),
		HTTPResp: &model.HTTPResp{
			StatusCode: t.HttpResp.StatusCode,
			Header:     h,
			Body:       t.HttpResp.Body,
		},
		Deps:    ConvertDeps(t.Deps),
		Anchors: anchors,
		Noise:   t.Noise,
	}
}

func GetPreloads(ctx context.Context) []string {
	return GetNestedPreloads(
		graphql.GetOperationContext(ctx),
		graphql.CollectFieldsCtx(ctx, nil),
		"",
	)
}

func GetNestedPreloads(ctx *graphql.OperationContext, fields []graphql.CollectedField, prefix string) (preloads []string) {
	for _, column := range fields {
		prefixColumn := GetPreloadString(prefix, column.Name)
		preloads = append(preloads, prefixColumn)
		preloads = append(preloads, GetNestedPreloads(ctx, graphql.CollectFields(ctx, column.Selections, nil), prefixColumn)...)
	}
	return
}

func GetPreloadString(prefix, name string) string {
	if len(prefix) > 0 {
		return prefix + "." + name
	}
	return name
}
