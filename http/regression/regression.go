package regression

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi"
	"github.com/go-chi/render"
	"github.com/google/uuid"

	"go.keploy.io/server/graph"
	"go.keploy.io/server/grpc/mock"
	"go.keploy.io/server/pkg/models"
	regression2 "go.keploy.io/server/pkg/service/regression"
	"go.keploy.io/server/pkg/service/run"
	tcSvc "go.keploy.io/server/pkg/service/testCase"
	"go.uber.org/zap"
)

func New(r chi.Router, logger *zap.Logger, svc regression2.Service, run run.Service, tc tcSvc.Service, testExport bool, testReportPath string) {
	s := &regression{logger: logger, svc: svc, run: run, testExport: testExport, testReportPath: testReportPath, tcSvc: tc}

	r.Route("/regression", func(r chi.Router) {
		r.Route("/testcase", func(r chi.Router) {
			r.Get("/{id}", s.GetTC)
			r.Get("/", s.GetTCS)
			r.Post("/", s.PostTC)
		})
		r.Post("/test", s.Test)
		r.Post("/denoise", s.DeNoise)
		r.Get("/start", s.Start)
		r.Get("/end", s.End)

		//r.Get("/search", searchArticles)                                  // GET /articles/search
	})
}

type regression struct {
	testExport     bool
	testReportPath string
	logger         *zap.Logger
	svc            regression2.Service
	tcSvc          tcSvc.Service
	run            run.Service
}

func (rg *regression) End(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	status := models.TestRunStatus(r.URL.Query().Get("status"))
	stat := models.TestRunStatusFailed
	if status == "true" {
		stat = models.TestRunStatusPassed
	}

	var (
		err error
		now = time.Now().Unix()
	)

	if rg.testExport {
		err := rg.svc.StopTestRun(r.Context(), id, rg.testReportPath)
		if err != nil {
			render.Render(w, r, ErrInvalidRequest(err))
			return
		}
	}
	err = rg.run.Put(r.Context(), run.TestRun{
		ID:      id,
		Updated: now,
		Status:  stat,
	}, rg.testExport, rg.testReportPath)

	if err != nil {
		render.Render(w, r, ErrInvalidRequest(err))
		return

	}

	render.Status(r, http.StatusOK)

}

func (rg *regression) Start(w http.ResponseWriter, r *http.Request) {
	t := r.URL.Query().Get("total")
	testCasePath := r.URL.Query().Get("testCasePath")
	mockPath := r.URL.Query().Get("mockPath")
	total, err := strconv.Atoi(t)
	if err != nil {
		render.Render(w, r, ErrInvalidRequest(err))
		return
	}
	app := rg.getMeta(w, r, true)
	if app == "" {
		return
	}

	id := uuid.New().String()
	now := time.Now().Unix()

	// user := "default"
	if rg.testExport {
		err = rg.svc.StartTestRun(r.Context(), id, testCasePath, mockPath, rg.testReportPath)
		if err != nil {
			render.Render(w, r, ErrInvalidRequest(err))
			return
		}
	}
	err = rg.run.Put(r.Context(), run.TestRun{
		ID:      id,
		Created: now,
		Updated: now,
		Status:  models.TestRunStatusRunning,
		CID:     graph.DEFAULT_COMPANY,
		App:     app,
		User:    graph.DEFAULT_USER,
		Total:   total,
	}, rg.testExport, rg.testReportPath)
	if err != nil {
		render.Render(w, r, ErrInvalidRequest(err))
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, map[string]string{
		"id": id,
	})

}

func (rg *regression) GetTC(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	app := rg.getMeta(w, r, false)
	tcs, err := rg.tcSvc.Get(r.Context(), graph.DEFAULT_COMPANY, app, id)
	if err != nil {
		render.Render(w, r, ErrInvalidRequest(err))
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, tcs)

}

func (rg *regression) getMeta(w http.ResponseWriter, r *http.Request, appRequired bool) string {
	app := r.URL.Query().Get("app")
	if app == "" && appRequired {
		rg.logger.Error("request for fetching testcases should include app id")
		render.Render(w, r, ErrInvalidRequest(errors.New("missing app id")))
		return ""
	}
	return app
}

