package regression

import (
	"net/http"

	"github.com/go-chi/render"
	"go.keploy.io/server/pkg/models"
)

func ErrInvalidRequest(err error) render.Renderer {
	return &ErrResponse{
		Err:            err,
		HTTPStatusCode: 400,
		StatusText:     "Invalid request.",
		ErrorText:      err.Error(),
	}
}

func ReqTypeFilter(tcs []models.TestCase, reqType string) []models.TestCase {
	var result []models.TestCase
	for i := 0; i < len(tcs); i++ {
		if tcs[i].Type == reqType {
			result = append(result, tcs[i])
		}
	}
	return result
}

type ErrResponse struct {
	Err            error `json:"-"` // low-level runtime error
	HTTPStatusCode int   `json:"-"` // http response status code

	StatusText string `json:"status"`          // ddb-level status message
	AppCode    int64  `json:"code,omitempty"`  // application-specific error code
	ErrorText  string `json:"error,omitempty"` // application-level error message, for debugging
}

func (e *ErrResponse) Render(w http.ResponseWriter, r *http.Request) error {
	render.Status(r, e.HTTPStatusCode)
	return nil
}
