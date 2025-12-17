package replay

import (
	"context"
	"sort"

	"go.keploy.io/server/v3/pkg/platform/sql/flakiness"
	"go.uber.org/zap"
)

type FlakinessTracker struct {
	logger *zap.Logger
	db     *flakiness.FlakinessDB
}

func NewFlakinessTracker(logger *zap.Logger, db *flakiness.FlakinessDB) *FlakinessTracker {
	return &FlakinessTracker{
		logger: logger,
		db:     db,
	}
}

func (ft *FlakinessTracker) RecordResult(ctx context.Context, testName string, passed bool) {
	err := ft.db.RecordResult(ctx, testName, passed)
	if err != nil {
		ft.logger.Warn("failed to record test flakiness result", zap.String("test", testName), zap.Error(err))
	}
}

func (ft *FlakinessTracker) GetFlakyTests(ctx context.Context, threshold float64) ([]*flakiness.TestHistory, error) {
	flaky, err := ft.db.GetFlakyTests(ctx, threshold)
	if err != nil {
		return nil, err
	}

	sort.Slice(flaky, func(i, j int) bool {
		return flaky[i].FlakinessRate > flaky[j].FlakinessRate
	})

	return flaky, nil
}