func (rg *regression) GetTCS(w http.ResponseWriter, r *http.Request) {
	app := rg.getMeta(w, r, true)
	if app == "" {
		return
	}
	testCasePath := r.URL.Query().Get("testCasePath")
	mockPath := r.URL.Query().Get("mockPath")
	offsetStr := r.URL.Query().Get("offset")
	limitStr := r.URL.Query().Get("limit")
	reqType := r.URL.Query().Get("reqType")
	var (
		offset int
		limit  int
		err    error
		tcs    []models.TestCase
		eof    bool = rg.testExport
		ctx    context.Context
	)
	if offsetStr != "" {
		offset, err = strconv.Atoi(offsetStr)
		if err != nil {
			rg.logger.Error("request for fetching testcases in converting offset to integer")
		}
	}
	if limitStr != "" {
		limit, err = strconv.Atoi(limitStr)
		if err != nil {
			rg.logger.Error("request for fetching testcases in converting limit to integer")
		}
	}

	// switch rg.testExport {
	// case false:
	ctx = r.Context()
	ctx = context.WithValue(ctx, "reqType", reqType)
	tcs, err = rg.tcSvc.GetAll(ctx, graph.DEFAULT_COMPANY, app, &offset, &limit, testCasePath, mockPath)
	if rg.testExport && testCasePath != "" && mockPath != "" {
		filteredTcs := ReqTypeFilter(tcs, reqType)
		tcs = filteredTcs
	}

	if err != nil {
		render.Render(w, r, ErrInvalidRequest(err))
		return
	}
	// case true:
	// 	tcs, err = rg.tcSvc.ReadTCS(r.Context(), testCasePath, mockPath)
	// 	if err != nil {
	// 		render.Render(w, r, ErrInvalidRequest(err))
	// 		return
	// 	}
	// 	eof = true
	// }
	render.Status(r, http.StatusOK)
	// In test-export, eof is true to stop the infinite for loop in sdk
	w.Header().Set("EOF", fmt.Sprintf("%v", eof))
	render.JSON(w, r, tcs)

}

func (rg *regression) PostTC(w http.ResponseWriter, r *http.Request) {
	data := &TestCaseReq{}
	var (
		inserted []string
		err      error
	)
	if err := render.Bind(r, data); err != nil {
		rg.logger.Error("error parsing request", zap.Error(err))
		render.Render(w, r, ErrInvalidRequest(err))
		return
	}

	now := time.Now().UTC().Unix()
	var (
		id = uuid.New().String()
		tc = []models.Mock{{
			Version: string(models.V1_BETA1),
			Kind:    string(models.HTTP_EXPORT),
			Name:    id,
		}}
		mocks = []string{}
	)
	for i, j := range data.Mocks {
		doc, err := mock.Encode(j)
		if err != nil {
			rg.logger.Error(err.Error())
		}
		tc = append(tc, doc)
		m := id + "-" + strconv.Itoa(i)
		tc[len(tc)-1].Name = m
		mocks = append(mocks, m)
	}

	switch data.Type {
	case "http":
		if rg.testExport {
			tc[0].Spec.Encode(&models.HttpSpec{
				// Metadata: , TODO: What should be here
				Request: models.MockHttpReq{
					Method:     models.Method(data.HttpReq.Method),
					ProtoMajor: int(data.HttpReq.ProtoMajor),
					ProtoMinor: int(data.HttpReq.ProtoMinor),
					URL:        data.HttpReq.URL,
					URLParams:  data.HttpReq.URLParams,
					Body:       data.HttpReq.Body,
					Header:     mock.ToMockHeader(data.HttpReq.Header),
				},
				Response: models.MockHttpResp{
					StatusCode:    int(data.HttpResp.StatusCode),
					Body:          data.HttpResp.Body,
					Header:        mock.ToMockHeader(data.HttpResp.Header),
					StatusMessage: data.HttpResp.StatusMessage,
					ProtoMajor:    int(data.HttpReq.ProtoMajor),
					ProtoMinor:    int(data.HttpReq.ProtoMinor),
				},
				Objects: []models.Object{{
					Type: "error",
					Data: "",
				}},
				Mocks: mocks,
				Assertions: map[string][]string{
					"noise": {},
				},
				Created: data.Captured,
			})
		} else {
			inserted, err = rg.tcSvc.InsertToDB(r.Context(), graph.DEFAULT_COMPANY, []models.TestCase{{
				ID:       uuid.New().String(),
				Created:  now,
				Updated:  now,
				Captured: data.Captured,
				URI:      data.URI,
				AppID:    data.AppID,
				HttpReq:  data.HttpReq,
				HttpResp: data.HttpResp,
				Deps:     data.Deps,
				Type:     data.Type,
			}})
		}
	case "grpc":
		if rg.testExport {
			tc[0].Kind = string(models.GRPC_EXPORT)
			tc[0].Spec.Encode(&models.GrpcSpec{
				// Metadata: , TODO: What should be here
				Request: models.MockGrpcReq{
					Method: data.GrpcMethod,
					Body:   data.GrpcReq,
				},
				Response: data.GrpcResp,
				Objects: []models.Object{{
					Type: "error",
					Data: "",
				}},
				Mocks: mocks,
				Assertions: map[string][]string{
					"noise": {},
				},
				Created: data.Captured,
			})
		} else {
			inserted, err = rg.tcSvc.InsertToDB(r.Context(), graph.DEFAULT_COMPANY, []models.TestCase{{
				ID:         uuid.New().String(),
				Created:    now,
				Updated:    now,
				Captured:   data.Captured,
				GrpcMethod: data.GrpcMethod,
				AppID:      data.AppID,
				GrpcReq:    data.GrpcReq,
				GrpcResp:   data.GrpcResp,
				Deps:       data.Deps,
				Type:       data.Type,
			}})
		}
	}

	if rg.testExport {
		inserted, err := rg.tcSvc.WriteToYaml(r.Context(), tc, data.TestCasePath, data.MockPath)
		if err != nil {
			rg.logger.Error("error writing testcase to yaml file", zap.Error(err))
			render.Render(w, r, ErrInvalidRequest(err))
			return
		}
		render.Status(r, http.StatusOK)
		render.JSON(w, r, map[string]string{"id": inserted[0]})
		return
	}

	if err != nil {
		rg.logger.Error("error putting testcase", zap.Error(err))
		render.Render(w, r, ErrInvalidRequest(err))
		return

	}

	if len(inserted) == 0 {
		rg.logger.Error("unknown failure while inserting testcase")
		render.Render(w, r, ErrInvalidRequest(err))
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, map[string]string{"id": inserted[0]})

}

