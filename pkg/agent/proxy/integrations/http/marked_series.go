package http

import (
	"context"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
)

// reportMarkedSeries reports a call of a series judging marked answered with
// recording x (integrations.AnswerSeries): of the set's background
// (models.AnswerBackgroundSeries), or, when owner names the test whose own
// series it is, of that test's own series (models.AnswerOwnSeries). It
// counts against no test; the replay lists it under the answered calls of
// the test whose window it came in (the owner's, for its own series),
// naming x.
func reportMarkedSeries(ctx context.Context, input *req, x *models.Mock, owner string) {
	r := answerReport(input)
	r.Answer, r.ClosestMock = models.AnswerBackgroundSeries, x.Name
	if owner != "" {
		r.Answer = models.AnswerOwnSeries
	}
	r.ClosestMockReq = renderMockRequest(x.Spec.HTTPReq, x.Noise)
	models.ReportAnswer(ctx, r)
}

// seriesCall is the call input as a recording holds its request, for a
// series reading of it (integrations.AnswerSeries): its method, URL as it
// came, headers (Host among them) and body, read as the body was read.
func seriesCall(input *req) *models.Mock {
	r := &models.HTTPReq{Method: models.Method(input.method), Header: pkg.ToYamlHTTPHeader(input.header), Body: string(input.body)}
	if input.url != nil {
		r.URL = input.url.String()
	}
	return &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: r}}
}
