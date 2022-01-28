package run

import (
	"context"
	"errors"
	"time"

	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
)

func New(rdb DB, log *zap.Logger) *Run {
	return &Run{
		rdb: rdb,
		log: log,
	}
}

type Run struct {
	rdb DB
	tdb models.TestCaseDB
	log *zap.Logger
}

func (r *Run) Normalize(ctx context.Context, cid, id string) error {
	t, err := r.rdb.ReadTest(ctx, id)
	if err != nil {
		r.log.Error("failed to fetch test from db", zap.String("cid", cid), zap.String("id", id), zap.Error(err))
		return errors.New("test not found")
	}
	tc, err := r.tdb.Get(ctx, cid, t.TestCaseID, nil, nil)
	if err != nil {
		r.log.Error("failed to fetch testcase from db", zap.String("cid", cid), zap.String("id", id), zap.Error(err))
		return errors.New("testcase not found")
	}
	// update the responses
	tc.HttpResp = t.Resp
	err = r.tdb.Upsert(ctx, tc)
	if err != nil {
		r.log.Error("failed to update testcase in db", zap.String("cid", cid), zap.String("id", id), zap.Error(err))
		return errors.New("could not update testcase")
	}
	return nil
}

func (r *Run) Get(ctx context.Context, summary bool, cid string, user, app, id *string, from, to *time.Time, offset *int, limit *int) ([]*TestRun, error) {
	res, err := r.rdb.Read(ctx, cid, user, app, id, from, to, offset, limit)
	if err != nil {
		r.log.Error("failed to read test runs from DB", zap.String("cid", cid), zap.Any("user", user), zap.Any("app", app), zap.Any("id", id), zap.Any("from", from), zap.Any("to", to), zap.Error(err))
		return nil, errors.New("failed getting test runs")
	}
	err = r.updateStatus(ctx, res)
	if err != nil {
		return nil, err
	}
	if summary {
		return res, nil
	}
	if len(res) == 0 {
		return res, nil
	}

	for _, v := range res {
		tests, err1 := r.rdb.ReadTests(ctx, v.ID)
		if err1 != nil {
			msg := "failed getting tests from DB"
			r.log.Error(msg, zap.String("cid", cid), zap.String("test run id", v.ID), zap.Error(err1))
			return nil, errors.New(msg)
		}
		v.Tests = tests
	}
	return res, nil
}

func (r *Run) updateStatus(ctx context.Context, trs []*TestRun) error {
	for _, tr := range trs {
		if tr.Status != TestRunStatusRunning {
			return nil
		}
		tests, err1 := r.rdb.ReadTests(ctx, tr.ID)
		if err1 != nil {
			msg := "failed getting tests from DB"
			r.log.Error(msg, zap.String("cid", tr.CID), zap.String("test run id", tr.ID), zap.Error(err1))
			return errors.New(msg)
		}
		if len(tests) == 0 {
			return nil
		}
		// find the newest test
		ts := tests[0].Started
		for _, test := range tests {
			if test.Started > ts {
				ts = test.Started
			}
		}

		// if the oldest test is older than 5 minutes then fail the whole test run
		diff := time.Now().Sub(time.Unix(ts, 0))
		if diff < 5*time.Minute {
			return nil
		}
		tr.Status = TestRunStatusFailed
		err2 := r.rdb.Upsert(ctx, *tr)
		if err2 != nil {
			msg := "failed validating and updating test run status"
			r.log.Error(msg, zap.String("cid", tr.CID), zap.String("test run id", tr.ID), zap.Error(err2))
			return errors.New(msg)
		}
	}
	return nil
}

func (r *Run) Put(ctx context.Context, run TestRun) error {
	return r.rdb.Upsert(ctx, run)
}