func (rg *regression) DeNoise(w http.ResponseWriter, r *http.Request) {
	// key := r.Header.Get("key")
	// if key == "" {
	// 	rg.logger.Error("missing api key")
	// 	render.Render(w, r, ErrInvalidRequest(errors.New("missing api key")))
	// 	return
	// }

	data := &TestReq{}
	var (
		err  error
		body string
		ctx  context.Context
	)
	if err = render.Bind(r, data); err != nil {
		rg.logger.Error("error parsing request", zap.Error(err))
		render.Render(w, r, ErrInvalidRequest(err))
		return
	}
	ctx = r.Context()
	ctx = context.WithValue(ctx, "reqType", data.Type)
	switch data.Type {
	case "http":
		body = data.Resp.Body

	case "grpc":
		body = data.GrpcResp
	}

	err = rg.svc.DeNoise(ctx, graph.DEFAULT_COMPANY, data.ID, data.AppID, body, data.Resp.Header, data.TestCasePath)
	if err != nil {
		rg.logger.Error("error putting testcase", zap.Error(err))
		render.Render(w, r, ErrInvalidRequest(err))
		return
	}

	render.Status(r, http.StatusOK)

}

func (rg *regression) Test(w http.ResponseWriter, r *http.Request) {

	data := &TestReq{}
	var (
		pass bool
		err  error
		ctx  context.Context
	)
	if err = render.Bind(r, data); err != nil {
		rg.logger.Error("error parsing request", zap.Error(err))
		render.Render(w, r, ErrInvalidRequest(err))
		return
	}
	ctx = r.Context()
	ctx = context.WithValue(ctx, "reqType", data.Type)
	switch data.Type {
	case "http":
		pass, err = rg.svc.Test(ctx, graph.DEFAULT_COMPANY, data.AppID, data.RunID, data.ID, data.TestCasePath, data.MockPath, data.Resp)

	case "grpc":
		pass, err = rg.svc.TestGrpc(ctx, graph.DEFAULT_COMPANY, data.AppID, data.RunID, data.ID, data.GrpcResp, data.TestCasePath, data.MockPath)
	}

	if err != nil {
		rg.logger.Error("error putting testcase", zap.Error(err))
		render.Render(w, r, ErrInvalidRequest(err))
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, map[string]bool{"pass": pass})

}