package regression

import (
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
	"go.uber.org/zap"
)

func New(r chi.Router, logger *zap.Logger, svc regression2.Service, run run.Service, testExport bool) {
	s := &regression{logger: logger, svc: svc, run: run, testExport: testExport}

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
	testExport bool
	logger     *zap.Logger
	svc        regression2.Service
	run        run.Service
}

func (rg *regression) End(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	status := run.TestRunStatus(r.URL.Query().Get("status"))
	stat := run.TestRunStatusFailed
	if status == "true" {
		stat = run.TestRunStatusPassed
	}

	now := time.Now().Unix()

	if rg.testExport {
		rg.svc.StopTestRun(r.Context(), id)
	}
	err := rg.run.Put(r.Context(), run.TestRun{
		ID:      id,
		Updated: now,
		Status:  stat,
	})
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
		rg.svc.StartTestRun(r.Context(), id, testCasePath, mockPath)
	}
	err = rg.run.Put(r.Context(), run.TestRun{
		ID:      id,
		Created: now,
		Updated: now,
		Status:  run.TestRunStatusRunning,
		CID:     graph.DEFAULT_COMPANY,
		App:     app,
		User:    graph.DEFAULT_USER,
		Total:   total,
	})
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
	tcs, err := rg.svc.Get(r.Context(), graph.DEFAULT_COMPANY, app, id)
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
	var (
		offset int
		limit  int
		err    error
		tcs    []models.TestCase
		eof    bool
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

	switch rg.testExport {
	case false:
		tcs, err = rg.svc.GetAll(r.Context(), graph.DEFAULT_COMPANY, app, &offset, &limit)
		if err != nil {
			render.Render(w, r, ErrInvalidRequest(err))
			return
		}
	case true:
		tcs, err = rg.svc.ReadTCS(r.Context(), testCasePath, mockPath)
		if err != nil {
			render.Render(w, r, ErrInvalidRequest(err))
			return
		}
		eof = true
	}
	render.Status(r, http.StatusOK)
	w.Header().Set("EOF", fmt.Sprintf("%v", eof))
	render.JSON(w, r, tcs)

}

func (rg *regression) PostTC(w http.ResponseWriter, r *http.Request) {

	data := &TestCaseReq{}
	if err := render.Bind(r, data); err != nil {
		rg.logger.Error("error parsing request", zap.Error(err))
		render.Render(w, r, ErrInvalidRequest(err))
		return
	}

	now := time.Now().UTC().Unix()
	if rg.testExport {
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
		tc[0].Spec.Encode(&models.HttpSpec{
			// Metadata: , TODO: What should be here
			Request: models.HttpReq{
				Method:     models.Method(data.HttpReq.Method),
				ProtoMajor: int(data.HttpReq.ProtoMajor),
				ProtoMinor: int(data.HttpReq.ProtoMinor),
				URL:        data.HttpReq.URL,
				URLParams:  data.HttpReq.URLParams,
				Body:       data.HttpReq.Body,
				Header:     data.HttpReq.Header,
			},
			Response: models.HttpResp{
				StatusCode: int(data.HttpResp.StatusCode),
				Body:       data.HttpResp.Body,
				Header:     data.HttpResp.Header,
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
		inserted, err := rg.svc.WriteTC(r.Context(), tc, data.TestCasePath, data.MockPath)
		if err != nil {
			rg.logger.Error("error writing testcase to yaml file", zap.Error(err))
			render.Render(w, r, ErrInvalidRequest(err))
			return
		}
		render.Status(r, http.StatusOK)
		render.JSON(w, r, map[string]string{"id": inserted[0]})
		return
	}
	inserted, err := rg.svc.Put(r.Context(), graph.DEFAULT_COMPANY, []models.TestCase{{
		ID:       uuid.New().String(),
		Created:  now,
		Updated:  now,
		Captured: data.Captured,
		URI:      data.URI,
		AppID:    data.AppID,
		HttpReq:  data.HttpReq,
		HttpResp: data.HttpResp,
		Deps:     data.Deps,
	}})
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
	if err := render.Bind(r, data); err != nil {
		rg.logger.Error("error parsing request", zap.Error(err))
		render.Render(w, r, ErrInvalidRequest(err))
		return
	}

	err := rg.svc.DeNoise(r.Context(), graph.DEFAULT_COMPANY, data.ID, data.AppID, data.Resp.Body, data.Resp.Header, data.TestCasePath)
	if err != nil {
		rg.logger.Error("error putting testcase", zap.Error(err))
		render.Render(w, r, ErrInvalidRequest(err))
		return
	}

	render.Status(r, http.StatusOK)

}

func (rg *regression) Test(w http.ResponseWriter, r *http.Request) {

	data := &TestReq{}
	if err := render.Bind(r, data); err != nil {
		rg.logger.Error("error parsing request", zap.Error(err))
		render.Render(w, r, ErrInvalidRequest(err))
		return
	}

	pass, err := rg.svc.Test(r.Context(), graph.DEFAULT_COMPANY, data.AppID, data.RunID, data.ID, data.TestCasePath, data.MockPath, data.Resp)

	if err != nil {
		rg.logger.Error("error putting testcase", zap.Error(err))
		render.Render(w, r, ErrInvalidRequest(err))
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, map[string]bool{"pass": pass})

}
