package regression

import (
	"errors"
	// "fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi"
	"github.com/go-chi/render"
	"github.com/google/uuid"
	"go.keploy.io/server/graph"
	"go.keploy.io/server/pkg/models"
	regression2 "go.keploy.io/server/pkg/service/regression"
	"go.keploy.io/server/pkg/service/run"
	"go.uber.org/zap"
)

func New(r chi.Router, logger *zap.Logger, svc regression2.Service, run run.Service) {
	s := &regression{logger: logger, svc: svc, run: run}

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
	logger *zap.Logger
	svc    regression2.Service
	run    run.Service
}

func (rg *regression) End(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	status := run.TestRunStatus(r.URL.Query().Get("status"))
	stat := run.TestRunStatusFailed
	if status == "true" {
		stat = run.TestRunStatusPassed
	}

	now := time.Now().Unix()

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
	offsetStr := r.URL.Query().Get("offset")
	limitStr := r.URL.Query().Get("limit")
	var (
		offset int
		limit  int
		err    error
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
	tcs, err := rg.svc.GetAll(r.Context(), graph.DEFAULT_COMPANY, app, &offset, &limit)
	if err != nil {
		render.Render(w, r, ErrInvalidRequest(err))
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, tcs)

}

func (rg *regression) PostTC(w http.ResponseWriter, r *http.Request) {
	// key := r.Header.Get("key")
	// if key == "" {
	// 	rg.logger.Error("missing api key")
	// 	render.Render(w, r, ErrInvalidRequest(errors.New("missing api key")))
	// 	return
	// }
	data := &TestCaseReq{}
	if err := render.Bind(r, data); err != nil {
		rg.logger.Error("error parsing request", zap.Error(err))
		render.Render(w, r, ErrInvalidRequest(err))
		return
	}

	// rg.logger.Debug("testcase posted",zap.Any("testcase request",data))

	now := time.Now().UTC().Unix()
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

	// rg.logger.Debug("testcase inserted",zap.Any("testcase ids",inserted))
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

	err := rg.svc.DeNoise(r.Context(), graph.DEFAULT_COMPANY, data.ID, data.AppID, data.Resp.Body, data.Resp.Header)
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

	pass, err := rg.svc.Test(r.Context(), graph.DEFAULT_COMPANY, data.AppID, data.RunID, data.ID, data.Resp)

	if err != nil {
		rg.logger.Error("error putting testcase", zap.Error(err))
		render.Render(w, r, ErrInvalidRequest(err))
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, map[string]bool{"pass": pass})

}
