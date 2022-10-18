package graph

// This file will be automatically regenerated based on the schema, any resolver implementations
// will be copied through when generating and any unknown code will be moved to the end.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.keploy.io/server/graph/generated"
	"go.keploy.io/server/graph/model"
	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/models"
)

func (r *mutationResolver) UpdateTestCase(ctx context.Context, tc []*model.TestCaseInput) (bool, error) {
	var tcs []models.TestCase
	for _, t := range tc {
		tcs = append(tcs, ConvertTestCaseInput(t))
	}
	err := r.tcSvc.UpdateTC(ctx, tcs)
	if err != nil {
		return false, err
	}
	return true, nil
}

func (r *mutationResolver) DeleteTestCase(ctx context.Context, id string) (bool, error) {
	err := r.tcSvc.DeleteTC(ctx, DEFAULT_COMPANY, id)
	if err != nil {
		return false, err
	}
	return true, nil
}

func (r *mutationResolver) NormalizeTests(ctx context.Context, ids []string) (bool, error) {
	var errStrings []string
	for _, id := range ids {
		err := r.run.Normalize(ctx, DEFAULT_COMPANY, id)
		if err != nil {
			errStrings = append(errStrings, id+": "+err.Error())
		}
	}
	if len(errStrings) != 0 {
		return false, fmt.Errorf(strings.Join(errStrings, "\n"))
	}
	return true, nil
}

func (r *queryResolver) Apps(ctx context.Context) ([]*model.App, error) {
	apps, err := r.tcSvc.GetApps(ctx, DEFAULT_COMPANY)
	if err != nil {
		return nil, err
	}
	var res []*model.App
	for _, v := range apps {
		res = append(res, &model.App{ID: v})
	}

	return res, nil
}

func (r *queryResolver) TestRun(ctx context.Context, user *string, app *string, id *string, from *time.Time, to *time.Time, offset *int, limit *int) ([]*model.TestRun, error) {
	preloads := GetPreloads(ctx)
	summary := true
	if pkg.Contains(preloads, "tests") {
		summary = false
	}

	usr := DEFAULT_USER

	runs, err := r.run.Get(ctx, summary, DEFAULT_COMPANY, &usr, app, id, from, to, offset, limit)
	if err != nil {
		return nil, err
	}

	var res []*model.TestRun
	for _, run := range runs {
		var tests []*model.Test
		if run.Tests != nil {
			for _, t := range run.Tests {
				uri := t.URI
				completed := time.Unix(t.Completed, 0).UTC()
				tests = append(tests, &model.Test{
					ID:         t.ID,
					Status:     ConvertTestStatus(t.Status),
					Started:    time.Unix(t.Started, 0).UTC(),
					Completed:  &completed,
					TestCaseID: t.TestCaseID,
					URI:        &uri,
					Req:        ConvertHttpReq(t.Req),
					Noise:      t.Noise,
					Deps:       ConvertDeps(t.Dep),
					Result:     ConvertResult(t.Result),
				})
			}
		}

		ts := &model.TestRun{
			ID:      run.ID,
			Status:  ConvertTestRunStatus(run.Status),
			Created: time.Unix(run.Created, 0).UTC(),
			Updated: time.Unix(run.Updated, 0).UTC(),
			App:     run.App,
			User:    run.User,
			Success: run.Success,
			Failure: run.Failure,
			Total:   run.Total,
			Tests:   tests,
		}
		//if run.Updated != nil {
		//	ts.Updated = time.Unix(*run.Updated, 0)
		//}
		//if run.Created != nil {
		//	ts.Created = time.Unix(*run.Created, 0)
		//}
		//if run.App != nil {
		//	ts.App = *run.App
		//}
		//if run.User != nil {
		//	ts.User = *run.User
		//}
		//if run.Success != nil {
		//	ts.Success = *run.Success
		//}
		//if run.Failure != nil {
		//	ts.Failure = *run.Failure
		//}
		//if run.Total != nil {
		//	ts.Total = *run.Total
		//}

		res = append(res, ts)

	}

	return res, nil
}

func (r *queryResolver) TestCase(ctx context.Context, app *string, id *string, offset *int, limit *int) ([]*model.TestCase, error) {
	a := ""
	if app != nil {
		a = *app
	}

	if id != nil {
		tc, err := r.tcSvc.Get(ctx, DEFAULT_COMPANY, a, *id)
		if err != nil {
			return nil, err
		}
		return []*model.TestCase{ConvertTestCase(tc)}, nil
	}

	tcs, err := r.tcSvc.GetAll(ctx, DEFAULT_COMPANY, a, offset, limit)
	if err != nil {
		return nil, err
	}
	var res []*model.TestCase
	for _, v := range tcs {
		res = append(res, ConvertTestCase(v))
	}
	return res, nil
}

func (r *subscriptionResolver) TestRun(ctx context.Context, app *string, id *string) (<-chan []*model.TestRun, error) {
	panic(fmt.Errorf("not implemented"))
}

// Subscription returns generated.SubscriptionResolver implementation.
func (r *Resolver) Subscription() generated.SubscriptionResolver { return &subscriptionResolver{r} }

type subscriptionResolver struct{ *Resolver }
