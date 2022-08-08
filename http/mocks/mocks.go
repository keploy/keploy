package mocks

import (
	"net/http"

	"github.com/go-chi/chi"
	"github.com/go-chi/render"
	"go.keploy.io/server/http/regression"
	"go.keploy.io/server/pkg/models"
	mocks2 "go.keploy.io/server/pkg/service/mocks"
	"go.uber.org/zap"
)

func New(r chi.Router, logger *zap.Logger, svc mocks2.Service) {
	s := &mocks{
		logger: logger,
		svc:    svc,
	}

	r.Route("/deps", func(r chi.Router) {
		r.Get("/", s.Get)
		r.Post("/", s.Post)
	})
}

type mocks struct {
	logger *zap.Logger
	svc    mocks2.Service
}

func (m *mocks) Get(w http.ResponseWriter, r *http.Request) {
	app := r.URL.Query().Get("appid")
	testName := r.URL.Query().Get("testName")
	res, err := m.svc.Get(r.Context(), app, testName)
	if err != nil {
		render.Render(w, r, regression.ErrInvalidRequest(err))
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, res)
}

func (m *mocks) Post(w http.ResponseWriter, r *http.Request) {
	data := &TestMocksReq{}
	if err := render.Bind(r, data); err != nil {
		m.logger.Error("error parsing request", zap.Error(err))
		render.Render(w, r, regression.ErrInvalidRequest(err))
		return
	}

	err := m.svc.Insert(r.Context(), models.TestMock(*data))
	if err != nil {
		render.Render(w, r, regression.ErrInvalidRequest(err))
	}
	return
	// render.Status(r, http.StatusOK)
	// render.JSON(w, r, "Inserted succesfully")
}
